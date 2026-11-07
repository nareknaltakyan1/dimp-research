package hotswap

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type impl struct {
	name    string
	counter *atomic.Uint64
	failImp bool
	failExp bool
}

func (i *impl) ExportState() (any, error) {
	if i.failExp {
		return nil, errors.New("boom")
	}
	return i.counter, nil
}

func (i *impl) ImportState(s any) error {
	if i.failImp {
		return errors.New("bad state")
	}
	c, ok := s.(*atomic.Uint64)
	if !ok {
		return errors.New("unexpected state type")
	}
	i.counter = c
	return nil
}

func TestAcquireReleaseBasic(t *testing.T) {
	r := New[*impl]("v1", &impl{name: "v1", counter: new(atomic.Uint64)})
	v := r.Acquire()
	if v.Name != "v1" || v.Refs() != 1 {
		t.Fatalf("got %s refs=%d", v.Name, v.Refs())
	}
	u, err := r.Swap("v2", &impl{name: "v2"}, SwapOptions[*impl]{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-u.Drained():
		t.Fatal("old drained while a request still holds it")
	default:
	}
	if got := r.Acquire(); got.Name != "v2" {
		t.Fatalf("new acquire got %s", got.Name)
	} else {
		got.Release()
	}
	v.Release()
	if err := u.Wait(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if r.Current().Impl.counter == nil {
		t.Fatal("state not imported")
	}
}

func TestSwapIdleDrainsImmediately(t *testing.T) {
	r := New[*impl]("v1", &impl{counter: new(atomic.Uint64)})
	u, err := r.Swap("v2", &impl{}, SwapOptions[*impl]{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-u.Drained():
	default:
		t.Fatal("idle old version should drain immediately")
	}
}

func TestStateHandoverErrorsLeaveRegistryUnchanged(t *testing.T) {
	r := New[*impl]("v1", &impl{counter: new(atomic.Uint64), failExp: true})
	if _, err := r.Swap("v2", &impl{}, SwapOptions[*impl]{}); !errors.Is(err, ErrExport) {
		t.Fatalf("want ErrExport, got %v", err)
	}
	if r.Current().Name != "v1" {
		t.Fatal("registry changed after failed export")
	}
	r2 := New[*impl]("v1", &impl{counter: new(atomic.Uint64)})
	if _, err := r2.Swap("v2", &impl{failImp: true}, SwapOptions[*impl]{}); !errors.Is(err, ErrImport) {
		t.Fatalf("want ErrImport, got %v", err)
	}
	if _, err := r2.Swap("v2", &impl{}, SwapOptions[*impl]{Probe: func(*impl) error { return errors.New("unhealthy") }}); !errors.Is(err, ErrProbe) {
		t.Fatalf("want ErrProbe, got %v", err)
	}
	if r2.Current().Name != "v1" {
		t.Fatal("registry changed after failed probe")
	}
}

func TestVerifyFailureRollsBack(t *testing.T) {
	c := new(atomic.Uint64)
	c.Store(41)
	r := New[*impl]("v1", &impl{counter: c})
	_, err := r.Swap("v2", &impl{}, SwapOptions[*impl]{Verify: func(*impl) error { return errors.New("canary failed") }})
	if !errors.Is(err, ErrVerify) {
		t.Fatalf("want ErrVerify, got %v", err)
	}
	cur := r.Current()
	if cur.Name != "v1" || cur.Impl.counter.Load() != 41 {
		t.Fatalf("expected rollback to v1 with state, got %s %d", cur.Name, cur.Impl.counter.Load())
	}
}

func TestExplicitRollback(t *testing.T) {
	r := New[*impl]("v1", &impl{counter: new(atomic.Uint64)})
	if _, err := r.Rollback(SwapOptions[*impl]{}); !errors.Is(err, ErrNoPrevious) {
		t.Fatalf("want ErrNoPrevious, got %v", err)
	}
	if _, err := r.Swap("v2", &impl{}, SwapOptions[*impl]{}); err != nil {
		t.Fatal(err)
	}
	u, err := r.Rollback(SwapOptions[*impl]{})
	if err != nil {
		t.Fatal(err)
	}
	if u.New.Name != "v1" || u.Old.Name != "v2" {
		t.Fatalf("rollback published %s (old %s)", u.New.Name, u.Old.Name)
	}
}

func TestDrainTimeout(t *testing.T) {
	r := New[*impl]("v1", &impl{counter: new(atomic.Uint64)})
	v := r.Acquire()
	u, _ := r.Swap("v2", &impl{}, SwapOptions[*impl]{})
	err := u.Wait(context.Background(), 20*time.Millisecond)
	if !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("want ErrDrainTimeout, got %v", err)
	}
	v.Release() // the straggler finishes later; drain completes in background
	if err := u.Wait(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
}

// TestNoUseAfterDrain hammers Acquire/Release while swapping repeatedly and
// checks the safety property: no request ever runs on a version whose
// Drained channel has been closed.
func TestNoUseAfterDrain(t *testing.T) {
	r := New[*impl]("v0", &impl{counter: new(atomic.Uint64)})
	var stop atomic.Bool
	var violations, served atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				v := r.Acquire()
				select {
				case <-v.Drained():
					violations.Add(1)
				default:
				}
				served.Add(1)
				v.Release()
			}
		}()
	}
	var ups []*Upgrade[*impl]
	for i := 1; i <= 500; i++ {
		u, err := r.Swap("v", &impl{}, SwapOptions[*impl]{})
		if err != nil {
			t.Fatal(err)
		}
		ups = append(ups, u)
	}
	stop.Store(true)
	wg.Wait()
	for _, u := range ups {
		if err := u.Wait(context.Background(), 2*time.Second); err != nil {
			t.Fatalf("version never drained: %v", err)
		}
	}
	if violations.Load() != 0 {
		t.Fatalf("%d requests ran on a drained version", violations.Load())
	}
	if n := len(r.InFlight()); n == 0 {
		t.Fatal("InFlight lost current version")
	}
	t.Logf("served %d requests across 500 swaps", served.Load())
}

// steadyLoad models continuous, overlapping traffic: every worker acquires
// its next request BEFORE releasing the previous one, so the process-wide
// number of in-flight requests never reaches zero, while each individual
// request is short (hold).
func steadyLoad(stop *atomic.Bool, workers int, hold time.Duration, acquire func() func()) *sync.WaitGroup {
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel := acquire()
			for !stop.Load() {
				time.Sleep(hold)
				next := acquire()
				rel()
				rel = next
			}
			rel()
		}()
	}
	return &wg
}

// TestPerVersionDrainUnderContinuousLoad is the key property: the old version
// drains within ~one request duration even though traffic never stops.
func TestPerVersionDrainUnderContinuousLoad(t *testing.T) {
	r := New[*impl]("v1", &impl{counter: new(atomic.Uint64)})
	var stop atomic.Bool
	wg := steadyLoad(&stop, 32, time.Millisecond, func() func() {
		v := r.Acquire()
		return v.Release
	})
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	u, err := r.Swap("v2", &impl{}, SwapOptions[*impl]{})
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Wait(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("per-version drain failed under load: %v", err)
	}
	d := time.Since(start)
	inflightNew := u.New.Refs()
	stop.Store(true)
	wg.Wait()
	if inflightNew == 0 {
		t.Fatal("load generator was not running on new version")
	}
	t.Logf("old version drained in %v while %d requests in flight on v2", d, inflightNew)
}

// TestGlobalCounterNeverDrainsUnderContinuousLoad is the contrasting test:
// the original PoC's global-counter wait times out under the same load.
func TestGlobalCounterNeverDrainsUnderContinuousLoad(t *testing.T) {
	g := NewGlobal[string]("v1")
	var stop atomic.Bool
	wg := steadyLoad(&stop, 32, time.Millisecond, func() func() {
		g.Acquire()
		return g.Release
	})
	time.Sleep(50 * time.Millisecond)
	err := g.SwapAndWait(context.Background(), "v2", 10*time.Millisecond, 500*time.Millisecond)
	stop.Store(true)
	wg.Wait()
	if !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("expected global-counter drain to time out under continuous load, got %v", err)
	}
	if g.InFlight() != 0 {
		t.Fatalf("global counter not balanced: %d", g.InFlight())
	}
}

// TestPerVersionInFlightAccounting shows the second flaw of a global counter:
// it cannot say WHICH version is busy. Here v1 has a long request
// in flight, but by construction we can only observe a sum (1 + v2 load).
// Per-version counters report v1=1 precisely.
func TestPerVersionInFlightAccounting(t *testing.T) {
	r := New[*impl]("v1", &impl{counter: new(atomic.Uint64)})
	long := r.Acquire()
	r.Swap("v2", &impl{}, SwapOptions[*impl]{})
	a, b := r.Acquire(), r.Acquire()
	m := r.InFlight()
	if m["v1"] != 1 || m["v2"] != 2 {
		t.Fatalf("inflight = %v", m)
	}
	long.Release()
	a.Release()
	b.Release()
	// live-set cleanup is asynchronous: poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		m := r.InFlight()
		if len(m) == 1 && m["v2"] == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after drain inflight = %v", m)
		}
		time.Sleep(time.Millisecond)
	}
}
