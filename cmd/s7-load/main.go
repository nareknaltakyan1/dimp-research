// Command s7-load is the S7 demo load generator. It sends GET /api?x=..&key=..
// at a fixed rate (open loop: a request slot is skipped and counted, never
// queued, when all workers are busy) and checks every response:
//
//   - status code / transport errors (client-visible failures),
//   - result == price(x) per the v1 reference semantics (client-visible
//     WRONG answers, e.g. from a buggy patch leaking past shadow),
//   - per-instance counter continuity (X-Instance header): a counter that
//     drops is in-memory state loss (e.g. a pod restart).
//
// It prints a one-line report every -report and a final JSON summary when it
// stops (-duration elapsed, SIGTERM, or GET /stop on -ctl-addr).
//
//	s7-load -url http://s7-inplace:8080 -rps 150
//	s7-load -stop http://localhost:8081      # ask a running instance to stop
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"
)

func price(x int64) int64 {
	r := (31*x + 7) % 1000003
	if r < 0 {
		r += 1000003
	}
	return r
}

type second struct {
	OK, Err, Wrong int64
	Versions       map[string]int64
}

type stats struct {
	mu          sync.Mutex
	start       time.Time
	Total       int64             `json:"total"`
	OK          int64             `json:"ok"`
	Failed      int64             `json:"failed"`
	TransportEr int64             `json:"transport_errors"`
	Wrong       int64             `json:"wrong_results"`
	Skipped     int64             `json:"skipped_slots"`
	Codes       map[string]int64  `json:"status_codes"`
	Versions    map[string]int64  `json:"versions"`
	Instances   map[string]int64  `json:"instances"`
	MaxCount    map[string]uint64 `json:"max_count_per_instance"`
	Resets      int64             `json:"counter_resets"`
	LatUs       []int64           `json:"-"`
	Timeline    []second          `json:"-"`
	FirstErrors []string          `json:"first_errors,omitempty"`
}

func (s *stats) sec() *second {
	i := int(time.Since(s.start) / time.Second)
	for len(s.Timeline) <= i {
		s.Timeline = append(s.Timeline, second{Versions: map[string]int64{}})
	}
	return &s.Timeline[i]
}

type apiResp struct {
	Version string `json:"version"`
	Count   uint64 `json:"count"`
	X       int64  `json:"x"`
	Result  int64  `json:"result"`
}

func main() {
	url := flag.String("url", "http://localhost:8080", "service base URL")
	rps := flag.Int("rps", 150, "request rate")
	workers := flag.Int("workers", 32, "max concurrent requests")
	duration := flag.Duration("duration", 0, "run time (0 = until stopped)")
	report := flag.Duration("report", 5*time.Second, "report interval")
	ctlAddr := flag.String("ctl-addr", ":8081", "control address (/stop, /summary)")
	stopURL := flag.String("stop", "", "client mode: stop the instance at this base URL and print its summary")
	workMs := flag.Int("work-ms", 0, "work_ms parameter")
	keepAlive := flag.Bool("keepalive", true, "reuse connections (false: one TCP connection per request, so kube-proxy spreads load over all pods)")
	flag.Parse()

	if *stopURL != "" {
		resp, err := http.Get(*stopURL + "/stop")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		io.Copy(os.Stdout, resp.Body)
		return
	}

	st := &stats{start: time.Now(), Codes: map[string]int64{}, Versions: map[string]int64{}, Instances: map[string]int64{}, MaxCount: map[string]uint64{}}
	ctx, cancel := context.WithCancel(context.Background())
	if *duration > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), *duration)
	}
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-sigs; cancel() }()

	finalCh := make(chan []byte, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		cancel()
		w.Write(<-finalCh)
	})
	mux.HandleFunc("/summary", func(w http.ResponseWriter, r *http.Request) { w.Write(summary(st)) })
	go http.ListenAndServe(*ctlAddr, mux)

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: *workers, MaxConnsPerHost: *workers, DisableKeepAlives: !*keepAlive}}
	slots := make(chan struct{}, *workers)
	var wg sync.WaitGroup
	tick := time.NewTicker(time.Second / time.Duration(*rps))
	rep := time.NewTicker(*report)
	fmt.Printf("{\"ev\":\"load_start\",\"url\":%q,\"rps\":%d,\"ts\":%q}\n", *url, *rps, time.Now().Format(time.RFC3339Nano))
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-rep.C:
			st.mu.Lock()
			fmt.Printf("{\"ev\":\"load_report\",\"t_s\":%.0f,\"total\":%d,\"failed\":%d,\"wrong\":%d,\"resets\":%d,\"versions\":%s,\"max_count\":%s}\n",
				time.Since(st.start).Seconds(), st.Total, st.Failed, st.Wrong, st.Resets, js(st.Versions), js(st.MaxCount))
			st.mu.Unlock()
		case <-tick.C:
			select {
			case slots <- struct{}{}:
			default:
				st.mu.Lock()
				st.Skipped++
				st.mu.Unlock()
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				one(client, *url, *workMs, st)
			}()
		}
	}
	tick.Stop()
	wg.Wait()
	out := summary(st)
	fmt.Printf("%s\n", out)
	finalCh <- out
	time.Sleep(500 * time.Millisecond) // let /stop deliver the summary
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

func one(c *http.Client, base string, workMs int, st *stats) {
	x := rand.Int64N(1_000_000)
	key := "user-" + strconv.Itoa(rand.IntN(1000))
	u := fmt.Sprintf("%s/api?x=%d&key=%s", base, x, key)
	if workMs > 0 {
		u += "&work_ms=" + strconv.Itoa(workMs)
	}
	t0 := time.Now()
	resp, err := c.Get(u)
	lat := time.Since(t0)
	var body []byte
	code := 0
	inst := ""
	if err == nil {
		body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		code = resp.StatusCode
		inst = resp.Header.Get("X-Instance")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.Total++
	st.LatUs = append(st.LatUs, lat.Microseconds())
	sec := st.sec()
	if err != nil || code != 200 {
		st.Failed++
		sec.Err++
		if err != nil {
			st.TransportEr++
			st.Codes["transport"]++
		} else {
			st.Codes[strconv.Itoa(code)]++
		}
		if len(st.FirstErrors) < 10 {
			st.FirstErrors = append(st.FirstErrors, fmt.Sprintf("%s code=%d err=%v body=%.120s", time.Now().Format(time.RFC3339Nano), code, err, body))
		}
		return
	}
	st.Codes["200"]++
	var r apiResp
	if json.Unmarshal(body, &r) != nil {
		st.Wrong++
		sec.Wrong++
		return
	}
	st.OK++
	sec.OK++
	st.Versions[r.Version]++
	sec.Versions[r.Version]++
	st.Instances[inst]++
	if r.X != x || r.Result != price(x) {
		st.Wrong++
		sec.Wrong++
		if len(st.FirstErrors) < 10 {
			st.FirstErrors = append(st.FirstErrors, fmt.Sprintf("wrong result x=%d got=%d want=%d version=%s", x, r.Result, price(x), r.Version))
		}
	}
	// Counter continuity: responses arrive out of order under concurrency,
	// so only a large drop indicates a reset (state loss).
	if m := st.MaxCount[inst]; r.Count+1000 < m && r.Count < m/2 {
		st.Resets++
	}
	if r.Count > st.MaxCount[inst] {
		st.MaxCount[inst] = r.Count
	}
}

func summary(st *stats) []byte {
	st.mu.Lock()
	defer st.mu.Unlock()
	lat := append([]int64(nil), st.LatUs...)
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	q := func(p float64) float64 {
		if len(lat) == 0 {
			return 0
		}
		return float64(lat[int(p*float64(len(lat)-1))]) / 1000
	}
	tl := make([]map[string]any, len(st.Timeline))
	for i, s := range st.Timeline {
		tl[i] = map[string]any{"t": i, "ok": s.OK, "err": s.Err, "wrong": s.Wrong, "v": s.Versions}
	}
	b, _ := json.Marshal(map[string]any{
		"ev": "load_summary", "duration_s": time.Since(st.start).Seconds(),
		"total": st.Total, "ok": st.OK, "failed": st.Failed, "transport_errors": st.TransportEr,
		"wrong_results": st.Wrong, "skipped_slots": st.Skipped, "status_codes": st.Codes,
		"versions": st.Versions, "instances": st.Instances, "max_count_per_instance": st.MaxCount,
		"counter_resets": st.Resets, "p50_ms": q(0.5), "p99_ms": q(0.99), "p999_ms": q(0.999), "max_ms": q(1),
		"first_errors": st.FirstErrors, "timeline": tl,
	})
	return b
}
