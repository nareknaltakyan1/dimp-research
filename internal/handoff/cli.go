package handoff

import (
	"flag"
	"fmt"
	"os"
	"time"
)

// Main is the shared entry point of cmd/s4-reuseport and cmd/s5-fdpass.
// buildVersion and buildFail come from -ldflags "-X main.version=... -X main.simulateFail=...".
func Main(mode Mode, buildVersion, buildFail string) {
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	version := fs.String("version", buildVersion, "logical version reported by this binary")
	ctl := fs.String("ctl-sock", fmt.Sprintf("/tmp/dimp-%s.sock", mode), "unix control socket for the handoff protocol")
	nextBin := fs.String("next-binary", "", "binary to upgrade to (default: re-exec self with -version=<to>)")
	nextVer := fs.String("next-version", "v2", "version used for SIGHUP / POST without to=")
	allowBin := fs.Bool("allow-bin-param", false, "allow POST /admin/upgrade?bin=PATH (arbitrary exec: trusted networks only)")
	readyTO := fs.Duration("ready-timeout", 10*time.Second, "successor must be ready within this, else rollback")
	drainTO := fs.Duration("drain-timeout", 30*time.Second, "max time to drain in-flight requests")
	stall := fs.Duration("stall-accept", 0, "reuseport pitfall demo: stop accepting this long before closing the old listener")
	pidfile := fs.String("pidfile", "", "write the active server pid here (used by -supervise)")
	takeover := fs.Bool("takeover", false, "connect to -ctl-sock and take over a running server")
	supervise := fs.Bool("supervise", false, "run as PID-1 style supervisor (subreaper) around a worker")
	_ = fs.Parse(os.Args[1:])

	if *supervise {
		if *pidfile == "" {
			*pidfile = fmt.Sprintf("/tmp/dimp-%s.pid", mode)
		}
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		argv := append([]string{self}, FlagArgs(fs, "supervise")...)
		os.Exit(Supervise(argv, *pidfile))
	}

	s := NewServer(Config{
		Mode: mode, Addr: *addr, CtlSock: *ctl, Version: *version,
		NextBinary: *nextBin, NextVersion: *nextVer, AllowBinParam: *allowBin,
		ReadyTimeout: *readyTO, DrainTimeout: *drainTO, StallAccept: *stall,
		PIDFile: *pidfile, Takeover: *takeover, SimulateFail: buildFail,
		ChildArgs: func(to string, same bool) []string {
			args := FlagArgs(fs, "version", "takeover", "supervise")
			if same && to != "" {
				args = append(args, "-version="+to)
			}
			return args
		},
	})
	if err := s.Run(); err != nil {
		Emit("fatal", map[string]any{"err": err.Error(), "version": *version})
		os.Exit(1)
	}
}
