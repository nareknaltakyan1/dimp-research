package fixture

// Hand-written test (not generated): exercises the checked-in output of
// hotpatchgen. Compiling this package at all proves the transformation rules
// produce valid Go for every case in ../src; init() proves the go/types and
// reflect fingerprints agree for every signature.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/patchable"
)

var reg = patchable.Default

func mustApply(t *testing.T, label string, p map[string]any) *patchable.Txn {
	t.Helper()
	tx, err := reg.Apply(label, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if rv, err := reg.Revert(tx); err == nil {
			rv.Wait(context.Background(), time.Second)
		}
	})
	return tx
}

func TestRegistered(t *testing.T) {
	want := []string{"fixture.(*Counter).Add", "fixture.Counter.Label", "fixture.Sum", "fixture.Divide", "fixture.Join",
		"fixture.(*Counter).Unnamed", "fixture.Exotic", "fixture.NoResult", "fixture.Recursive", "fixture.Shadow", "fixture.Spawn"}
	for _, s := range want {
		if _, ok := reg.Lookup(s); !ok {
			t.Errorf("slot %s missing", s)
		}
	}
}

func TestBuildTimeBehaviourUnchanged(t *testing.T) {
	ctx := context.Background()
	c := &Counter{Name: "c"}
	if c.Add(ctx, 2) != 2 || c.Label() != "c=2" {
		t.Fatal("methods")
	}
	if Sum(ctx, 1, 2, 3) != 6 || Sum(ctx, 1) != 1 {
		t.Fatal("variadic")
	}
	if q, r, err := Divide(7, 2); q != 3 || r != 1 || err != nil {
		t.Fatal("named results")
	}
	if _, _, err := Divide(1, 0); err == nil {
		t.Fatal("naked return error")
	}
	if s, err := Join(ctx, []string{"a", "b"}); s != "A+B" || err != nil {
		t.Fatal("closures", s, err)
	}
	if _, err := Join(ctx, []string{"panic"}); err == nil || !strings.Contains(err.Error(), "recovered") {
		t.Fatal("recover inside moved body", err)
	}
	if Recursive(ctx, 4) != 10 || Shadow(Counter{N: 2}, 3) != 5 || !c.Unnamed(ctx, 1, "") {
		t.Fatal("misc")
	}
	NoResult(nil, c) //nolint:staticcheck // nil ctx tolerated by trampolines
	if c.N != -1 {
		t.Fatal("no result")
	}
}

func TestPatchGeneratedFunctions(t *testing.T) {
	ctx := context.Background()
	mustApply(t, "v2", map[string]any{
		"fixture.Sum": func(ctx context.Context, base int, xs ...int) int { return -len(xs) },
		"fixture.Divide": func(a, b int) (int, int, error) {
			if b == 0 {
				return 0, 0, errors.New("v2")
			}
			return a / b, 0, nil
		},
		"fixture.(*Counter).Add": func(c *Counter, ctx context.Context, d int) int { c.N += 10 * d; return c.N },
		"fixture.Counter.Label":  func(c Counter) string { return "v2:" + c.Name },
		// Recursion goes through the trampoline, so the patched version
		// is also used by the recursive calls inside Recursive_v0's callers.
		"fixture.Recursive": func(ctx context.Context, n int) int {
			if n <= 0 {
				return 100
			}
			return Recursive_v0(ctx, n) // delegate to the original body
		},
	})
	if Sum(ctx, 1, 2, 3) != -2 {
		t.Fatal("variadic patch")
	}
	if q, r, err := Divide(7, 2); q != 3 || r != 0 || err != nil {
		t.Fatal("named results patch")
	}
	c := &Counter{Name: "x"}
	if c.Add(ctx, 1) != 10 || c.Label() != "v2:x" {
		t.Fatal("method patch")
	}
	// Recursive(3) -> v2 -> Recursive_v0(3) = 3 + Recursive(2) -> ... -> Recursive(0) -> 100
	if got := Recursive(ctx, 3); got != 106 {
		t.Fatalf("recursive patch: %d", got)
	}
}

func TestWrongSignatureRejected(t *testing.T) {
	before := reg.Current()
	_, err := reg.Apply("bad", map[string]any{
		"fixture.Sum":    func(ctx context.Context, base int, xs ...int) int { return 0 },
		"fixture.Divide": func(a, b int64) (int, int, error) { return 0, 0, nil },
	})
	if !errors.Is(err, nil) && !strings.Contains(err.Error(), "signature mismatch") {
		t.Fatal(err)
	}
	if err == nil || reg.Current() != before {
		t.Fatal("partial apply")
	}
	// variadic vs slice is a different signature
	if _, err := reg.Apply("bad", map[string]any{"fixture.Sum": func(ctx context.Context, base int, xs []int) int { return 0 }}); err == nil {
		t.Fatal("slice accepted for variadic")
	}
}

func TestStackScanOnGeneratedSpawn(t *testing.T) {
	release := make(chan struct{})
	Spawn(context.Background(), release) // returns, leaves Spawn_v0.func1 running
	tx := mustApply(t, "v2", map[string]any{"fixture.Spawn": func(context.Context, chan struct{}) {}})
	rep, err := tx.Wait(context.Background(), 20*time.Millisecond)
	if !errors.Is(err, patchable.ErrDrainTimeout) || rep.StackHits == 0 {
		t.Fatalf("expected stack hit on Spawn_v0.func1: %v %+v", err, rep)
	}
	close(release)
	if _, err := tx.Wait(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
}

var sinkI int

// BenchmarkGenerated compares calling the generated trampoline with calling
// the moved body directly (what the call cost would be without
// hotpatch-readiness, minus inlining effects measured separately).
func BenchmarkGenerated(b *testing.B) {
	ctx := context.Background()
	pinned, pin := reg.Pin(ctx)
	defer pin.Unpin()
	b.Run("Divide/direct_v0", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			q, _, _ := Divide_v0(i, 3)
			sinkI += q
		}
	})
	b.Run("Divide/trampoline", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			q, _, _ := Divide(i, 3)
			sinkI += q
		}
	})
	b.Run("Sum/direct_v0", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sinkI += Sum_v0(ctx, i, 1, 2)
		}
	})
	b.Run("Sum/trampoline_unpinned", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sinkI += Sum(ctx, i, 1, 2)
		}
	})
	b.Run("Sum/trampoline_pinned", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sinkI += Sum(pinned, i, 1, 2)
		}
	})
}
