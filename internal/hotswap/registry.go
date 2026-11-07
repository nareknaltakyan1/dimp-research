// Package hotswap implements an RCU-like versioned-implementation registry
// with per-version reference counting ("epoch-based draining").
//
// A Registry publishes exactly one *current* Version at a time. Request
// handlers call Acquire to pin the current version for the duration of a
// request and Release when done. Swap atomically publishes a new version and
// retires the old one; the old version's Drained channel is closed as soon
// as its own reference count reaches zero. Unlike a single global in-flight
// counter, requests that started on the *new* version never delay the
// retirement of the old one, so draining terminates under continuous load
// (bounded by the longest request that was already in flight on the old
// version at publish time).
//
// Correctness sketch (Go memory model: all sync/atomic operations are
// sequentially consistent):
//
//	Acquire:  v := cur.Load(); v.refs.Add(1); if cur.Load() != v { undo; retry }
//	Swap:     old := cur.Swap(new); old.retired.Store(true); if old.refs.Load()==0 { drained }
//	Release:  if v.refs.Add(-1)==0 && v.retired.Load() { drained }
//
// If an acquirer's validating load observes v as current, that load precedes
// cur.Swap in the total order, therefore its refs.Add(1) also precedes the
// swapper's refs.Load and retired.Store, so the old version cannot be
// declared drained while that request runs. If the validating load observes a
// different pointer, the acquirer never uses v; the transient increment can
// at worst cause the drained signal to be raised by that acquirer's undo
// instead of by the swapper (signalling is idempotent via sync.Once).
package hotswap

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// StateExporter is optionally implemented by an implementation that owns
// state which must survive an upgrade. ExportState is called on the old
// implementation while it may still be serving requests, so it must either
// hand over a *shared handle* (e.g. a pointer to an atomic counter, giving
// ownership transfer with zero copy) or return a consistent snapshot of state
// that the old version no longer mutates.
type StateExporter interface {
	ExportState() (any, error)
}

// StateImporter is optionally implemented by an implementation that accepts
// state from its predecessor. It is called before the new implementation is
// published, so it never races with requests served by the new version.
type StateImporter interface {
	ImportState(any) error
}

// Version is one published implementation together with its reference count.
type Version[T any] struct {
	Name string
	Impl T

	seq       uint64
	refs      atomic.Int64
	retired   atomic.Bool
	drained   chan struct{}
	drainOnce sync.Once

	PublishedAt time.Time
	retiredAt   atomic.Int64 // unix nanos
	drainedAt   atomic.Int64 // unix nanos
}

func newVersion[T any](name string, impl T, seq uint64) *Version[T] {
	return &Version[T]{Name: name, Impl: impl, seq: seq, drained: make(chan struct{}), PublishedAt: time.Now()}
}

// Release drops the reference obtained by Acquire. It must be called exactly
// once per successful Acquire.
func (v *Version[T]) Release() {
	n := v.refs.Add(-1)
	if n == 0 && v.retired.Load() {
		v.signalDrained()
	} else if n < 0 {
		panic("hotswap: Release called more times than Acquire")
	}
}

func (v *Version[T]) signalDrained() {
	v.drainOnce.Do(func() {
		v.drainedAt.Store(time.Now().UnixNano())
		close(v.drained)
	})
}

// Refs returns the number of requests currently pinned to this version.
func (v *Version[T]) Refs() int64 { return v.refs.Load() }

// Retired reports whether the version has been replaced.
func (v *Version[T]) Retired() bool { return v.retired.Load() }

// Drained is closed once the version is retired and its refcount is zero.
func (v *Version[T]) Drained() <-chan struct{} { return v.drained }

// DrainedAt returns the time the version drained (zero if not yet).
func (v *Version[T]) DrainedAt() time.Time {
	if ns := v.drainedAt.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return time.Time{}
}

// Registry holds the current version and all retired-but-not-drained ones.
type Registry[T any] struct {
	cur atomic.Pointer[Version[T]]

	swapMu sync.Mutex // serialises Swap/Rollback
	seq    uint64     // guarded by swapMu
	prev   *Version[T]

	liveMu sync.Mutex
	live   map[*Version[T]]struct{} // current + draining versions
}

// New creates a registry with an initial version.
func New[T any](name string, impl T) *Registry[T] {
	r := &Registry[T]{live: make(map[*Version[T]]struct{})}
	v := newVersion(name, impl, 0)
	r.cur.Store(v)
	r.live[v] = struct{}{}
	return r
}

// Acquire pins the current version. The caller must call Release on the
// returned version when the request completes. Acquire is lock-free and
// allocation-free; it retries only if it races with a concurrent Swap.
func (r *Registry[T]) Acquire() *Version[T] {
	for {
		v := r.cur.Load()
		v.refs.Add(1)
		if r.cur.Load() == v {
			return v
		}
		// Lost a race with Swap: v is (being) retired. Undo and retry.
		v.Release()
	}
}

// Do runs fn with the current implementation pinned.
func (r *Registry[T]) Do(fn func(name string, impl T)) {
	v := r.Acquire()
	defer v.Release()
	fn(v.Name, v.Impl)
}

// Current returns the current version without pinning it (for status only).
func (r *Registry[T]) Current() *Version[T] { return r.cur.Load() }

// InFlight returns per-version in-flight request counts for the current
// version and all versions that are still draining. Versions sharing a name
// are summed.
func (r *Registry[T]) InFlight() map[string]int64 {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	m := make(map[string]int64, len(r.live))
	for v := range r.live {
		m[v.Name] += v.refs.Load()
	}
	return m
}

// Errors returned by Swap.
var (
	ErrExport       = errors.New("hotswap: state export failed")
	ErrImport       = errors.New("hotswap: state import failed")
	ErrProbe        = errors.New("hotswap: pre-publish probe failed")
	ErrVerify       = errors.New("hotswap: post-publish verification failed (rolled back)")
	ErrDrainTimeout = errors.New("hotswap: old version did not drain before timeout")
	ErrNoPrevious   = errors.New("hotswap: no previous version to roll back to")
)

// SwapOptions customise a Swap.
type SwapOptions[T any] struct {
	// Probe is run on the new implementation after state import and before
	// publication. An error aborts the swap with nothing published.
	Probe func(T) error
	// Verify is run after publication. An error triggers an automatic
	// rollback to the previous implementation (state is handed back).
	Verify func(T) error
	// OnPublished is called right after the new version becomes current.
	OnPublished func(newV, oldV *Version[T])
}

// Upgrade describes a published swap whose old version may still be draining.
type Upgrade[T any] struct {
	Old, New *Version[T]
}

// Wait blocks until the old version has drained, ctx is done, or timeout
// elapses (timeout <= 0 means no timeout). The old version keeps draining
// in the background after a timeout; it is never forcibly cut off.
func (u *Upgrade[T]) Wait(ctx context.Context, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	select {
	case <-u.Old.Drained():
		return nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: %s still has %d in-flight", ErrDrainTimeout, u.Old.Name, u.Old.Refs())
		}
		return ctx.Err()
	}
}

// Drained is closed when the old version has drained.
func (u *Upgrade[T]) Drained() <-chan struct{} { return u.Old.Drained() }

// Swap hands state from the current implementation to impl, optionally
// probes it, atomically publishes it and retires the old version. It returns
// immediately after publication; use Upgrade.Wait to wait for draining.
// If an error is returned before publication the registry is unchanged.
func (r *Registry[T]) Swap(name string, impl T, opts SwapOptions[T]) (*Upgrade[T], error) {
	r.swapMu.Lock()
	defer r.swapMu.Unlock()
	return r.swapLocked(name, impl, opts)
}

func (r *Registry[T]) swapLocked(name string, impl T, opts SwapOptions[T]) (*Upgrade[T], error) {
	old := r.cur.Load()

	// 1. State hand-over (before publication: new never serves without state).
	if exp, ok := any(old.Impl).(StateExporter); ok {
		st, err := exp.ExportState()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrExport, err)
		}
		if imp, ok := any(impl).(StateImporter); ok {
			if err := imp.ImportState(st); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrImport, err)
			}
		}
	}
	// 2. Pre-publish probe.
	if opts.Probe != nil {
		if err := opts.Probe(impl); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrProbe, err)
		}
	}
	// 3. Publish.
	u := r.publishLocked(name, impl)
	if opts.OnPublished != nil {
		opts.OnPublished(u.New, u.Old)
	}
	// 4. Post-publish verification with automatic rollback.
	if opts.Verify != nil {
		if err := opts.Verify(impl); err != nil {
			if _, rbErr := r.swapLocked(old.Name, old.Impl, SwapOptions[T]{}); rbErr != nil {
				return u, fmt.Errorf("%w: %v; rollback also failed: %v", ErrVerify, err, rbErr)
			}
			return u, fmt.Errorf("%w: %v", ErrVerify, err)
		}
	}
	return u, nil
}

func (r *Registry[T]) publishLocked(name string, impl T) *Upgrade[T] {
	r.seq++
	nv := newVersion(name, impl, r.seq)
	r.liveMu.Lock()
	r.live[nv] = struct{}{}
	r.liveMu.Unlock()

	old := r.cur.Swap(nv) // linearisation point of the upgrade
	old.retiredAt.Store(time.Now().UnixNano())
	old.retired.Store(true)
	if old.refs.Load() == 0 {
		old.signalDrained()
	}
	r.prev = old
	go func() {
		<-old.Drained()
		r.liveMu.Lock()
		delete(r.live, old)
		r.liveMu.Unlock()
	}()
	return &Upgrade[T]{Old: old, New: nv}
}

// Rollback re-publishes the implementation that was current before the last
// Swap (state is handed back through the same export/import hooks).
func (r *Registry[T]) Rollback(opts SwapOptions[T]) (*Upgrade[T], error) {
	r.swapMu.Lock()
	defer r.swapMu.Unlock()
	if r.prev == nil {
		return nil, ErrNoPrevious
	}
	return r.swapLocked(r.prev.Name, r.prev.Impl, opts)
}
