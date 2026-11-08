package canary

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type in struct {
	Count uint64
	X     int
}

type out struct {
	Version string
	Count   uint64
	Result  int
}

// impl builds a test version. bug(x) returns (wrongResult, err) overrides.
type impl struct {
	name   string
	delay  time.Duration
	bug    func(x int) (wrong bool, err error)
	closed atomic.Bool
	calls  atomic.Int64
	// useAfterClose counts calls that arrived after Close (must stay 0).
	useAfterClose atomic.Int64
	panics        bool
}

func ref(x int) int { return (x*31 + 7) % 1000003 }

func (m *impl) version() *Version[in, out] {
	return &Version[in, out]{
		Name: m.name,
		Serve: func(ctx context.Context, i in) (out, error) {
			if m.closed.Load() {
				m.useAfterClose.Add(1)
			}
			m.calls.Add(1)
			if m.panics && i.X%5 == 0 {
				panic("boom")
			}
			if m.delay > 0 {
				time.Sleep(m.delay)
			}
			r := ref(i.X)
			if m.bug != nil {
				wrong, err := m.bug(i.X)
				if err != nil {
					return out{}, err
				}
				if wrong {
					r++
				}
			}
			return out{Version: m.name, Count: i.Count, Result: r}, nil
		},
		Close: func() {
			if !m.closed.CompareAndSwap(false, true) {
				panic("closed twice")
			}
		},
	}
}

type harness struct {
	e       *Engine[in, out]
	counter atomic.Uint64
	admits  atomic.Int64
}

func newHarness(stable *impl) *harness {
	h := &harness{}
	h.e = New(Config[in, out]{
		Admit: func(i in) in {
			h.admits.Add(1)
			i.Count = h.counter.Add(1)
			return i
		},
		// Declared expected divergence: the version string.
		Equal: func(a, b out) bool { return a.Count == b.Count && a.Result == b.Result },
	}, stable.version())
	return h
}

type clientStats struct {
	total, errs, wrong atomic.Int64
	mu                 sync.Mutex
	byVersion          map[string]int64
}

// load runs n workers until stop is closed.
func (h *harness) load(workers int, stop <-chan struct{}, cs *clientStats, keyed bool) *sync.WaitGroup {
	cs.byVersion = map[string]int64{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				x := w*100000 + i
				key := ""
				if keyed {
					key = fmt.Sprintf("user-%d", x%997)
				}
				r := h.e.Serve(context.Background(), key, in{X: x}, nil)
				cs.total.Add(1)
				if r.Err != nil {
					cs.errs.Add(1)
				} else if r.Out.Result != ref(x) {
					cs.wrong.Add(1)
				}
				cs.mu.Lock()
				cs.byVersion[r.Version]++
				cs.mu.Unlock()
				time.Sleep(200 * time.Microsecond)
			}
		}(w)
	}
	return &wg
}

func testStrategy() Strategy {
	s := FastStrategy()
	for i := range s.Stages {
		if s.Stages[i].Weight < 1 {
			s.Stages[i].MinDuration = 100 * time.Millisecond
			s.Stages[i].MinSamples = 50
			s.Stages[i].MaxDuration = 10 * time.Second
		}
	}
	s.Stages[1].MinSamples = 5
	s.SLO.P99Slack = 5 * time.Millisecond
	s.SLO.MaxP99Factor = 3
	s.Interval = 10 * time.Millisecond
	return s
}

func waitRollout(t *testing.T, e *Engine[in, out]) Rollout {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := e.Wait(ctx)
	if err != nil {
		t.Fatalf("rollout did not finish: %v", err)
	}
	return r
}

func TestGoodCandidatePromotedUnderLoad(t *testing.T) {
	v1, v2 := &impl{name: "v1"}, &impl{name: "v2"}
	h := newHarness(v1)
	stop := make(chan struct{})
	var cs clientStats
	wg := h.load(8, stop, &cs, false)
	time.Sleep(50 * time.Millisecond)
	if err := h.e.Start(v2.version(), testStrategy()); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Start((&impl{name: "v9"}).version(), testStrategy()); !errors.Is(err, ErrBusy) {
		t.Fatalf("second rollout: want ErrBusy, got %v", err)
	}
	r := waitRollout(t, h.e)
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	h.e.WaitShadows()

	if r.Phase != PhasePromoted || h.e.Stable() != "v2" {
		t.Fatalf("want promoted to v2, got %s (%s), stable %s", r.Phase, r.Reason, h.e.Stable())
	}
	if cs.errs.Load() != 0 || cs.wrong.Load() != 0 {
		t.Fatalf("client errors %d wrong %d", cs.errs.Load(), cs.wrong.Load())
	}
	if !v1.closed.Load() || v1.useAfterClose.Load() != 0 {
		t.Fatalf("v1 closed=%v useAfterClose=%d", v1.closed.Load(), v1.useAfterClose.Load())
	}
	if v2.closed.Load() {
		t.Fatal("promoted v2 must not be closed")
	}
	// Host state: exactly one admission per client request, i.e. shadow
	// mirrors did not touch the counter.
	if got, want := h.counter.Load(), uint64(cs.total.Load()); got != want {
		t.Fatalf("counter %d != client requests %d (shadow leaked side effects)", got, want)
	}
	var names []string
	for _, s := range r.Stages {
		names = append(names, s.Stage+":"+s.Verdict)
	}
	t.Logf("stages: %s; decision %.1fms release %.1fms; versions %v", strings.Join(names, " "), r.DecisionMs, r.ReleaseMs, cs.byVersion)
	if len(r.Stages) != 5 {
		t.Fatalf("expected 5 stage records, got %d", len(r.Stages))
	}
	if r.Stages[0].Candidate.Count == 0 || r.Stages[0].Candidate.Mismatches != 0 {
		t.Fatalf("shadow stage: %+v", r.Stages[0].Candidate)
	}
}

// A candidate that is wrong for 1 in 7 inputs and fails 1 in 11 is caught in
// the shadow stage: clients see zero errors and zero wrong answers.
func TestBuggyCandidateCaughtInShadow(t *testing.T) {
	v1 := &impl{name: "v1"}
	v3 := &impl{name: "v3", delay: 2 * time.Millisecond, bug: func(x int) (bool, error) {
		if x%11 == 0 {
			return false, errors.New("v3 internal error")
		}
		return x%7 == 0, nil
	}}
	h := newHarness(v1)
	stop := make(chan struct{})
	var cs clientStats
	wg := h.load(8, stop, &cs, false)
	time.Sleep(30 * time.Millisecond)
	if err := h.e.Start(v3.version(), testStrategy()); err != nil {
		t.Fatal(err)
	}
	r := waitRollout(t, h.e)
	time.Sleep(30 * time.Millisecond)
	close(stop)
	wg.Wait()
	h.e.WaitShadows()

	if r.Phase != PhaseRolledBack || h.e.Stable() != "v1" {
		t.Fatalf("want rolled back to v1, got %s stable %s", r.Phase, h.e.Stable())
	}
	if len(r.Stages) != 1 || r.Stages[0].Mode != ModeShadow {
		t.Fatalf("expected rollback during shadow, stages %+v", r.Stages)
	}
	if cs.errs.Load() != 0 || cs.wrong.Load() != 0 || cs.byVersion["v3"] != 0 {
		t.Fatalf("clients affected during shadow: errs %d wrong %d served-by-v3 %d", cs.errs.Load(), cs.wrong.Load(), cs.byVersion["v3"])
	}
	if !v3.closed.Load() || v3.useAfterClose.Load() != 0 {
		t.Fatalf("v3 closed=%v useAfterClose=%d", v3.closed.Load(), v3.useAfterClose.Load())
	}
	if v1.closed.Load() {
		t.Fatal("stable must stay loaded")
	}
	t.Logf("rolled back: %s; decision %.1fms, release %.1fms, client requests %d", r.Reason, r.DecisionMs, r.ReleaseMs, cs.total.Load())
}

// Without a shadow stage the buggy candidate reaches clients, but only the
// canary fraction, and only until fail-fast trips.
func TestBuggyCandidateCanaryOnlyBoundedImpact(t *testing.T) {
	v1 := &impl{name: "v1"}
	v3 := &impl{name: "v3", bug: func(x int) (bool, error) {
		if x%3 == 0 {
			return false, errors.New("v3 internal error")
		}
		return false, nil
	}}
	h := newHarness(v1)
	stop := make(chan struct{})
	var cs clientStats
	wg := h.load(8, stop, &cs, true)
	time.Sleep(30 * time.Millisecond)
	s := testStrategy()
	s.Stages = s.Stages[2:] // canary-10 -> canary-50 -> promote
	if err := h.e.Start(v3.version(), s); err != nil {
		t.Fatal(err)
	}
	r := waitRollout(t, h.e)
	// Keep the load running after the rollback: no further errors may occur.
	errsAtDecision := cs.errs.Load()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	if r.Phase != PhaseRolledBack || len(r.Stages) != 1 {
		t.Fatalf("want rollback in first canary stage, got %s %+v", r.Phase, r.Stages)
	}
	servedByV3 := cs.byVersion["v3"]
	if cs.errs.Load() == 0 || cs.errs.Load() > servedByV3 {
		t.Fatalf("errors %d must be >0 and <= requests served by v3 %d", cs.errs.Load(), servedByV3)
	}
	if cs.errs.Load() != errsAtDecision {
		t.Fatalf("errors after rollback: %d -> %d", errsAtDecision, cs.errs.Load())
	}
	frac := float64(servedByV3) / float64(cs.total.Load())
	if frac > 0.10+0.05 {
		t.Fatalf("candidate share %.3f exceeds canary weight", frac)
	}
	t.Logf("client errors %d of %d (%.2f%%), v3 share %.2f%%, %s", cs.errs.Load(), cs.total.Load(),
		100*float64(cs.errs.Load())/float64(cs.total.Load()), 100*frac, r.Reason)
}

func TestLatencyRegressionRolledBack(t *testing.T) {
	v1 := &impl{name: "v1"}
	slow := &impl{name: "v2", delay: 15 * time.Millisecond}
	h := newHarness(v1)
	stop := make(chan struct{})
	var cs clientStats
	wg := h.load(4, stop, &cs, false)
	s := testStrategy()
	s.SLO.FailFastSamples = 0
	if err := h.e.Start(slow.version(), s); err != nil {
		t.Fatal(err)
	}
	r := waitRollout(t, h.e)
	close(stop)
	wg.Wait()
	if r.Phase != PhaseRolledBack || !strings.Contains(r.Reason, "p99") {
		t.Fatalf("want p99 rollback, got %s %q", r.Phase, r.Reason)
	}
	if cs.byVersion["v2"] != 0 {
		t.Fatalf("slow candidate served %d client requests in shadow", cs.byVersion["v2"])
	}
}

func TestAbortAndPanicIsolation(t *testing.T) {
	v1 := &impl{name: "v1"}
	v2 := &impl{name: "v2", panics: true}
	h := newHarness(v1)
	s := testStrategy()
	s.SLO.FailFastSamples = 0
	s.Stages[0].MinSamples = 1 << 40 // never passes on its own
	if err := h.e.Start(v2.version(), s); err != nil {
		t.Fatal(err)
	}
	waitShadowPlan(t, h.e)
	for i := 0; i < 200; i++ {
		r := h.e.Serve(context.Background(), "", in{X: i}, nil)
		if r.Err != nil || r.Version != "v1" {
			t.Fatalf("shadow must be invisible: %+v", r)
		}
	}
	h.e.WaitShadows()
	st := h.e.Status()
	if st.Window["shadow"].Errors == 0 {
		t.Fatalf("panics should be recorded as shadow errors: %+v", st.Window["shadow"])
	}
	if !h.e.Abort("operator request") {
		t.Fatal("abort refused")
	}
	r := waitRollout(t, h.e)
	if r.Phase != PhaseAborted || !v2.closed.Load() {
		t.Fatalf("abort: %+v closed=%v", r, v2.closed.Load())
	}
	if h.e.Abort("again") {
		t.Fatal("abort of finished rollout should be refused")
	}
}

// Start publishes the first stage asynchronously; wait until it is live.
func waitShadowPlan(t *testing.T, e *Engine[in, out]) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if p := e.reg.Current().Impl; p.cand != nil && p.mode == ModeShadow {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("shadow plan never published")
}

func TestStickyKeyRouting(t *testing.T) {
	p := &plan[in, out]{weightPPM: 100_000}
	hits := 0
	for i := 0; i < 10000; i++ {
		k := fmt.Sprintf("k%d", i)
		a := p.pick(k)
		for j := 0; j < 3; j++ {
			if p.pick(k) != a {
				t.Fatal("keyed routing must be deterministic")
			}
		}
		if a {
			hits++
		}
	}
	if hits < 800 || hits > 1200 {
		t.Fatalf("10%% weight gave %d/10000", hits)
	}
}

func TestShadowConcurrencyLimitDrops(t *testing.T) {
	v1 := &impl{name: "v1"}
	v2 := &impl{name: "v2", delay: 50 * time.Millisecond}
	h := &harness{}
	h.e = New(Config[in, out]{ShadowConcurrency: 2, Equal: func(a, b out) bool { return a.Result == b.Result }}, v1.version())
	s := testStrategy()
	s.Stages[0].MinSamples = 1 << 40
	h.e.Start(v2.version(), s)
	waitShadowPlan(t, h.e)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if r := h.e.Serve(context.Background(), "", in{X: i}, nil); r.Err != nil {
				t.Error(r.Err)
			}
		}(i)
	}
	wg.Wait()
	h.e.WaitShadows()
	sh := h.e.Status().Window["shadow"]
	if sh.Dropped == 0 || sh.Count > 50-sh.Dropped {
		t.Fatalf("expected dropped mirrors: %+v", sh)
	}
	h.e.Abort("done")
	waitRollout(t, h.e)
}

func TestReplaceAndClose(t *testing.T) {
	v1, v2 := &impl{name: "v1"}, &impl{name: "v2"}
	h := newHarness(v1)
	if err := h.e.Replace(v2.version()); err != nil {
		t.Fatal(err)
	}
	if h.e.Stable() != "v2" || !v1.closed.Load() {
		t.Fatalf("replace: stable %s v1 closed %v", h.e.Stable(), v1.closed.Load())
	}
	h.e.Close()
	if !v2.closed.Load() {
		t.Fatal("Close should unload stable")
	}
}

func TestAnalyze(t *testing.T) {
	st := Stage{Name: "s", Mode: ModeShadow, MinSamples: 100, MinDuration: time.Second, MaxDuration: 10 * time.Second}
	slo := DefaultSLO()
	stable := Snapshot{Count: 1000, P99Ms: 1}
	cases := []struct {
		name    string
		cand    Snapshot
		elapsed time.Duration
		want    Verdict
	}{
		{"too early", Snapshot{Count: 50, P99Ms: 1}, 2 * time.Second, Continue},
		{"too short", Snapshot{Count: 500, P99Ms: 1}, 500 * time.Millisecond, Continue},
		{"pass", Snapshot{Count: 500, P99Ms: 1.2}, 2 * time.Second, Pass},
		{"p99 within slack", Snapshot{Count: 500, P99Ms: 2.9}, 2 * time.Second, Pass},
		{"p99 too slow", Snapshot{Count: 500, P99Ms: 3.5}, 2 * time.Second, Fail},
		{"errors fail-fast", Snapshot{Count: 30, Errors: 5, ErrorRate: 5.0 / 30}, 100 * time.Millisecond, Fail},
		{"mismatch fail-fast", Snapshot{Count: 30, Mismatches: 3, MismatchRate: 0.1}, 100 * time.Millisecond, Fail},
		{"insufficient data", Snapshot{Count: 10}, 11 * time.Second, Fail},
	}
	for _, c := range cases {
		if d := Analyze(st, slo, stable, c.cand, c.elapsed); d.Verdict != c.want {
			t.Errorf("%s: got %v (%s), want %v", c.name, d.Verdict, d.Reason, c.want)
		}
	}
	// Mismatch is not judged in canary stages.
	st.Mode = ModeCanary
	if d := Analyze(st, slo, stable, Snapshot{Count: 500, P99Ms: 1, MismatchRate: 0.5}, 2*time.Second); d.Verdict != Pass {
		t.Errorf("canary stage judged mismatch: %v", d)
	}
}

func TestHistogramQuantiles(t *testing.T) {
	var h Histogram
	for i := 1; i <= 1000; i++ {
		h.Record(time.Duration(i) * time.Microsecond * 10)
	}
	p50, p99 := h.Quantile(0.5), h.Quantile(0.99)
	if p50 < 4500*time.Microsecond || p50 > 5500*time.Microsecond {
		t.Fatalf("p50 %v", p50)
	}
	if p99 < 9500*time.Microsecond || p99 > 10*time.Millisecond {
		t.Fatalf("p99 %v", p99)
	}
}

func BenchmarkServeStable(b *testing.B) {
	v1 := &impl{name: "v1"}
	h := newHarness(v1)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h.e.Serve(context.Background(), "", in{X: 1}, nil)
		}
	})
}

func BenchmarkServeShadow(b *testing.B) {
	v1, v2 := &impl{name: "v1"}, &impl{name: "v2"}
	h := newHarness(v1)
	s := testStrategy()
	s.Stages[0].MinSamples = 1 << 40
	h.e.Start(v2.version(), s)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h.e.Serve(context.Background(), "", in{X: 1}, nil)
		}
	})
	b.StopTimer()
	h.e.WaitShadows()
	h.e.Abort("bench done")
	h.e.Wait(context.Background())
}
