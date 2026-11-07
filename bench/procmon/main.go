// Command procmon samples memory of every process in a session (Linux only).
//
// The driver starts the server with setsid, so the server PID is also the
// session ID. All processes that inherit that session (children, re-exec'd
// successors, FD-passing peers started by the server) are sampled, even if
// the original process exits. Each tick writes one row per live process:
//
//	ts_ns,pid,comm,rss_kb,vmhwm_kb,threads,fds
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func sid(pid string) (int, string) {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return -1, ""
	}
	s := string(b)
	l, r := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if l < 0 || r < 0 {
		return -1, ""
	}
	f := strings.Fields(s[r+2:]) // state ppid pgrp session ...
	if len(f) < 4 {
		return -1, ""
	}
	v, _ := strconv.Atoi(f[3])
	return v, s[l+1 : r]
}

func status(pid string) (rss, hwm, thr int64) {
	f, err := os.Open("/proc/" + pid + "/status")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		val := func() int64 {
			fs := strings.Fields(line)
			if len(fs) < 2 {
				return 0
			}
			v, _ := strconv.ParseInt(fs[1], 10, 64)
			return v
		}
		switch {
		case strings.HasPrefix(line, "VmRSS:"):
			rss = val()
		case strings.HasPrefix(line, "VmHWM:"):
			hwm = val()
		case strings.HasPrefix(line, "Threads:"):
			thr = val()
		}
	}
	return
}

func main() {
	session := flag.Int("sid", 0, "session id to sample (server pid started via setsid)")
	every := flag.Duration("interval", 100*time.Millisecond, "sampling interval")
	out := flag.String("out", "rss.csv", "output CSV")
	flag.Parse()
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "ts_ns,pid,comm,rss_kb,vmhwm_kb,threads,fds")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	t := time.NewTicker(*every)
	defer t.Stop()
	for {
		ents, _ := os.ReadDir("/proc")
		now := time.Now().UnixNano()
		for _, e := range ents {
			p := e.Name()
			if p[0] < '0' || p[0] > '9' {
				continue
			}
			s, comm := sid(p)
			if s != *session {
				continue
			}
			rss, hwm, thr := status(p)
			fds, _ := filepath.Glob("/proc/" + p + "/fd/*")
			fmt.Fprintf(w, "%d,%s,%s,%d,%d,%d,%d\n", now, p, comm, rss, hwm, thr, len(fds))
		}
		select {
		case <-t.C:
		case <-stop:
			w.Flush()
			f.Close()
			return
		}
	}
}
