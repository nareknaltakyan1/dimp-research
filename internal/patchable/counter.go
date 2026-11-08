package patchable

import (
	"sync/atomic"
	"unsafe"
)

// counter is a striped in-flight counter.
//
// A single atomic int64 per version is correct but becomes a cache-line
// hotspot: every call on every core increments and decrements the same
// line. Measured on a 12-core M4 Pro, a trampoline with one shared counter
// costs ~4 ns single-threaded but ~116 ns per call with 12 goroutines
// (BenchmarkCall/parallel/*_shared). Striping restores scaling.
//
// Stripe choice: Go exposes no cheap processor or goroutine id, so the
// stripe is derived from the address of a stack variable. Every goroutine
// has its own stack, so concurrent goroutines spread over the stripes. The
// address can change if the stack is moved (growth), therefore the stripe
// index is returned to the caller and the decrement uses the SAME stripe.
//
// Correctness of the drain check is preserved: on every stripe each
// decrement is preceded by its own increment, so each stripe is >= 0 at all
// times, and an executing call keeps its stripe >= 1; hence sum()==0 (read
// after the new version was published) implies that no call that
// incremented before the publication is still running, and calls that
// increment afterwards fail the recheck and back out.
type counter struct {
	s [stripes]paddedInt64
}

const stripes = 32

type paddedInt64 struct {
	n atomic.Int64
	_ [56]byte // keep each stripe on its own 64-byte cache line
}

func stripeOf() uint32 {
	var x byte
	a := uintptr(unsafe.Pointer(&x))
	return uint32((a>>13)^(a>>17)) % stripes
}

func (c *counter) add(st uint32, d int64) { c.s[st].n.Add(d) }

func (c *counter) sum() int64 {
	var t int64
	for i := range c.s {
		t += c.s[i].n.Load()
	}
	return t
}
