// Command loadgen is an OPEN-LOOP constant-rate HTTP load generator for the
// DIMP benchmark (see docs/evaluation-methodology.md).
//
// Request i is scheduled at intended_i = t0 + i/rate, independent of how fast
// earlier requests complete. Latency is measured from the INTENDED send time,
// so client-side queueing caused by a stalled server is charged to the server
// (no coordinated omission, as in wrk2). The concurrency cap bounds in-flight
// requests; when the cap is hit the dispatcher waits, the delay shows up as
// start_ns - intended_ns and is included in latency_ns.
//
// One CSV row per request is written to -out.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type result struct {
	intended, start, end int64
	status               int
	errClass             string
	version              string
	pid                  int64
	count                int64
	serverTS             int64
	workMS               int
	attempts             int32
	reused               bool
}

type mixEntry struct {
	ms  int
	cum float64
}

func parseMix(s string) ([]mixEntry, error) {
	var out []mixEntry
	tot := 0.0
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("bad mix entry %q (want ms:weight)", part)
		}
		ms, err := strconv.Atoi(kv[0])
		if err != nil {
			return nil, err
		}
		w, err := strconv.ParseFloat(kv[1], 64)
		if err != nil {
			return nil, err
		}
		tot += w
		out = append(out, mixEntry{ms, tot})
	}
	for i := range out {
		out[i].cum /= tot
	}
	return out, nil
}

func pick(m []mixEntry, u float64) int {
	for _, e := range m {
		if u < e.cum {
			return e.ms
		}
	}
	return m[len(m)-1].ms
}

// classify maps a transport error to one of the harness error classes.
func classify(err error) string {
	if err == nil {
		return "none"
	}
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "reset"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return "refused"
	case strings.Contains(s, "connection reset"), strings.Contains(s, "broken pipe"):
		return "reset"
	case strings.Contains(s, "EOF"), strings.Contains(s, "server closed idle connection"):
		return "eof"
	case strings.Contains(s, "timeout"):
		return "timeout"
	}
	return "other"
}

// emptyBody is a non-nil, non-replayable empty request body. Its presence
// makes net/http consider the request non-replayable, which disables the
// transport's silent retry of GETs on dead keep-alive connections. Without
// this, a handoff that kills idle keep-alive connections would be masked.
type emptyBody struct{}

func (emptyBody) Read([]byte) (int, error) { return 0, io.EOF }
func (emptyBody) Close() error             { return nil }

func main() {
	var (
		url       = flag.String("url", "http://127.0.0.1:8080/api", "target URL (work_ms is appended)")
		rate      = flag.Float64("rate", 200, "requests per second (open loop)")
		dur       = flag.Duration("duration", 30*time.Second, "test duration")
		conc      = flag.Int("concurrency", 256, "max in-flight requests")
		mixS      = flag.String("work-mix", "0:1", "work_ms mix, e.g. 0:0.9,500:0.1")
		ka        = flag.Bool("keepalive", true, "use HTTP keep-alive")
		timeout   = flag.Duration("timeout", 5*time.Second, "per-request timeout")
		out       = flag.String("out", "loadgen.csv", "output CSV path")
		seed      = flag.Int64("seed", 1, "RNG seed for the work mix")
		noRetry   = flag.Bool("no-retry", true, "disable net/http transparent retries of idempotent requests")
		startFile = flag.String("start-file", "", "if set, write t0 (unix ns) here once the schedule starts")
		spin      = flag.Duration("spin", 2*time.Millisecond, "busy-wait this long before each send instead of sleeping (timer wake-up jitter would otherwise be added to latency)")
	)
	flag.Parse()
	mix, err := parseMix(*mixS)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: *timeout, KeepAlive: 30 * time.Second}).DialContext,
		DisableKeepAlives:   !*ka,
		MaxIdleConns:        *conc,
		MaxIdleConnsPerHost: *conc,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	client := &http.Client{Transport: tr, Timeout: *timeout}

	n := int(*rate * dur.Seconds())
	res := make([]result, n)
	rng := rand.New(rand.NewSource(*seed))
	for i := range res {
		res[i].workMS = pick(mix, rng.Float64())
	}
	sep := "?"
	if strings.Contains(*url, "?") {
		sep = "&"
	}

	sem := make(chan struct{}, *conc)
	var wg sync.WaitGroup
	interval := float64(time.Second) / *rate
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	t0 := time.Now().Add(50 * time.Millisecond)
	// All per-request timestamps are t0's wall time plus a MONOTONIC offset, so
	// latencies are immune to wall-clock steps (the Docker Desktop VM clock is
	// periodically re-synced to the host, which produced negative latencies
	// when mixing wall readings). Only the anchor t0Wall is a wall-clock value.
	t0Wall := t0.UnixNano()
	ns := func(t time.Time) int64 { return t0Wall + int64(t.Sub(t0)) }
	if *startFile != "" {
		os.WriteFile(*startFile, []byte(strconv.FormatInt(t0Wall, 10)+"\n"), 0o644)
	}
	sent := 0
loop:
	for i := 0; i < n; i++ {
		intended := t0.Add(time.Duration(float64(i) * interval))
		if d := time.Until(intended); d > *spin {
			select {
			case <-time.After(d - *spin):
			case <-stop:
				break loop
			}
		}
		for time.Now().Before(intended) {
			runtime.Gosched()
		}
		sem <- struct{}{}
		sent++
		wg.Add(1)
		go func(i int, intended time.Time) {
			defer wg.Done()
			defer func() { <-sem }()
			r := &res[i]
			r.intended = ns(intended)
			var attempts int32
			trace := &httptrace.ClientTrace{GotConn: func(ci httptrace.GotConnInfo) {
				attempts++
				r.reused = ci.Reused
			}}
			u := *url
			if r.workMS > 0 {
				u += sep + "work_ms=" + strconv.Itoa(r.workMS)
			}
			req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, u, nil)
			if *noRetry {
				req.Body = emptyBody{}
				req.GetBody = nil
				req.ContentLength = 0
			}
			r.start = ns(time.Now())
			resp, err := client.Do(req)
			if err == nil {
				var body []byte
				body, err = io.ReadAll(resp.Body)
				resp.Body.Close()
				r.status = resp.StatusCode
				if err == nil {
					var b struct {
						Version string `json:"version"`
						PID     int64  `json:"pid"`
						Count   int64  `json:"count"`
						TS      int64  `json:"ts_ns"`
					}
					if json.Unmarshal(body, &b) == nil {
						r.version, r.pid, r.count, r.serverTS = b.Version, b.PID, b.Count, b.TS
					}
				}
			}
			r.end = ns(time.Now())
			r.attempts = attempts
			r.errClass = classify(err)
			if err == nil && r.status != 200 {
				r.errClass = "http_" + strconv.Itoa(r.status)
			}
		}(i, intended)
	}
	wg.Wait()
	res = res[:sent]

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	fmt.Fprintln(w, "seq,intended_ns,start_ns,end_ns,latency_ns,status,err_class,version,pid,count,server_ts_ns,work_ms,attempts,conn_reused")
	byClass := map[string]int{}
	lat := make([]int64, 0, len(res))
	for i, r := range res {
		byClass[r.errClass]++
		l := r.end - r.intended
		if r.errClass == "none" {
			lat = append(lat, l)
		}
		reused := 0
		if r.reused {
			reused = 1
		}
		fmt.Fprintf(w, "%d,%d,%d,%d,%d,%d,%s,%s,%d,%d,%d,%d,%d,%d\n", i, r.intended, r.start, r.end, l, r.status, r.errClass, r.version, r.pid, r.count, r.serverTS, r.workMS, r.attempts, reused)
	}
	w.Flush()
	f.Close()
	sort.Slice(lat, func(a, b int) bool { return lat[a] < lat[b] })
	q := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return time.Duration(lat[int(p*float64(len(lat)-1))])
	}
	now := time.Now()
	fmt.Fprintf(os.Stderr, "loadgen: sent=%d classes=%v p50=%v p99=%v max=%v wall_step_ns=%d\n", len(res), byClass, q(.5), q(.99), q(1), now.UnixNano()-ns(now))
}
