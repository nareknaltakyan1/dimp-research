//go:build wasip1

// Package main is the v3 S7 guest: a BUGGY patch, used to show that shadow
// validation catches it before any client sees it.
//
//   - wrong result for inputs with x % 10 == 3 (off-by-one "discount bug");
//   - ~4 ms extra latency on every call (busy wait; a WASI reactor export
//     must not block, so it spins on the WASI clock).
package main

import (
	"strconv"
	"time"
	"unsafe"
)

var Version = "v3"

var (
	out  [320]byte
	vbuf [32]byte
)

func pack(b []byte) uint64 { return uint64(uintptr(unsafe.Pointer(&b[0])))<<32 | uint64(len(b)) }

const mod = 1000003

func price(x int64) int64 {
	xm := x % mod
	if xm < 0 {
		xm += mod
	}
	r := (31*xm + 7) % mod
	if x%10 == 3 { // the bug
		r = (r + 1) % mod
	}
	return r
}

func slowPath() {
	t0 := time.Now()
	for time.Since(t0) < 4*time.Millisecond {
	}
}

//go:wasmexport handle
func handle(count uint64, pid uint32, ts int64, x int64) uint64 {
	slowPath()
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
