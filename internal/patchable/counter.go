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

const (
	stripeBits = 5
	stripes    = 1 << stripeBits
)

type paddedInt64 struct {
	n atomic.Int64
	_ [56]byte // keep each stripe on its own 64-byte cache line
}

// stripeOf hashes the stack address with a multiplicative (Fibonacci) hash
// and takes the top bits. Goroutine stacks start at 2 KiB (a multiple of it
// when the runtime picks a larger initial size) and are carved out of spans
// contiguously, so a>>11 of concurrently running goroutines are often
// consecutive integers. The previous ((a>>13)^(a>>17)) % 32 ignored bits 11
// and 12 (up to four 2 KiB stacks per value) and mapped neighbours onto few
// stripes: 12 fresh goroutines landed on 4 to 9 of the 32 stripes, up to 4
// per stripe. With a>>11 and the multiplicative hash they land on 11 to 12
// stripes (uniform random would give ~10; TestStripeSpread).
func stripeOf() uint32 {
	var x byte
	a := uint64(uintptr(unsafe.Pointer(&x)) >> 11)
	return uint32((a * 0x9E3779B97F4A7C15) >> (64 - stripeBits))
}

func (c *counter) add(st uint32, d int64) { c.s[st].n.Add(d) }

func (c *counter) sum() int64 {
	var t int64
	for i := range c.s {
		t += c.s[i].n.Load()
	}
	return t
}
