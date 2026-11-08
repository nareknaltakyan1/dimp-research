package patchable

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// ---- signature canonicalisation ----

type myInt int
type rec struct {
	A int `json:"a"`
	b string
}
type embeds struct {
	rec
	X []byte
}

func TestCanonType(t *testing.T) {
	cases := []struct {
		fn   any
		want string
	}{
		{func() {}, "func()"},
		{func(int, string) error { return nil }, "func(int, string) error"},
		{func(...int) {}, "func(...int)"},
		{func(byte, rune, uintptr, unsafe.Pointer) {}, "func(uint8, int32, uintptr, unsafe.Pointer)"},
		{func(context.Context, *rec) (n int, err error) { return }, "func(context.Context, *patchable.rec) (int, error)"},
		{func(map[string][]*myInt, [3]chan int, <-chan bool, chan<- string) {}, "func(map[string][]*patchable.myInt, [3]chan int, <-chan bool, chan<- string)"},
		{func(any, interface {
			B()
			A(int) string
		}) {}, "func(interface{}, interface{A(int) string; B()})"},
		{func(struct {
			X int `k:"v"`
			embeds
		}) {
		}, `func(struct{X int "k:\"v\""; embeds patchable.embeds embedded})`},
		{func(func(int) (int, error)) func() { return nil }, "func(func(int) (int, error)) func()"},
	}
	for _, c := range cases {
		canon, fp, err := FingerprintOf(c.fn)
		if err != nil {
			t.Fatal(err)
		}
		if canon != c.want {
			t.Errorf("canon = %q, want %q", canon, c.want)
		}
		if len(fp) != FingerprintLen {
			t.Errorf("fp %q", fp)
		}
	}
	if _, _, err := FingerprintOf(42); err == nil {
		t.Error("non-func accepted")
	}
	var nilf func()
	if _, _, err := FingerprintOf(nilf); err == nil {
		t.Error("nil func accepted")
	}
}

// ---- test fixture: three slots, trampolines written exactly as hotpatchgen emits them ----

type fctx = func(context.Context, int) string
type fplain = func(int) string

func fV0(ctx context.Context, x int) string { return "v0" }
func gV0(ctx context.Context, x int) string { return "v0" }
func hV0(x int) string                      { return "v0" }

func fixture(t testing.TB) (*Registry, *Slot, *Slot, *Slot) {
	r := NewRegistry("v1")
	sf := r.MustRegister(SlotSpec{Symbol: "t.F", Impl: fV0})
	sg := r.MustRegister(SlotSpec{Symbol: "t.G", Impl: gV0})
	sh := r.MustRegister(SlotSpec{Symbol: "t.H", Impl: hV0})
	return r, sf, sg, sh
}

func tramp(s *Slot, ctx context.Context, x int) string {
	hp := s.Enter(ctx)
	defer hp.Exit()
	return hp.Impl().(fctx)(hp.Ctx(), x)
}

func trampPlain(s *Slot, x int) string {
	hp := s.Acquire()
	defer hp.Release()
	return hp.Impl().(fplain)(x)
}

func ret(s string) fctx { return func(context.Context, int) string { return s } }

func TestRegisterMismatchPanics(t *testing.T) {
	r := NewRegistry("v1")
	if _, err := r.Register(SlotSpec{Symbol: "x", Fingerprint: "0000000000000000", Impl: hV0}); err == nil {
		t.Fatal("fingerprint mismatch not detected")
	}
	r.MustRegister(SlotSpec{Symbol: "y", Impl: hV0})
	if _, err := r.Register(SlotSpec{Symbol: "y", Impl: hV0}); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestPatchRevert(t *testing.T) {
	r, sf, _, sh := fixture(t)
	ctx := context.Background()
	if got := tramp(sf, ctx, 1); got != "v0" {
		t.Fatal(got)
	}
	tx, err := r.Patch("t.F", ret("v2"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tramp(sf, ctx, 1); got != "v2" {
		t.Fatal(got)
	}
	if _, err := r.Patch("t.H", func(x int) string { return "h2" }); err != nil {
		t.Fatal(err)
	}
	if got := trampPlain(sh, 1); got != "h2" {
		t.Fatal(got)
	}
	if _, err := r.Revert(tx); !errors.Is(err, ErrNotTop) {
		t.Fatalf("revert of non-top: %v", err)
	}
	if _, err := r.RevertSymbol("t.H"); err != nil {
		t.Fatal(err)
	}
	if got := trampPlain(sh, 1); got != "v0" {
		t.Fatal(got)
	}
}

func TestApplyAllOrNothing(t *testing.T) {
	r, sf, sg, _ := fixture(t)
	before := r.Current()
	_, err := r.Apply("v2", map[string]any{
		"t.F": ret("v2"),
		"t.G": func(context.Context, int64) string { return "bad" }, // wrong signature
	})
	if !errors.Is(err, ErrSignature) && (err == nil || !strings.Contains(err.Error(), "signature mismatch")) {
		t.Fatalf("want signature error, got %v", err)
	}
	if r.Current() != before {
		t.Fatal("rejected transaction changed the current set")
	}
	if _, err := r.Apply("v2", map[string]any{"t.F": ret("v2"), "t.nope": ret("x")}); err == nil {
		t.Fatal("unknown symbol accepted")
	}
	tx, err := r.Apply("v2", map[string]any{"t.F": ret("v2"), "t.G": ret("v2")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if tramp(sf, ctx, 0) != "v2" || tramp(sg, ctx, 0) != "v2" {
		t.Fatal("not applied")
	}
	if r.Current().Label != "v2" || r.Current().Epoch != tx.To.Epoch {
		t.Fatal("label/epoch")
	}
	rv, err := r.Revert(tx)
	if err != nil {
		t.Fatal(err)
	}
	if tramp(sf, ctx, 0) != "v0" || tramp(sg, ctx, 0) != "v0" || r.Current().Label != "v1" {
		t.Fatal("not reverted")
	}
	if _, err := rv.Wait(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
}

// A pinned request keeps seeing its epoch's versions across a transaction.
func TestPinnedRequestSeesOneSet(t *testing.T) {
	r, sf, sg, _ := fixture(t)
	ctx, pin := r.Pin(context.Background())
	defer pin.Unpin()
	a := tramp(sf, ctx, 0)
	tx, err := r.Apply("v2", map[string]any{"t.F": ret("v2"), "t.G": ret("v2")})
	if err != nil {
		t.Fatal(err)
	}
	b := tramp(sg, ctx, 0)
	if a != "v0" || b != "v0" {
		t.Fatalf("pinned request mixed: F=%s G=%s", a, b)
	}
	if got := tramp(sg, context.Background(), 0); got != "v2" {
		t.Fatalf("new request got %s", got)
	}
	// The drain must wait for the pinned request.
	if _, err := tx.Wait(context.Background(), 20*time.Millisecond); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("drain did not wait for pinned request: %v", err)
	}
	if in := r.Inflight(); in["v1"] != 1 {
		t.Fatalf("inflight %v", in)
	}
}

// Ablation: with per-call consistency the same sequence mixes versions.
func TestPerCallAblationMixes(t *testing.T) {
	r, sf, sg, _ := fixture(t)
	r.SetPerCallConsistency(true)
	ctx, pin := r.Pin(context.Background())
	defer pin.Unpin()
	a := tramp(sf, ctx, 0)
	if _, err := r.Apply("v2", map[string]any{"t.F": ret("v2"), "t.G": ret("v2")}); err != nil {
		t.Fatal(err)
	}
	b := tramp(sg, ctx, 0)
	if a != "v0" || b != "v2" {
		t.Fatalf("expected mixed versions in per-call mode, got F=%s G=%s", a, b)
	}
}

// An unpinned call through a ctx trampoline pins for the callee's duration,
// so nested calls see the same set.
func TestTrampolinePinsNestedCalls(t *testing.T) {
	r, sf, sg, _ := fixture(t)
	var inner string
	gate, release := make(chan struct{}), make(chan struct{})
	outer := func(ctx context.Context, x int) string {
		close(gate)
		<-release
		inner = tramp(sg, ctx, x) // nested call: must see the outer epoch
		return "outer"
	}
	if _, err := r.Patch("t.F", outer); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { tramp(sf, context.Background(), 0); close(done) }()
	<-gate
	if _, err := r.Patch("t.G", ret("g-new")); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if inner != "v0" {
		t.Fatalf("nested call saw %s, want the pinned epoch's v0", inner)
	}
	if nilCtx := tramp(sg, nil, 0); nilCtx != "g-new" { //nolint:staticcheck // nil ctx is tolerated
		t.Fatal(nilCtx)
	}
}

// Stress: requests pin, call F, yield, call G; a writer flips v1<->v2
// transactions. No request may observe a mixed pair. After every drain the
// replaced versions must never start executing again (increment-then-recheck).
func TestStressConsistencyAndDrain(t *testing.T) {
	r, sf, sg, sh := fixture(t)
	var drainedGen atomic.Int64 // generation tag of the last version declared drained
	var violations, mixed, requests atomic.Int64
	mk := func(tag string, gen int64) (fctx, func(int) string) {
		f := func(ctx context.Context, x int) string {
			if drainedGen.Load() == gen {
				violations.Add(1)
			}
			return tag
		}
		h := func(x int) string {
			if drainedGen.Load() == gen {
				violations.Add(1)
			}
			return tag
		}
		return f, h
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
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
				a := tramp(sf, ctx, 0)
				if requests.Add(1)%7 == 0 {
					time.Sleep(time.Microsecond)
				}
				b := tramp(sg, ctx, 0)
				trampPlain(sh, 0)
				pin.Unpin()
				if a != b {
					mixed.Add(1)
				}
			}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	if testing.Short() {
		deadline = time.Now().Add(300 * time.Millisecond)
	}
	gen := int64(0)
	var txs int
	for time.Now().Before(deadline) {
		gen++
		tag := "g" + string(rune('a'+gen%26))
		f, h := mk(tag, gen)
		tx, err := r.Apply(tag, map[string]any{"t.F": f, "t.G": f, "t.H": h})
		if err != nil {
			t.Fatal(err)
		}
		if gen > 1 {
			if _, err := tx.Wait(context.Background(), 5*time.Second); err != nil {
				t.Fatal(err)
			}
			drainedGen.Store(gen - 1) // previous generation is now declared quiescent
		}
		txs++
	}
	close(stop)
	wg.Wait()
	t.Logf("requests=%d transactions=%d mixed=%d violations=%d", requests.Load(), txs, mixed.Load(), violations.Load())
	if mixed.Load() != 0 {
		t.Fatalf("%d requests observed a mixed patch set", mixed.Load())
	}
	if violations.Load() != 0 {
		t.Fatalf("%d calls executed a version after it was declared drained", violations.Load())
	}
}

// ---- stack scan: what reference counts cannot see ----

var spawnRelease = make(chan struct{})

// oldSpawner returns immediately but leaves a goroutine running its closure.
func oldSpawner(ctx context.Context, x int) string {
	go func() { <-spawnRelease }()
	return "old"
}

var capturedRelease = make(chan struct{})

func oldBlocking(ctx context.Context, x int) string { <-capturedRelease; return "old" }

func TestStackScanCatchesWhatRefcountMisses(t *testing.T) {
	r := NewRegistry("v1")
	s := r.MustRegister(SlotSpec{Symbol: "t.S", Impl: oldSpawner})
	tramp(s, context.Background(), 0) // spawns the goroutine, returns: refcount back to 0
	tx, err := r.Patch("t.S", ret("new"))
	if err != nil {
		t.Fatal(err)
	}
	if !tx.RefsClear() {
		t.Fatal("refcounts should already be clear")
	}
	rep, err := tx.Wait(context.Background(), 30*time.Millisecond)
	if !errors.Is(err, ErrDrainTimeout) || rep.StackHits == 0 {
		t.Fatalf("stack scan did not catch the spawned closure: %v %+v", err, rep)
	}
	close(spawnRelease)
	rep, err = tx.Wait(context.Background(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("spawned-goroutine case: %+v", rep)

	// Captured implementation pointer called outside any trampoline.
	r2 := NewRegistry("v1")
	s2 := r2.MustRegister(SlotSpec{Symbol: "t.B", Impl: oldBlocking})
	v := s2.Acquire()
	captured := v.Impl().(fctx)
	v.Release()
	go captured(context.Background(), 0) // no refcount held
	time.Sleep(10 * time.Millisecond)
	tx2, err := r2.Patch("t.B", ret("new"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Wait(context.Background(), 30*time.Millisecond); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("captured pointer not detected: %v", err)
	}
	r2.SetStackScan(false)
	if _, err := tx2.Wait(context.Background(), 30*time.Millisecond); err != nil {
		t.Fatalf("refcount-only drain should (wrongly) succeed: %v", err)
	}
	close(capturedRelease)
}

func TestStackHasMatching(t *testing.T) {
	dump := []byte("goroutine 1 [running]:\nmain.F_v0(0x1)\n\t/x.go:1\ncreated by main.G_v0 in goroutine 1\nmain.H_v0.func1()\nmain.F_v01()\n")
	for name, want := range map[string]bool{"main.F_v0": true, "main.G_v0": false, "main.H_v0": true, "main.F_v": false} {
		if got := stackHas(dump, name); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
}
