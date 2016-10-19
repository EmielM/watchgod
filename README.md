# watchGoD v0.1

A process manager in golang with some novel ideas.

- Complex blocks in your stack (such as a 100kloc backend binary) should be checked and controlled by simpler blocks (for example a 50loc program that checks if website is still up). In watchgod, these simpler blocks are called **loaders**.

- The loader of your binary can be included in binary itself. This keeps your loader close to the more complex logic it manages.

- Loaders should not have too much state: should be safe to reload them at any time.

- Loaders could be used to pass over a server's `accept()-fd` when it eventually needs to reload for hot code reloading. (not implemented yet) 

For golang programs, you can use the `watchgod` library to create definitions in your binary's `init()` function. Example:

```go

package main

import (
	"net/http"
	"github.com/emielm/watchgod"
)

func main() {
	http.ListenAndServe(":80", myBackend)
}

func init() {
	job := watchgod.Job("backend")
	job.Auto = true
	job.Exec = []string{os.Args[0]}
	job.Check = func() string {
		return "ok"
	}

	job = watchgod.Job("postgres")
	job.Auto = true
	job.Exec = []string{"/usr/lib/postgresql/9.5/bin/postgres", "-D", "data/db"}

	watchgod.Init()
}

```

**This is an early, experimental release. Do not use in production just yet.**

## Installation and usage

`go get -u github.com/emielm/watchgod/...`

This repository contains both the golang library (`watchgod.go`) and the god command (`cmd/god/main.go`).

Compile/symlink your watchgod-compatible binaries to `build/god` and start with `god --dir build/god`.

## Loader protocol

All executable binaries in the `god/` directory should support the special invocation with `godv1` as `argv[1]`. In that case, they should speak the line-based json loader protocol over stdin/stdout.

## TODO

- Job states in transition diagram
- Think about auto-start implementation and how to improve 
- More consistent signal handling 
- Rethink the request/response json wire protocol to allow async checks that take longer
- Implement stdout/stderr transformations
- Get a server's accept-FD handover to work through loader
- Test more in production envs

