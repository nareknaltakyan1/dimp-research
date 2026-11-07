package handoff

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

// Supervise runs argv as a worker and stays alive as long as ANY descendant
// is alive, forwarding SIGHUP/SIGTERM/SIGINT to the pid in pidfile (the
// currently active server). It is meant to be PID 1 in a container: when the
// old server exits after a handoff, the new server (its child) is re-parented
// to us (Linux PR_SET_CHILD_SUBREAPER, or simply because we are PID 1) instead
// of the container exiting with the old process. Returns an exit code.
func Supervise(argv []string, pidfile string) int {
	if err := becomeSubreaper(); err != nil {
		fmt.Fprintf(os.Stderr, "supervisor: subreaper: %v\n", err)
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	p, err := os.StartProcess(argv[0], argv, &os.ProcAttr{
		Env:   append(os.Environ(), "DIMP_SUPERVISED=1"),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "supervisor: start: %v\n", err)
		return 1
	}
	first := p.Pid
	go func() {
		for sig := range sigs {
			pid := first
			if b, err := os.ReadFile(pidfile); err == nil {
				if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
					pid = n
				}
			}
			_ = syscall.Kill(pid, sig.(syscall.Signal))
		}
	}()
	code := 0
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil { // ECHILD: no descendants left
			return code
		}
		_ = pid
		if ws.Exited() {
			code = ws.ExitStatus()
		} else if ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
	}
}
