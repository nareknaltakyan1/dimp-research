//go:build wasip1

// Package main is the v1 S7 guest (WASI reactor, -buildmode=c-shared).
//
//	handle(count u64, pid u32, ts_ns i64, x i64) -> u64  // packed ptr<<32|len of the JSON body
//	version() -> u64                                    // packed ptr<<32|len of the version string
//
// Business logic: result = price(x) = (31*x + 7) mod 1000003. The guest is a
// pure function of its arguments; the counter (service state) is host-owned.
package main

import (
	"strconv"
	"unsafe"
)

var Version = "v1"

var (
	out  [320]byte
	vbuf [32]byte
)

func pack(b []byte) uint64 { return uint64(uintptr(unsafe.Pointer(&b[0])))<<32 | uint64(len(b)) }

func price(x int64) int64 {
	r := (31*x + 7) % 1000003
	if r < 0 {
		r += 1000003
	}
	return r
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
