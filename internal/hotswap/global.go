package hotswap

import (
	"context"
	"sync/atomic"
	"time"
)

// GlobalCounterSwapper reproduces the design of the original student PoC
// (atomic.Pointer to a handler plus ONE process-wide in-flight counter) so it
// can be compared against Registry in tests and benchmarks. It is
// intentionally naive and should not be used in production code.
//
// Its flaw: the counter includes requests served by the NEW version, so
// "inflight == 0" is neither necessary nor sufficient evidence that the OLD
// version is quiescent. Under continuous overlapping load it never reaches 0.
type GlobalCounterSwapper[T any] struct {
	cur      atomic.Pointer[T]
	inflight atomic.Int64
}

// NewGlobal creates a GlobalCounterSwapper.
func NewGlobal[T any](impl T) *GlobalCounterSwapper[T] {
	g := &GlobalCounterSwapper[T]{}
	g.cur.Store(&impl)
	return g
}

// Acquire increments the global counter and returns the current impl.
func (g *GlobalCounterSwapper[T]) Acquire() T {
	g.inflight.Add(1)
	return *g.cur.Load()
}

// Release decrements the global counter.
func (g *GlobalCounterSwapper[T]) Release() { g.inflight.Add(-1) }

// InFlight returns the global in-flight count.
func (g *GlobalCounterSwapper[T]) InFlight() int64 { return g.inflight.Load() }

// SwapAndWait publishes impl and then polls (as the PoC does) until the
// global counter is zero, ctx is done, or timeout elapses.
func (g *GlobalCounterSwapper[T]) SwapAndWait(ctx context.Context, impl T, poll, timeout time.Duration) error {
	g.cur.Store(&impl)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t := time.NewTicker(poll)
	defer t.Stop()
	for g.inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			return ErrDrainTimeout
		case <-t.C:
		}
	}
	return nil
}
