package hotswap

import (
	"sync/atomic"
	"testing"
)

//go:noinline
func work() {}

// Per-request overhead of the dispatch mechanisms. The "work" is an indirect
// call to an empty non-inlined function (no shared memory writes), so the
// measurement isolates the bookkeeping, including cache-line contention on
// the counters in the parallel variants.

type fnImpl struct{ f func() }

func BenchmarkPlainCall(b *testing.B) {
	f := work
	for i := 0; i < b.N; i++ {
		f()
	}
}

// Student PoC without counting: one atomic pointer load per request.
func BenchmarkAtomicPointerOnly(b *testing.B) {
	var p atomic.Pointer[fnImpl]
	p.Store(&fnImpl{f: work})
	for i := 0; i < b.N; i++ {
		p.Load().f()
	}
}

// Student PoC: atomic pointer + global in-flight counter.
func BenchmarkGlobalCounter(b *testing.B) {
	g := NewGlobal(fnImpl{f: work})
	for i := 0; i < b.N; i++ {
		h := g.Acquire()
		h.f()
		g.Release()
	}
}

// This work: per-version refcount with validation load.
func BenchmarkAcquireRelease(b *testing.B) {
	r := New("v1", fnImpl{f: work})
	for i := 0; i < b.N; i++ {
		v := r.Acquire()
		v.Impl.f()
		v.Release()
	}
}

func BenchmarkPlainCallParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		f := work
		for pb.Next() {
			f()
		}
	})
}

func BenchmarkAtomicPointerOnlyParallel(b *testing.B) {
	var p atomic.Pointer[fnImpl]
	p.Store(&fnImpl{f: work})
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p.Load().f()
		}
	})
}

func BenchmarkGlobalCounterParallel(b *testing.B) {
	g := NewGlobal(fnImpl{f: work})
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h := g.Acquire()
			h.f()
			g.Release()
		}
	})
}

func BenchmarkAcquireReleaseParallel(b *testing.B) {
	r := New("v1", fnImpl{f: work})
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			v := r.Acquire()
			v.Impl.f()
			v.Release()
		}
	})
}

// Acquire/Release while another goroutine swaps versions continuously
// (exercises the retry path).
func BenchmarkAcquireReleaseParallelWithSwaps(b *testing.B) {
	r := New("v1", fnImpl{f: work})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				r.Swap("v", fnImpl{f: work}, SwapOptions[fnImpl]{})
			}
		}
	}()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			v := r.Acquire()
			v.Impl.f()
			v.Release()
		}
	})
	close(stop)
	<-done
}
