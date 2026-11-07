//go:build wasip1

// Package main is the v1 WASM guest for S3. It is a WASI *reactor* (built
// with -buildmode=c-shared) exporting:
//
//	handle(count u64, pid u32, ts_ns i64) -> u64   // packed (ptr<<32 | len) of the JSON body
//	version() -> u64                              // packed (ptr<<32 | len) of the version string
//
// The guest is stateless across calls by design: the request counter (the
// service state) is owned by the host and passed in, so any number of guest
// instances can serve concurrently and the module can be replaced without a
// state copy. Build: GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o v1.wasm .
package main

import (
	"strconv"
	"unsafe"
)

// Version is the logical version string of this module.
var Version = "v1"

var (
	out  [256]byte // response buffer in linear memory; one call at a time per instance
	vbuf [32]byte
)

func pack(b []byte) uint64 {
	return uint64(uintptr(unsafe.Pointer(&b[0])))<<32 | uint64(len(b))
}

//go:wasmexport handle
func handle(count uint64, pid uint32, ts int64) uint64 {
	b := out[:0]
	b = append(b, `{"version":"`...)
	b = append(b, Version...)
	b = append(b, `","pid":`...)
	b = strconv.AppendUint(b, uint64(pid), 10)
	b = append(b, `,"count":`...)
	b = strconv.AppendUint(b, count, 10)
	b = append(b, `,"ts_ns":`...)
	b = strconv.AppendInt(b, ts, 10)
	b = append(b, '}', '\n')
	return pack(b)
}

//go:wasmexport version
func version() uint64 {
	n := copy(vbuf[:], Version)
	return pack(vbuf[:n])
}

func main() {}
