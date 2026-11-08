//go:build wasip1

// Package main is the v2 S7 guest: a GOOD patch. Same observable business
// result as v1, different implementation (Horner-style modular arithmetic
// that never overflows for large |x|), and a new version string. Shadow
// comparison (which ignores the declared "version" divergence) must find
// zero mismatches.
package main

import (
	"strconv"
	"unsafe"
)

var Version = "v2"

var (
	out  [320]byte
	vbuf [32]byte
)

func pack(b []byte) uint64 { return uint64(uintptr(unsafe.Pointer(&b[0])))<<32 | uint64(len(b)) }

const mod = 1000003

func price(x int64) int64 {
	// Reduce first: (31*(x mod p) + 7) mod p == (31*x + 7) mod p.
	xm := x % mod
	if xm < 0 {
		xm += mod
	}
	return (31*xm + 7) % mod
}

//go:wasmexport handle
func handle(count uint64, pid uint32, ts int64, x int64) uint64 {
	b := out[:0]
	b = append(b, `{"version":"`...)
	b = append(b, Version...)
	b = append(b, `","pid":`...)
	b = strconv.AppendUint(b, uint64(pid), 10)
	b = append(b, `,"count":`...)
	b = strconv.AppendUint(b, count, 10)
	b = append(b, `,"ts_ns":`...)
	b = strconv.AppendInt(b, ts, 10)
	b = append(b, `,"x":`...)
	b = strconv.AppendInt(b, x, 10)
	b = append(b, `,"result":`...)
	b = strconv.AppendInt(b, price(x), 10)
	b = append(b, '}', '\n')
	return pack(b)
}

//go:wasmexport version
func version() uint64 {
	n := copy(vbuf[:], Version)
	return pack(vbuf[:n])
}

func main() {}
