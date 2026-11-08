package patchable

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

//go:noinline
func directImpl(ctx context.Context, x int) string {
	if x < 0 {
		return "neg"
	}
	return "v0"
}

//go:noinline
func directPlain(x int) string {
	if x < 0 {
		return "neg"
	}
	return "v0"
}

var sink string

// BenchmarkCall compares the cost of one call through each dispatch style.
// The callee is the same trivial (non-inlined) function in every case.
func BenchmarkCall(b *testing.B) {
	r := NewRegistry("v1")
	sCtx := r.MustRegister(SlotSpec{Symbol: "b.F", Impl: fctx(directImpl)})
	sPlain := r.MustRegister(SlotSpec{Symbol: "b.H", Impl: fplain(directPlain)})
	var fv fctx = directImpl
	var ap atomic.Pointer[fctx]
	ap.Store(&fv)
	bg := context.Background()
	pinned, pin := r.Pin(bg)
	defer pin.Unpin()
	var shared sharedSlot
	shared.cur.Store(&sharedVersion{fn: directPlain})

	b.Run("direct", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sink = directImpl(bg, i)
		}
	})
	b.Run("func_value", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sink = fv(bg, i)
		}
	})
	b.Run("atomic_pointer", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sink = (*ap.Load())(bg, i)
		}
	})
	b.Run("trampoline_noctx", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sink = trampPlain(sPlain, i)
		}
	})
	b.Run("trampoline_ctx_pinned", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sink = tramp(sCtx, pinned, i)
		}
	})
	b.Run("trampoline_ctx_unpinned", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			sink = tramp(sCtx, bg, i)
		}
	})
	b.Run("pin_unpin_request", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, p := r.Pin(bg)
			p.Unpin()
		}
	})
	// Parallel: all goroutines hit the SAME version counter (one cache line).
	b.Run("parallel/atomic_pointer", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			var s string
			for i := 0; pb.Next(); i++ {
				s = (*ap.Load())(bg, i)
			}
			_ = s
		})
	})
	b.Run("trampoline_noctx_shared", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sink = shared.call(i)
		}
	})
	b.Run("parallel/trampoline_noctx_shared", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			var s string
			for i := 0; pb.Next(); i++ {
				s = shared.call(i)
			}
			_ = s
		})
	})
	b.Run("parallel/trampoline_noctx", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			var s string
			for i := 0; pb.Next(); i++ {
				s = trampPlain(sPlain, i)
			}
			_ = s
		})
	})
	b.Run("parallel/trampoline_ctx_pinned", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			ctx, p := r.Pin(bg)
			defer p.Unpin()
			var s string
			for i := 0; pb.Next(); i++ {
				s = tramp(sCtx, ctx, i)
			}
			_ = s
		})
	})
}

func manySlots(b *testing.B, n int) (*Registry, []string) {
	r := NewRegistry("v1")
	syms := make([]string, n)
	for i := range syms {
		syms[i] = fmt.Sprintf("b.F%03d", i)
		r.MustRegister(SlotSpec{Symbol: syms[i], Impl: fctx(directImpl)})
	}
	return r, syms
}

// BenchmarkApply measures the latency of publishing a transaction of N
// function patches (validation + copy-on-write set + one atomic store), in a
// registry of 200 slots. The revert that restores the state is excluded
// from the custom metrics.
func BenchmarkApply(b *testing.B) {
	for _, n := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			r, syms := manySlots(b, 200)
			patches := map[string]any{}
			for _, s := range syms[:n] {
				patches[s] = ret("v2")
			}
			var apply, publish time.Duration
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tx, err := r.Apply("v2", patches)
				if err != nil {
					b.Fatal(err)
				}
				apply += tx.ApplyLatency
				publish += tx.PublishLatency
				if _, err := r.Revert(tx); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(apply.Nanoseconds())/float64(b.N), "apply-ns")
			b.ReportMetric(float64(publish.Nanoseconds())/float64(b.N), "publish-ns")
		})
	}
}

// BenchmarkStackScan measures one quiescence stack scan with G parked
// goroutines (runtime.Stack(all) stops the world while it formats).
func BenchmarkStackScan(b *testing.B) {
	for _, g := range []int{10, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("goroutines=%d", g), func(b *testing.B) {
			stop := make(chan struct{})
			for i := 0; i < g; i++ {
				go func() { <-stop }()
			}
			time.Sleep(10 * time.Millisecond)
			names := []string{"github.com/nareknaltakyan1/dimp-research/internal/patchable.nothing_v0"}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if h := StackHolds(names); len(h) != 0 {
					b.Fatal(h)
				}
			}
			b.StopTimer()
			close(stop)
		})
	}
}

// BenchmarkDrainIdle: apply + wait for quiescence when nothing is running
// (refcount check + one stack scan), the floor of an upgrade.
func BenchmarkDrainIdle(b *testing.B) {
	for _, scan := range []bool{false, true} {
		b.Run(fmt.Sprintf("stackscan=%v", scan), func(b *testing.B) {
			r, syms := manySlots(b, 10)
			r.SetStackScan(scan)
			p := map[string]any{syms[0]: ret("v2"), syms[1]: ret("v2")}
			for i := 0; i < b.N; i++ {
				tx, err := r.Apply("v2", p)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := tx.Wait(context.Background(), time.Second); err != nil {
					b.Fatal(err)
				}
				rv, _ := r.Revert(tx)
				if _, err := rv.Wait(context.Background(), time.Second); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// sharedSlot is the naive design (one atomic counter per version, as in the
// original PoC fix and in S1): the baseline that motivated striping.
type sharedVersion struct {
	fn     func(int) string
	active atomic.Int64
}
type sharedSlot struct{ cur atomic.Pointer[sharedVersion] }

func (s *sharedSlot) call(x int) string {
	var v *sharedVersion
	for {
		v = s.cur.Load()
		v.active.Add(1)
		if s.cur.Load() == v {
			break
		}
		v.active.Add(-1)
	}
	defer v.active.Add(-1)
	return v.fn(x)
}
