package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nareknaltakyan1/dimp-research/cmd/s6-hotpatch/gen/biz"
	"github.com/nareknaltakyan1/dimp-research/internal/patchable"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newTest(t *testing.T) (*server, *httptest.Server, *syncBuf) {
	buf := &syncBuf{}
	patchable.Default.SetBaseLabel("v1")
	s := newServer(patchable.Default, t.TempDir(), nil, 5*time.Second, &events{w: buf, pid: 1})
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts, buf
}

func get(t *testing.T, url string) (apiResp, http.Header) {
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var x apiResp
	if err := json.NewDecoder(r.Body).Decode(&x); err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 200 {
		t.Fatalf("status %d: %+v", r.StatusCode, x)
	}
	return x, r.Header
}

func TestContract(t *testing.T) {
	_, ts, _ := newTest(t)
	x, h := get(t, ts.URL+"/api?work_ms=1")
	if x.Version != "v1" || h.Get("X-Version") != "v1" || x.Count == 0 || x.PID == 0 || x.TsNs == 0 {
		t.Fatalf("%+v", x)
	}
	if x.Cents != 5997 { // v1 bug: SAVE10 gives no discount
		t.Fatalf("v1 total %d", x.Cents)
	}
	r, _ := http.Get(ts.URL + "/admin/status")
	var st map[string]any
	json.NewDecoder(r.Body).Decode(&st)
	r.Body.Close()
	for _, k := range []string{"version", "pid", "inflight", "count"} {
		if _, ok := st[k]; !ok {
			t.Errorf("status missing %s", k)
		}
	}
}

func TestFailedUpgradeKeepsServing(t *testing.T) {
	s, ts, buf := newTest(t)
	s.upgrade("v2") // no manifest in the empty plugin dir
	if !strings.Contains(buf.String(), `"ev":"upgrade_failed"`) {
		t.Fatalf("no upgrade_failed event: %s", buf.String())
	}
	if x, _ := get(t, ts.URL+"/api"); x.Version != "v1" {
		t.Fatal(x.Version)
	}
	s.rollback()
	if !strings.Contains(buf.String(), "nothing to roll back") {
		t.Fatal(buf.String())
	}
}

// In-test replacements identical in behaviour to plugins/v2 (a plugin cannot
// be loaded by `go test`, which does not build with -buildmode=plugin).
var v2Patches = map[string]any{
	"biz.Validate": func(ctx context.Context, req biz.Request, tr *biz.Trace) error {
		tr.Add("validate@v2")
		return nil
	},
	"biz.ApplyDiscount": func(ctx context.Context, cents int64, coupon string, tr *biz.Trace) int64 {
		tr.Add("discount@v2")
		if pct, ok := biz.CouponPercent(coupon); ok {
			return cents - cents*pct/100
		}
		return cents
	},
	"biz.Format": func(ctx context.Context, tr *biz.Trace, cents int64, parts ...string) string {
		tr.Add("format@v2")
		return fmt.Sprint(cents)
	},
}

var want = map[string]string{
	"v1": "handle@v1,validate@v1,price@v1,discount@v1,format@v1",
	"v2": "handle@v1,validate@v2,price@v1,discount@v2,format@v2",
}

// Transactions applied and reverted under concurrent load: no request may
// mix versions, no request may fail, counts stay unique.
func TestTransactionsUnderLoad(t *testing.T) {
	s, ts, buf := newTest(t)
	stop := make(chan struct{})
	var mu sync.Mutex
	var got []apiResp
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				x, _ := get(t, fmt.Sprintf("%s/api?work_ms=%d", ts.URL, (n%5)*3))
				mu.Lock()
				got = append(got, x)
				mu.Unlock()
			}
		}(i)
	}
	for i := 0; i < 6; i++ {
		time.Sleep(40 * time.Millisecond)
		tx, err := s.reg.Apply("v2", v2Patches)
		if err != nil {
			t.Fatal(err)
		}
		s.finish(tx, time.Now(), "v1", "v2", "upgrade", nil)
		time.Sleep(40 * time.Millisecond)
		rv, err := s.reg.Revert(tx)
		if err != nil {
			t.Fatal(err)
		}
		s.finish(rv, time.Now(), "v2", "v1", "rollback", nil)
	}
	close(stop)
	wg.Wait()
	seen := map[uint64]bool{}
	by := map[string]int{}
	for _, x := range got {
		if strings.Join(x.Impls, ",") != want[x.Version] {
			t.Fatalf("mixed request: %+v", x)
		}
		if seen[x.Count] {
			t.Fatalf("duplicate count %d", x.Count)
		}
		seen[x.Count] = true
		by[x.Version]++
	}
	if by["v1"] == 0 || by["v2"] == 0 {
		t.Fatalf("both versions should have served: %v", by)
	}
	if n := strings.Count(buf.String(), `"ev":"old_version_drained"`); n != 12 {
		t.Fatalf("drained events %d", n)
	}
	t.Logf("requests=%d by_version=%v", len(got), by)
}
