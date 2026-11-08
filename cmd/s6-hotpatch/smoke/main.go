// Command smoke drives s6-hotpatch end to end: concurrent /api load while a
// scripted sequence of upgrades, failing upgrades and rollbacks runs, then
// checks every response:
//
//   - non-200 responses
//   - mixed patch sets: the per-step implementation trace of each request
//     must be exactly the one expected for the patch-set label that request
//     pinned (v1 or v2); any other combination is a mixed request
//   - count continuity: counts are unique and dense (max == #responses)
//   - version flips over time and the bug fix (v2 discounts, v1 does not)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type resp struct {
	Version string   `json:"version"`
	Count   uint64   `json:"count"`
	TsNs    int64    `json:"ts_ns"`
	Epoch   uint64   `json:"epoch"`
	Cents   int64    `json:"total_cents"`
	Impls   []string `json:"impls"`
	code    int
	start   int64
}

var expected = map[string]string{
	"v1": "handle@v1,validate@v1,price@v1,discount@v1,format@v1",
	"v2": "handle@v1,validate@v2,price@v1,discount@v2,format@v2",
}
var cents = map[string]int64{"v1": 5997, "v2": 5398}

func main() {
	base := flag.String("url", "http://127.0.0.1:8080", "server")
	conc := flag.Int("c", 16, "concurrent clients")
	dur := flag.Duration("d", 8*time.Second, "duration")
	script := flag.String("script", "1s:upgrade:v2,2s:rollback,2.5s:upgrade:badsig,3s:upgrade:tampered,3.5s:upgrade:badfp,4s:upgrade:badfp-liar,4.5s:upgrade:v2,5s:upgrade:old,5.2s:upgrade:expired,5.5s:rollback,6.5s:upgrade:v2",
		"comma-separated at:action[:arg]")
	long := flag.Int("long", 300, "work_ms of the long requests (every 10th)")
	flag.Parse()

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: *conc}}
	var mu sync.Mutex
	var all []resp
	var transport int
	stop := time.Now().Add(*dur)
	t0 := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < *conc; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(id)))
			for n := 0; time.Now().Before(stop); n++ {
				w := rng.Intn(20)
				if n%10 == 0 {
					w = *long
				}
				st := time.Now().UnixNano()
				r, err := client.Get(fmt.Sprintf("%s/api?work_ms=%d", *base, w))
				if err != nil {
					mu.Lock()
					transport++
					mu.Unlock()
					continue
				}
				var x resp
				json.NewDecoder(r.Body).Decode(&x)
				r.Body.Close()
				x.code, x.start = r.StatusCode, st
				mu.Lock()
				all = append(all, x)
				mu.Unlock()
			}
		}(i)
	}
	// admin script
	for _, step := range strings.Split(*script, ",") {
		parts := strings.Split(step, ":")
		at, _ := time.ParseDuration(parts[0])
		time.Sleep(time.Until(t0.Add(at)))
		u := *base + "/admin/" + parts[1]
		if len(parts) > 2 {
			u += "?to=" + parts[2]
		}
		r, err := client.Post(u, "", nil)
		if err == nil {
			r.Body.Close()
			fmt.Printf("t=%-5s POST %-40s -> %d\n", parts[0], strings.TrimPrefix(u, *base), r.StatusCode)
		} else {
			fmt.Printf("t=%-5s POST %s -> %v\n", parts[0], u, err)
		}
	}
	wg.Wait()

	// analysis
	non200, mixed, wrongCents := 0, 0, 0
	byVer := map[string]int{}
	seen := map[uint64]int{}
	var maxCount uint64
	var mixedEx []string
	for _, x := range all {
		if x.code != 200 {
			non200++
			continue
		}
		byVer[x.Version]++
		seen[x.Count]++
		if x.Count > maxCount {
			maxCount = x.Count
		}
		if got := strings.Join(x.Impls, ","); got != expected[x.Version] {
			mixed++
			if len(mixedEx) < 5 {
				mixedEx = append(mixedEx, fmt.Sprintf("label=%s epoch=%d impls=%s", x.Version, x.Epoch, got))
			}
		}
		if x.Cents != cents[x.Version] {
			wrongCents++
		}
	}
	dups := 0
	for _, n := range seen {
		if n > 1 {
			dups += n - 1
		}
	}
	ok := len(all) - non200
	// version flips ordered by server timestamp
	sort.Slice(all, func(i, j int) bool { return all[i].TsNs < all[j].TsNs })
	flips, last := 0, ""
	var flipSeq []string
	for _, x := range all {
		if x.code != 200 {
			continue
		}
		if last != "" && x.Version != last {
			flips++
		}
		if x.Version != last {
			flipSeq = append(flipSeq, x.Version)
		}
		last = x.Version
	}
	// Flips in ENTRY order: count is taken right after the request pinned
	// its patch set, so ordering by count approximates pin order. (Ordered
	// by completion time the versions interleave during every drain window,
	// because long requests pinned to the old set finish after the switch.)
	sort.Slice(all, func(i, j int) bool { return all[i].Count < all[j].Count })
	entryFlips, lastE, epochs := 0, "", []uint64{}
	for _, x := range all {
		if x.code != 200 {
			continue
		}
		if lastE != "" && x.Version != lastE {
			entryFlips++
		}
		lastE = x.Version
		if len(epochs) == 0 || epochs[len(epochs)-1] != x.Epoch {
			epochs = append(epochs, x.Epoch)
		}
	}
	fmt.Printf("\nrequests=%d ok=%d non200=%d transport_errors=%d by_version=%v\n", len(all), ok, non200, transport, byVer)
	fmt.Printf("count: max=%d unique=%d duplicates=%d gaps=%d\n", maxCount, len(seen), dups, int(maxCount)-len(seen))
	fmt.Printf("version flips in entry (count) order=%d, epoch sequence=%v\n", entryFlips, epochs)
	fmt.Printf("version flips in completion (ts_ns) order=%d (%d runs)\n", flips, len(flipSeq))
	fmt.Printf("mixed patch-set requests=%d wrong_total=%d\n", mixed, wrongCents)
	for _, e := range mixedEx {
		fmt.Println("  mixed example:", e)
	}
	if non200 > 0 || transport > 0 || dups > 0 || mixed > 0 || wrongCents > 0 {
		os.Exit(1)
	}
}
