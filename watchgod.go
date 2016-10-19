package watchgod

import (
	"encoding/json"
	"log"
	"os"
)

type job struct {
	Name    string
	Exec    interface{}
	Auto    interface{}
	Check   interface{}
	OnStart interface{}
	OnStop  interface{}
	OnExit  interface{}
}

var jobs = make(map[string]*job)

func Job(name string) *job {
	j := new(job)
	j.Name = name
	jobs[name] = j
	return j
}

func (j *job) ExecFunc(f func()) {
	if len(os.Args) == 3 && os.Args[1] == "godv1" && os.Args[2] == j.Name {
		log.SetFlags(0)
		f()
		os.Exit(0)
	}

	j.Exec = []string{os.Args[0], "godv1", j.Name}
}

func Init() {
	if len(os.Args) != 2 || os.Args[1] != "godv1" {
		return
	}

	i := json.NewDecoder(os.Stdin)
	o := json.NewEncoder(os.Stdout)

	keys := make([]string, 1, len(jobs)+1)
	keys[0] = "godv1"
	for k := range jobs {
		keys = append(keys, k)
	}
	o.Encode(keys)

	for {
		var cmdArgs []string
		err := i.Decode(&cmdArgs)
		if err != nil || len(cmdArgs) < 2 {
			os.Exit(1)
		}

		job := jobs[cmdArgs[0]]
		if job == nil {
			os.Exit(1)
		}

		var prop interface{}

		switch cmdArgs[1] {
		case "exec":
			prop = job.Exec
		case "auto":
			prop = job.Auto
		case "check":
			prop = job.Check
		case "onStart":
			prop = job.OnStart
		}

		if v, ok := prop.([]string); ok {
			o.Encode(v)
		} else if v, ok := prop.(string); ok {
			o.Encode([]string{v})
		} else if v, ok := prop.(bool); ok {
			if v {
				o.Encode([]string{"true"})
			} else {
				o.Encode([]string{"false"})
			}
		} else if v, ok := prop.(func() []string); ok {
			o.Encode(v())
		} else if v, ok := prop.(func() string); ok {
			o.Encode([]string{v()})
		} else if v, ok := prop.(func(string) []string); ok {
			o.Encode(v(cmdArgs[2]))
		} else if v, ok := prop.(func(string) string); ok {
			o.Encode([]string{v(cmdArgs[2])})
		} else if v, ok := prop.(func([]string) []string); ok {
			o.Encode(v(cmdArgs[2:]))
		} else if v, ok := prop.(func([]string) string); ok {
			o.Encode([]string{v(cmdArgs[2:])})
		} else {
			o.Encode([]string{})
		}
	}

	// should never reach, god kills -9
}
