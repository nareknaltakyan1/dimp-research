// Command hammer is a small closed-loop load generator for the handoff smoke
// tests: C workers hammer GET /api, optionally trigger an upgrade mid-run, and
// print a JSON summary of errors, versions, pids and count continuity.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type resp struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
	Count   uint64 `json:"count"`
}

func main() {
	base := flag.String("url", "http://127.0.0.1:8080", "server base url")
	conc := flag.Int("c", 32, "concurrent workers")
	dur := flag.Duration("d", 6*time.Second, "test duration")
	at := flag.Duration("upgrade-at", 2*time.Second, "when to trigger the upgrade (0 = never)")
	trigger := flag.String("trigger", "http", "http | sighup:<pid>")
	to := flag.String("to", "v2", "target version for POST /admin/upgrade")
	work := flag.Int("work-ms", 5, "work_ms per request")
	keep := flag.Bool("keepalive", false, "reuse connections (default: new TCP connection per request)")
	flag.Parse()

	tr := &http.Transport{DisableKeepAlives: !*keep, MaxIdleConnsPerHost: *conc}
	cl := &http.Client{Transport: tr, Timeout: 30 * time.Second}

	var mu sync.Mutex
	errKinds := map[string]int{}
	status := map[int]int{}
	versions := map[string]int{}
	pids := map[int]int{}
	counts := map[uint64]int{}
	var lastV1, firstV2 uint64 = 0, ^uint64(0)
	var maxLat time.Duration
	var lats []time.Duration
	ok := 0
	var samples []string
	t00 := time.Now()

	stop := time.Now().Add(*dur)
	var wg sync.WaitGroup
	for i := 0; i < *conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(stop) {
				t0 := time.Now()
				r, err := cl.Get(fmt.Sprintf("%s/api?work_ms=%d", *base, *work))
				lat := time.Since(t0)
				if err != nil {
					mu.Lock()
					errKinds[classify(err)]++
					if len(samples) < 6 {
						samples = append(samples, fmt.Sprintf("+%dms %v", time.Since(t00).Milliseconds(), err))
					}
					mu.Unlock()
					time.Sleep(time.Millisecond)
					continue
				}
				body, _ := io.ReadAll(r.Body)
				r.Body.Close()
				var x resp
				_ = json.Unmarshal(body, &x)
				mu.Lock()
				status[r.StatusCode]++
				lats = append(lats, lat)
				if lat > maxLat {
					maxLat = lat
				}
				if r.StatusCode == 200 {
					ok++
					versions[x.Version]++
					pids[x.PID]++
					counts[x.Count]++
					if x.Version == "v1" && x.Count > lastV1 {
						lastV1 = x.Count
					}
					if x.Version != "v1" && x.Count < firstV2 {
						firstV2 = x.Count
					}
				}
				mu.Unlock()
			}
		}()
	}
	if *at > 0 {
		time.Sleep(*at)
		if strings.HasPrefix(*trigger, "sighup:") {
			var pid int
			fmt.Sscanf(strings.TrimPrefix(*trigger, "sighup:"), "%d", &pid)
			if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
				fmt.Fprintln(os.Stderr, "sighup:", err)
			}
		} else {
			r, err := http.Post(*base+"/admin/upgrade?to="+*to, "", nil)
			if err != nil {
				fmt.Fprintln(os.Stderr, "trigger:", err)
			} else {
				fmt.Fprintln(os.Stderr, "trigger status:", r.StatusCode)
				r.Body.Close()
			}
		}
	}
	wg.Wait()

	dups := 0
	var maxCount uint64
	for c, n := range counts {
		if n > 1 {
			dups += n - 1
		}
		if c > maxCount {
			maxCount = c
		}
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	p := func(q float64) string {
		if len(lats) == 0 {
			return "0"
		}
		return lats[int(q*float64(len(lats)-1))].String()
	}
	errTotal := 0
	for _, n := range errKinds {
		errTotal += n
	}
	non200 := 0
	for s, n := range status {
		if s != 200 {
			non200 += n
		}
	}
	if firstV2 == ^uint64(0) {
		firstV2 = 0
	}
	out := map[string]any{
		"ok_200": ok, "non_200": non200, "conn_errors": errTotal, "error_kinds": errKinds,
		"status": status, "versions": versions, "pids": pids,
		"duplicate_counts": dups, "max_count": maxCount, "distinct_counts": len(counts),
		"last_v1_count": lastV1, "first_v2_count": firstV2,
		"error_samples": samples, "upgrade_at_ms": at.Milliseconds(),
		"lat_p50": p(0.5), "lat_p99": p(0.99), "lat_max": maxLat.String(),
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

func classify(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "conn_refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "conn_reset"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	case strings.Contains(err.Error(), "Timeout"):
		return "timeout"
	}
	return "other:" + err.Error()
}
