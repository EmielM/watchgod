package main

import (
	"container/list"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	listenF = flag.String("listen", "data/god.socket", "Address to listen at. [interface]:[port] for TCP or a Unix domain socket path.")
	dirF    = flag.String("dir", "god/", "Where to look for binaries that support god")

	connC = make(chan net.Conn)
	commC = make(chan Comm)
	logC  = make(chan string)

	loaders map[string]*Loader
	jobs    map[string]*Job

	sessions = list.New()

	waitMap = make(map[string][]chan bool)
	waitMu  sync.Mutex
)

// Command received from a client

type Session struct {
	Conn   net.Conn
	Filter func(string) string
}

type Comm struct {
	cmdArgs []string
	conn    net.Conn
}

func main() {
	flag.Parse()

	err := listen(*listenF)
	if err != nil {
		log.Fatal(err)
	}

	go fanoutLog()

	signalC := make(chan os.Signal, 1)
	signal.Notify(signalC, syscall.SIGHUP, syscall.SIGINT,
		syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2)

	jobs = make(map[string]*Job)

	loadLoaders()

	run := true
	for run {
		select {
		case s := <-signalC:
			logC <- "broadcast signal " + s.String() + " to jobs"
			for _, j := range jobs {
				j.Signal(s)
			}

			if s == syscall.SIGTERM || s == syscall.SIGINT {
				run = false
			}

		case c := <-commC:
			switch c.cmdArgs[0] {
			case "newLoaders":
				loadLoaders()

			case "jobs":
				if c.conn != nil {
					for _, job := range jobs {
						c.conn.Write([]byte(job.Name + ": " + job.State + "\n"))
					}
				}

			case "tail":
				if c.conn != nil {
					c.conn.Write([]byte("TODO\n"))
				}

			default:
				if job, ok := jobs[c.cmdArgs[0]]; ok {
					job.commC <- c.cmdArgs[1:]
				} else if c.conn != nil {
					c.conn.Write([]byte("no such job: " + c.cmdArgs[0] + "\n"))
				}

			}
		}
	}

	time.Sleep(1 * time.Second)

	for _, j := range jobs {
		log.Print("wait ", j.Name)
		// todo: wait till all processes are stopped somehow
	}

	close(logC)
}

// Fan-out messages on logC to all sessions; signal new connections on connC, old connections are closed lazily.
func fanoutLog() {
	for {
		select {
		case c := <-connC:
			sessions.PushBack(&Session{Conn: c})

		case line, ok := <-logC:
			if !ok {
				return
			}

			b := []byte(time.Now().Format("15:04:05.000") + " " + line + "\n")
			os.Stdout.Write(b)
			for e := sessions.Front(); e != nil; e = e.Next() {
				s := e.Value.(Session)
				if s.Filter != nil {
					s.Filter(line)
				}

				_, err := s.Conn.Write(b)
				if err != nil {
					//log.Print("cleanup conn ", i)
					s.Conn.Close()
					sessions.Remove(e)
				}
			}
		}
	}
}

// Scans for loader files and instantiates Loaders. Old loaders are not stopped when still in use.
func loadLoaders() {
	files, _ := filepath.Glob(*dirF + "/*")

	oldLoaders := loaders

	loaders = make(map[string]*Loader)

	for _, f := range files {
		l, err := NewLoader(f)
		if err != nil {
			logC <- filepath.Base(f) + " error " + err.Error()
			continue
		}
		loaders[f] = l
	}

	used := make(map[*Loader]bool)
	for _, job := range jobs {
		used[job.L] = true
	}

	for _, loader := range oldLoaders {
		if _, ok := used[loader]; !ok {
			loader.Stop()
		}
		// When still in use, we keep loaders lingering until the next loadLoaders() call.
	}
}

func listen(addr string) error {
	typ := "tcp"
	if strings.IndexByte(addr, ':') < 0 {
		typ = "unix"
		os.Remove(addr)
	}

	listen, err := net.Listen(typ, addr)
	if err != nil {
		return err
	}

	log.Print("listening at ", addr)

	go func() {
		conn, err := listen.Accept()
		if err != nil {
			log.Print(err)
			return
		}

		go func(conn net.Conn) {
			connC <- conn
			r := json.NewDecoder(conn)
			for {
				var cmdArgs []string
				err := r.Decode(&cmdArgs)
				if err != nil {
					break
				}
				commC <- Comm{cmdArgs, conn}
			}
			conn.Close()
		}(conn)
	}()

	return nil
}

// A Loader is a process that defines one or more jobs to run. Loaders communicate with god over stdin/out, and can be implemented in any language. They should not have (much) state because they might get reloaded.
type Loader struct {
	cmd  *exec.Cmd
	name string
	file string
	w    *json.Encoder
	r    *json.Decoder
	mu   sync.Mutex
}

func NewLoader(file string) (*Loader, error) {
	if file[0] != '/' {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		file = wd + "/" + file
	}

	l := &Loader{file: file, name: filepath.Base(file) + "L"}

	err := l.Start()
	return l, err
}

func (l *Loader) Start() error {

	l.cmd = exec.Command(l.file, "godv1")
	l.cmd.Stderr = &lineWriter{C: logC, Prefix: l.name + " "}

	w, _ := l.cmd.StdinPipe()
	r, _ := l.cmd.StdoutPipe()

	l.w = json.NewEncoder(w)
	l.r = json.NewDecoder(r)

	err := l.cmd.Start()
	if err != nil {
		return err
	}

	ok := false

	// give loader 500ms to reply with init
	go func() {
		time.Sleep(500 * time.Millisecond)
		if !ok {
			logC <- l.name + " no init in 500ms; does it support god?"
			r.Close()
		}
	}()

	var init []string
	err = l.r.Decode(&init)

	if err != nil || len(init) == 0 || init[0] != "godv1" {
		l.Stop()
		return errors.New("protocol error, does this binary support god?")
	}

	ok = true

	m := []string{}
	for _, name := range init[1:] {
		if job, ok := jobs[name]; ok {
			job.loaderC <- l
			m = append(m, "attach to job "+name)
		} else {
			jobs[name] = NewJob(l, name)
			m = append(m, "new job "+name)
		}
	}

	logC <- l.name + " loaded: " + strings.Join(m, ", ")

	return nil
}

func (l *Loader) Call(args ...string) []string {
	var r []string
	l.mu.Lock()
	//log.Print(l.name, " > ", args)
	err := l.w.Encode(args)
	if err == nil {
		err = l.r.Decode(&r)
		//log.Print(l.name, " < ", r)
	}
	if err != nil {
		logC <- l.name + " " + err.Error()
		l.Stop()
		//l.Start()
	}
	l.mu.Unlock()
	return r
}

func (l *Loader) Stop() {
	l.cmd.Process.Signal(os.Kill)
	l.cmd.Wait()

	// TODO: close pipes here? We don't have the references anymore.

	logC <- l.name + " loader stopped"
}

type Job struct {
	Name  string
	Exec  string
	L     *Loader
	Cmd   *exec.Cmd
	State string // new|wait|start|run|degrade|exit|kill|stop

	logC    chan string
	exitC   chan int
	commC   chan []string
	loaderC chan *Loader

	startT, checkT, killT <-chan time.Time
}

func NewJob(loader *Loader, name string) *Job {
	job := new(Job)
	job.L = loader

	job.Name = name

	job.logC = logC // TODO: support onLog transformations in loader
	job.exitC = make(chan int)
	job.commC = make(chan []string)
	job.loaderC = make(chan *Loader)
	job.State = "wait"

	auto := job.L.Call(job.Name, "auto")

	if len(auto) == 1 && auto[0] == "true" {
		job.startT = timerC(0)

	} else if len(auto) > 0 {
		job.Log("state wait (on " + strings.Join(auto, ", ") + ")")
		waitC := make(chan bool)
		waitMu.Lock()
		for _, j := range auto {
			waitMap[j] = append(waitMap[j], waitC)
		}
		waitMu.Unlock()

		// bit of a weird construct to accomplish waiting:
		// there might be more elegant solutions that also phase out waitMu
		startT := make(chan time.Time)
		job.startT = startT
		go func() {
			for i := 0; i < len(auto); i++ {
				<-waitC
			}
			startT <- time.Now()
		}()
	}

	go job.runloop()

	return job
}

func (job *Job) Signal(sig os.Signal) {
	if job.Cmd != nil {
		job.Cmd.Process.Signal(sig)
	}
}

func (job *Job) Start() {
	switch job.State {
	case "wait", "exit", "done":

		execArgs := job.Call("exec")
		if len(execArgs) == 0 {
			execArgs = []string{"/bin/echo", "missing exec"}
		}
		job.Cmd = exec.Command(execArgs[0], execArgs[1:]...)
		job.Cmd.Env = job.Call("env")
		job.Cmd.Stdout = &lineWriter{C: job.logC, Prefix: job.Name + ":1 "}
		job.Cmd.Stderr = &lineWriter{C: job.logC, Prefix: job.Name + ":2 "}

		job.startT = nil
		job.checkT = timerC(0)
		job.toState("start")

		err := job.Cmd.Start()
		if err != nil {
			job.Log(err.Error())
			go func() { job.exitC <- -1 }()
		}
		go func() {
			err := job.Cmd.Wait()
			if err != nil {
				job.Log(err.Error())
				if status, ok := err.(*exec.ExitError).Sys().(syscall.WaitStatus); ok {
					job.exitC <- status.ExitStatus()
				} else {
					job.exitC <- 255
				}
			} else {
				job.exitC <- 0
			}

		}()

	default:
		job.Log("cannot start; state is " + job.State)
	}
}

func (job *Job) Stop() {
	switch job.State {
	case "start", "run", "degrade":
		r := job.Call("stop", strconv.Itoa(job.Cmd.Process.Pid))
		if len(r) == 0 {
			job.Signal(os.Interrupt)
		}
		job.killT = timerC(intArg(r, 0, 5000))
	case "exit":
		job.startT = nil
		job.toState("stop")
	default:
		job.Log("cannot stop; state is " + job.State)
	}
}

func (job *Job) Restart() {
	/*switch job.State {
	case "start", "run", "degrade":
		job.Signal(os.Interrupt)
		job.killT = timerC(3)
	case "exit":
		job.Start() // will also reset startT
	default:
		job.Log("cannot restart; state is " + job.State)
	}*/
}

func (job *Job) Ok(checkTime int) {
	switch job.State {
	case "start", "degrade", "run":
		job.toState("run")
		job.ready()
		job.checkT = timerC(checkTime)
	default:
		job.Log("cannot ok; state is " + job.State)
	}
}

func (job *Job) Degrade(checkTime int) {
	switch job.State {
	case "start":
		job.checkT = timerC(checkTime)
	case "run":
		job.toState("degrade")
		job.checkT = timerC(checkTime)
	default:
		job.Log("cannot degrade; state is " + job.State)
	}
}

func (job *Job) Fail() {
	job.Stop()
}

func (job *Job) Call(args ...string) []string {
	return job.L.Call(append([]string{job.Name}, args...)...)
}

func (job *Job) CallDo(args ...string) {
	r := job.Call(args...)
	if len(r) == 0 {
		return
	}
	if len(r[0]) > 0 && r[0][0] == '@' {
		// command for other job
		r[0] = r[0][1:]
		commC <- Comm{r, nil}
		return
	}

	switch r[0] {
	case "start":
		job.Start()
	case "stop":
		job.Stop()
	case "restart":
		job.Restart()
	case "fail":
		job.Fail()
	case "ok":
		job.Ok(intArg(r, 1, 1000))
	case "degrade":
		job.Degrade(intArg(r, 1, 1000))
	default:
		job.Log("unknown command " + r[0])
	}
}

func intArg(args []string, n int, def int) int {
	if len(args) <= n {
		return def
	}
	v, err := strconv.Atoi(args[n])
	if err != nil {
		return def
	}
	return v
}

func (job *Job) toState(state string, args ...string) {
	if job.State != state {
		job.State = state
		job.Log("state " + state)
		job.Call(append([]string{"on" + strings.ToUpper(state[:1]) + state[1:]}, args...)...)
	}
}

func (job *Job) ready() {
	// when this job is ready, signal jobs that are waiting for us
	waitMu.Lock()
	for _, c := range waitMap[job.Name] {
		c <- true
	}
	delete(waitMap, job.Name)
	waitMu.Unlock()
}

func (job *Job) Log(l string) {
	job.logC <- job.Name + " " + l
}

func (job *Job) runloop() {
	for {
		select {
		case l := <-job.loaderC:
			job.Log("new loader")
			job.L = l

		case <-job.startT:
			job.Start()

		case <-job.checkT:
			job.CallDo("check")

		case <-job.killT:
			job.Signal(os.Kill)

		case exitCode := <-job.exitC:
			job.checkT = nil
			job.killT = nil
			if job.State == "stop" {
				job.toState("stopped")
			} else {
				done := (job.State == "start" && exitCode == 0)
				job.toState("exit", strconv.Itoa(exitCode)) // might do stop
				if done {
					job.toState("done")
					job.ready()
				} else {
					job.startT = timerC(1000)
				}
			}

		case cmdArgs := <-job.commC:
			job.Call(cmdArgs...)
		}
	}
}

func timerC(msec int) <-chan time.Time {
	return time.NewTimer(time.Duration(msec) * time.Millisecond).C
}

// io.Writer that writes lines to a channel
type lineWriter struct {
	C      chan string
	Prefix string
	buf    []byte
}

func (c *lineWriter) Write(b []byte) (int, error) {
	c.buf = append(c.buf, b...)
	for i := 0; i < len(c.buf); i++ {
		if c.buf[i] == '\n' {
			c.C <- c.Prefix + string(c.buf[0:i])
			c.buf = c.buf[i+1:]
			i = -1 // wrap-around will incr to 0
		}
	}
	return len(b), nil
}
