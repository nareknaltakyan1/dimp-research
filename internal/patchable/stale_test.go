package patchable

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- regression tests for review finding M7: stale pinned contexts ----
//
// A context outlives its pin when a goroutine spawned by the request keeps
// running after the handler returned, when it is stored somewhere, or when a
// callee keeps the Call.Ctx() of a finished trampoline call. Before the fix,
// Enter trusted such a context unconditionally: it executed the versions of
// a patch set that nobody pinned any more, possibly after Txn.Wait had
// declared them quiescent (and after the set was pruned from r.live), and
// Pin re-incremented the pruned set's count where no drainer looked.

func TestStaleContextAfterUnpinUsesCurrentSet(t *testing.T) {
	r, sf, sg, _ := fixture(t)
	ctx, pin := r.Pin(context.Background())
	if tramp(sf, ctx, 0) != "v0" {
		t.Fatal("setup")
	}
	pin.Unpin()
	pin.Unpin() // idempotent: must not drive the count negative
	if n := pin.Set().Pins(); n != 0 {
		t.Fatalf("pins after double Unpin = %d", n)
	}
	tx, err := r.Apply("v2", map[string]any{"t.F": ret("v2"), "t.G": ret("v2")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Wait(context.Background(), time.Second); err != nil {
		t.Fatalf("drain must not wait for a released pin: %v", err)
	}
	// The old set is drained and pruned. A late call through the stale ctx
	// must not run v0 (declared quiescent) - it falls back to the current set.
	if got := tramp(sg, ctx, 0); got != "v2" {
		t.Fatalf("stale context executed drained code: got %s", got)
	}
	if r.FromContext(ctx) != nil {
		t.Fatal("FromContext returned a released pin's set")
	}
	if r.StaleUses() == 0 {
		t.Fatal("stale use not counted")
	}
}

func TestPinOnStaleContextDoesNotResurrectPrunedSet(t *testing.T) {
	r, sf, _, _ := fixture(t)
	ctx, pin := r.Pin(context.Background())
	old := pin.Set()
	pin.Unpin()
	tx, _ := r.Patch("t.F", ret("v2"))
	if _, err := tx.Wait(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	ctx2, pin2 := r.Pin(ctx)
	defer pin2.Unpin()
	if pin2.Set() == old || old.Pins() != 0 {
		t.Fatalf("Pin on a stale ctx re-pinned the pruned set (pins=%d)", old.Pins())
	}
	if pin2.Set() != r.Current() || tramp(sf, ctx2, 0) != "v2" {
		t.Fatal("Pin on a stale ctx must pin the current set")
	}
}

// Nested Pin on a LIVE context still shares the outer set, and the inner
// Unpin does not release the outer pin.
func TestNestedPinSharesSetAndIsIndependent(t *testing.T) {
	r, sf, _, _ := fixture(t)
	outer, op := r.Pin(context.Background())
	defer op.Unpin()
	inner, ip := r.Pin(outer)
	if ip.Set() != op.Set() || op.Set().Pins() != 2 {
		t.Fatalf("nested pin: pins=%d", op.Set().Pins())
	}
	if _, err := r.Patch("t.F", ret("v2")); err != nil {
		t.Fatal(err)
	}
	ip.Unpin()
	if tramp(sf, inner, 0) != "v2" { // inner is stale now
		t.Fatal("inner ctx after its Unpin must be unpinned")
	}
	if tramp(sf, outer, 0) != "v0" { // outer still live
		t.Fatal("outer pin lost by the inner Unpin")
	}
}

// The context returned by Call.Ctx() of a trampoline that took the pin is
// stale once that call exits (e.g. a callee stored it, or started a
// goroutine with it).
func TestEscapedCallContextIsStaleAfterExit(t *testing.T) {
	r, sf, sg, _ := fixture(t)
	var leaked context.Context
	if _, err := r.Patch("t.F", func(ctx context.Context, x int) string { leaked = ctx; return "f" }); err != nil {
		t.Fatal(err)
	}
	tramp(sf, context.Background(), 0) // unpinned entry: Enter pins for the call
	tx, _ := r.Patch("t.G", ret("g2"))
	if _, err := tx.Wait(context.Background(), time.Second); err != nil {
		t.Fatalf("finished call's pin still counted: %v", err)
	}
	if got := tramp(sg, leaked, 0); got != "g2" {
		t.Fatalf("escaped call ctx ran drained code: %s", got)
	}
}

// Stress: requests hand their context to a goroutine that keeps calling
// through it after the request unpinned; a writer applies transactions and,
// after each successful drain, marks the replaced generation as dead. No call
// may ever execute a dead generation.
func TestStressStaleContextsNeverRunDrainedCode(t *testing.T) {
	r, sf, sg, _ := fixture(t)
	var dead atomic.Int64 // highest generation declared quiescent
	var violations, staleCalls atomic.Int64
	mk := func(gen int64) fctx {
		return func(ctx context.Context, x int) string {
			if gen <= dead.Load() {
				violations.Add(1)
			}
			return ""
		}
	}
	if _, err := r.Apply("g1", map[string]any{"t.F": mk(1), "t.G": mk(1)}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ctx, pin := r.Pin(context.Background())
				tramp(sf, ctx, 0)
				escaped := make(chan struct{})
				wg.Add(1)
				go func() { // outlives the request
					defer wg.Done()
					<-escaped
					for k := 0; k < 20; k++ {
						tramp(sg, ctx, 0)
						staleCalls.Add(1)
					}
				}()
				pin.Unpin()
				close(escaped)
			}
		}()
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	if testing.Short() {
		deadline = time.Now().Add(300 * time.Millisecond)
	}
	gen := int64(1)
	for time.Now().Before(deadline) {
		gen++
		tx, err := r.Apply("g", map[string]any{"t.F": mk(gen), "t.G": mk(gen)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Wait(context.Background(), 5*time.Second)
		if err != nil && !errors.Is(err, ErrDrainTimeout) {
			t.Fatal(err)
		}
		if err == nil {
			dead.Store(gen - 1)
		}
	}
	close(stop)
	wg.Wait()
	t.Logf("generations=%d stale_calls=%d stale_fallbacks=%d violations=%d", gen, staleCalls.Load(), r.StaleUses(), violations.Load())
	if violations.Load() != 0 {
		t.Fatalf("%d calls executed a generation after it was declared quiescent", violations.Load())
	}
}

// Regression: Revert must work for nested transactions and after
// Register/SetBaseLabel republished the current set (it compared the current
// set's pointer with txn.To and returned ErrNotTop).
func TestRevertNestedAndAfterRelabel(t *testing.T) {
	r, sf, sg, _ := fixture(t)
	ctx := context.Background()
	t1, _ := r.Apply("v2", map[string]any{"t.F": ret("f2")})
	t2, _ := r.Apply("v3", map[string]any{"t.G": ret("g3")})
	if _, err := r.Revert(t1); !errors.Is(err, ErrNotTop) {
		t.Fatalf("revert of non-top must fail: %v", err)
	}
	if _, err := r.Revert(t2); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Revert(t1); err != nil {
		t.Fatalf("revert of the new top after a nested revert: %v", err)
	}
	if tramp(sf, ctx, 0) != "v0" || tramp(sg, ctx, 0) != "v0" || r.Current().Label != "v1" {
		t.Fatal("not fully reverted")
	}
	t3, _ := r.Apply("v2", map[string]any{"t.F": ret("f2")})
	r.SetBaseLabel("v2") // republishes the current set under a new pointer
	late := r.MustRegister(SlotSpec{Symbol: "t.Late", Impl: fV0})
	if _, err := r.Revert(t3); err != nil {
		t.Fatalf("revert after SetBaseLabel/Register: %v", err)
	}
	if tramp(sf, ctx, 0) != "v0" || tramp(late, ctx, 0) != "v0" {
		t.Fatal("revert lost a slot")
	}
}

// Regression (review, Low): stripes of concurrently running goroutines must
// spread over the counter cells. Uniform random placement of 12 goroutines
// on 32 stripes uses ~10.1 stripes on average; the old hash averaged ~7.
func TestStripeSpread(t *testing.T) {
	const trials, n = 20, 12
	total := 0
	for i := 0; i < trials; i++ {
		var mu sync.Mutex
		var wg sync.WaitGroup
		used := map[uint32]bool{}
		start := make(chan struct{})
		for g := 0; g < n; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				st := stripeOf()
				mu.Lock()
				used[st] = true
				mu.Unlock()
			}()
		}
		close(start)
		wg.Wait()
		total += len(used)
	}
	avg := float64(total) / trials
	t.Logf("%d goroutines use %.1f of %d stripes on average", n, avg, stripes)
	if avg < 8 {
		t.Fatalf("stripes cluster: %d goroutines use only %.1f stripes on average", n, avg)
	}
}
