package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/svc"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type apiResp struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
	Count   uint64 `json:"count"`
}

// TestUpgradeUnderLoad: continuous load with long-ish requests, upgrade
// v1->v2 through the HTTP admin endpoint, expect zero errors, a version flip,
// and a strictly continuing set of counts (no duplicates, no gaps).
func TestUpgradeUnderLoad(t *testing.T) {
	counter := new(atomic.Uint64)
	events := &syncBuf{}
	s := svc.New(svc.Config{
		InitialName: "v1", Initial: &v1{counterImpl{version: "v1", count: counter}},
		Load: build, Count: counter.Load, DrainTimeout: 5 * time.Second,
		Events: svc.NewEventLog(events),
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	var stop atomic.Bool
	var mu sync.Mutex
	var resps []apiResp
	var errs atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				r, err := http.Get(ts.URL + "/api?work_ms=20")
				if err != nil || r.StatusCode != 200 {
					errs.Add(1)
					continue
				}
				var a apiResp
				if err := json.NewDecoder(r.Body).Decode(&a); err != nil || r.Header.Get("X-Version") != a.Version {
					errs.Add(1)
				}
				r.Body.Close()
				mu.Lock()
				resps = append(resps, a)
				mu.Unlock()
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	r, err := http.Post(ts.URL+"/admin/upgrade?to=v2", "", nil)
	if err != nil || r.StatusCode != http.StatusAccepted {
		t.Fatalf("upgrade: %v %v", err, r)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(events.String(), "upgrade_done") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	if errs.Load() != 0 {
		t.Fatalf("%d failed requests", errs.Load())
	}
	sort.Slice(resps, func(i, j int) bool { return resps[i].Count < resps[j].Count })
	seen := map[string]int{}
	for i, a := range resps {
		seen[a.Version]++
		if a.Count != uint64(i+1) {
			t.Fatalf("count sequence broken at %d: %d", i, a.Count)
		}
	}
	if seen["v1"] == 0 || seen["v2"] == 0 {
		t.Fatalf("expected both versions, got %v", seen)
	}
	// After the flip no v1 response may carry a count larger than any v2 count
	// observed after drain; simplest check: the last response is v2.
	if resps[len(resps)-1].Version != "v2" {
		t.Fatal("last response not served by v2")
	}
	ev := events.String()
	order := []string{"upgrade_start", "new_version_serving", "old_version_drained", "upgrade_done"}
	last := -1
	for _, e := range order {
		i := strings.Index(ev, `"ev":"`+e+`"`)
		if i < 0 || i < last {
			t.Fatalf("event %s missing or out of order:\n%s", e, ev)
		}
		last = i
	}
	t.Logf("responses=%d per-version=%v\nevents:\n%s", len(resps), seen, ev)
}

func TestUpgradeUnknownVersionFails(t *testing.T) {
	counter := new(atomic.Uint64)
	events := &syncBuf{}
	s := svc.New(svc.Config{
		InitialName: "v1", Initial: &v1{counterImpl{version: "v1", count: counter}},
		Load: build, Count: counter.Load, Events: svc.NewEventLog(events),
	})
	if err := s.Upgrade("v9"); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(events.String(), "upgrade_failed") || s.Status().Version != "v1" {
		t.Fatalf("expected upgrade_failed and still v1: %s", events.String())
	}
}
