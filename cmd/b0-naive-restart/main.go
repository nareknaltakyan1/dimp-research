// Command b0-naive-restart is the BASELINE: an upgrade that stops the old
// process' listener and then starts the new binary, which binds the port
// afterwards. Between the two, clients get "connection refused"; in "hard"
// mode in-flight requests are also cut. The count is lost unless -state-file
// is set.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/handoff"
)

var version = "v1" // -ldflags "-X main.version=v2"

func main() {
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	ver := fs.String("version", version, "logical version")
	mode := fs.String("mode", "graceful", "how the old process stops: graceful (Shutdown, drain in-flight) | hard (Close, cut in-flight)")
	execMode := fs.String("exec-mode", "spawn", "spawn (new pid, old exits) | exec (execve in place, same pid)")
	stateFile := fs.String("state-file", "", "persist count here across restart (empty = state lost)")
	nextBin := fs.String("next-binary", "", "binary to upgrade to (default: self with -version=<to>)")
	nextVer := fs.String("next-version", "v2", "version for SIGHUP / POST without to=")
	allowBin := fs.Bool("allow-bin-param", false, "allow POST /admin/upgrade?bin=")
	startupDelay := fs.Duration("startup-delay", 0, "simulated initialisation time before binding")
	drainTO := fs.Duration("drain-timeout", 30*time.Second, "graceful drain limit")
	_ = fs.Parse(os.Args[1:])

	upgraded := os.Getenv("DIMP_B0_UPGRADE") == "1"
	_ = os.Unsetenv("DIMP_B0_UPGRADE")

	var start uint64
	if *stateFile != "" {
		if b, err := os.ReadFile(*stateFile); err == nil {
			start, _ = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		}
	}
	time.Sleep(*startupDelay)

	svc := handoff.NewService(*ver, start)
	srv := &http.Server{Handler: svc.Handler(), ReadHeaderTimeout: 10 * time.Second}

	// The old process may still be releasing the port; retry bind briefly.
	var ln net.Listener
	var err error
	for i := 0; i < 250; i++ {
		if ln, err = net.Listen("tcp", *addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		handoff.Emit("fatal", map[string]any{"err": err.Error()})
		os.Exit(1)
	}
	svc.SetReady(true)
	go func() { _ = srv.Serve(ln) }()
	if upgraded {
		handoff.Emit("new_version_serving", map[string]any{"version": *ver, "count_start": start})
		handoff.Emit("upgrade_done", map[string]any{"to": *ver})
	}

	var upgrading atomic.Bool
	done := make(chan struct{})
	doUpgrade := func(to, bin string) error {
		if !upgrading.CompareAndSwap(false, true) {
			return errors.New("upgrade already in progress")
		}
		if to == "" {
			to = *nextVer
		}
		self, _ := os.Executable()
		if bin == "" {
			bin = *nextBin
		}
		if bin == "" {
			bin = self
		}
		go func() {
			handoff.Emit("upgrade_start", map[string]any{"from": *ver, "to": to, "bin": bin, "mode": *mode})
			svc.SetReady(false)
			if *mode == "hard" {
				_ = srv.Close()
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), *drainTO)
				_ = srv.Shutdown(ctx)
				cancel()
			}
			if *stateFile != "" {
				_ = os.WriteFile(*stateFile, []byte(strconv.FormatUint(svc.Count(), 10)), 0o644)
			}
			handoff.Emit("old_version_drained", map[string]any{"version": *ver, "count": svc.Count()})
			var args []string
			fs.VisitAll(func(f *flag.Flag) {
				if f.Name != "version" {
					args = append(args, "-"+f.Name+"="+f.Value.String())
				}
			})
			if bin == self {
				args = append(args, "-version="+to)
			}
			env := append(os.Environ(), "DIMP_B0_UPGRADE=1")
			if *execMode == "exec" {
				err := syscall.Exec(bin, append([]string{bin}, args...), env)
				handoff.Emit("upgrade_failed", map[string]any{"err": fmt.Sprint("exec: ", err)})
				os.Exit(1)
			}
			cmd := exec.Command(bin, args...)
			cmd.Env = env
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if err := cmd.Start(); err != nil {
				// Nothing to roll back to cheaply: rebinding is the baseline's only option.
				handoff.Emit("upgrade_failed", map[string]any{"err": err.Error()})
				os.Exit(1)
			}
			close(done)
		}()
		return nil
	}
	svc.OnUpgrade = func(to, bin string) error {
		if bin != "" && !*allowBin {
			return errors.New("bin= not allowed (start with -allow-bin-param)")
		}
		return doUpgrade(to, bin)
	}

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	for {
		select {
		case <-done:
			return
		case s := <-sigs:
			if s == syscall.SIGHUP {
				_ = doUpgrade("", "")
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), *drainTO)
			_ = srv.Shutdown(ctx)
			cancel()
			return
		}
	}
}
