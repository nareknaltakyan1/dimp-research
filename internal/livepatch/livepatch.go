//go:build livepatch_raw

// Package livepatch redirects a compiled Go function to a replacement by
// overwriting the machine code at its entry point, inside the running process.
//
// The design ("entry-point redirection through an indirection cell") is:
//
//	amd64:  old entry:  FF 25 <rel32>   JMP QWORD PTR [RIP+rel32]   (6 bytes, one aligned 8-byte store)
//	                     rel32 points at a uint64 "slot" in this package's .bss
//	                     slot = address of the replacement
//	arm64:  old entry:  B   <tramp_i>                        (one 4-byte instruction)
//	        tramp_i:    LDR X16, #8 ; BR X16 ; .quad target  (pre-assembled in text, see asm_linux_arm64.s)
//
// Properties that follow from this layout (argued in
// docs/solutions/S6-machine-code-livepatch.md):
//
//   - The code modification is a single naturally aligned store covering only
//     the stack-bound-check instructions of the Go prologue, which the Go
//     compiler marks as an async-preemption unsafe region. With the world
//     stopped, no goroutine can be parked at a PC strictly inside that window.
//   - The old function body is never modified, so goroutines that are already
//     executing v1 finish in v1 (per-call version consistency), and new calls
//     enter v2. No frame of the old function is ever created after the patch,
//     so the old pclntab never describes code that is not there.
//   - Re-targeting an applied patch (v2 -> v3) is a pure DATA store into the
//     slot / literal; no instruction bytes change, no I-cache maintenance.
//
// Prior art: bouk/monkey (amd64 "MOVABS RDX, fv; JMP [RDX]", 12 bytes) and
// agiledragon/gomonkey (arm64 "MOVZ/MOVK X26 x4; LDR X17,[X26]; BR X17", 24
// bytes) overwrite several instructions with ordinary byte copies and no
// concurrency protocol; they target unit-test mocking, not live services.
package livepatch

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"time"
)

// Errors returned by Apply / Revert.
var (
	ErrUnsupported    = errors.New("livepatch: unsupported platform (linux/amd64 and linux/arm64 only)")
	ErrNotFunc        = errors.New("livepatch: target and replacement must be non-nil funcs")
	ErrSignature      = errors.New("livepatch: signature mismatch")
	ErrNotEntry       = errors.New("livepatch: target is not a top-level function entry (closure or method value?)")
	ErrTooSmall       = errors.New("livepatch: target function is smaller than the patch window")
	ErrPrologue       = errors.New("livepatch: unrecognised prologue (no stack-bound check); refusing to patch")
	ErrNoSTW          = errors.New("livepatch: built without stop-the-world support (build with -tags livepatch_stw -ldflags=-checklinkname=0) and Options.UnsafeNoSTW not set")
	ErrOutOfRange     = errors.New("livepatch: indirection cell out of branch range")
	ErrPoolExhausted  = errors.New("livepatch: trampoline/slot pool exhausted")
	ErrNotApplied     = errors.New("livepatch: patch not applied")
	ErrAlreadyPatched = errors.New("livepatch: target already patched (use Retarget)")
	ErrNotQuiescent   = errors.New("livepatch: target still active on a goroutine stack")
)

// Options tunes Apply/Revert.
type Options struct {
	// UnsafeNoSTW writes the patch while other goroutines keep running. This
	// is NOT architecturally safe (see the doc); it exists for ablation
	// experiments only.
	UnsafeNoSTW bool
	// Force skips the prologue recognition check.
	Force bool
}

// Stats describes the cost of the last code modification.
type Stats struct {
	STW   time.Duration // world-stopped interval (includes mprotect + store + cache sync)
	Total time.Duration // whole Apply/Revert call
}

// Patch is an applied (or reverted) entry redirection.
type Patch struct {
	Target      uintptr // entry PC of the patched function
	Replacement uintptr // current redirect target
	TargetName  string
	ReplName    string
	orig        uint64 // original bytes of the patched word (amd64: 8 bytes, arm64: low 4 bytes)
	cell        int    // index of slot (amd64) or trampoline (arm64)
	applied     bool
	opts        Options
	Last        Stats
}

// Applied reports whether the redirection is currently installed.
func (p *Patch) Applied() bool { return p.applied }

// FuncPC returns the entry PC of a func value.
func FuncPC(f any) uintptr {
	v := reflect.ValueOf(f)
	if v.Kind() != reflect.Func || v.IsNil() {
		return 0
	}
	return v.Pointer()
}

// FuncName returns the runtime symbol name of f.
func FuncName(f any) string {
	pc := FuncPC(f)
	if fn := runtime.FuncForPC(pc); fn != nil {
		return fn.Name()
	}
	return ""
}

func validate(target, repl any) (tpc, rpc uintptr, err error) {
	tv, rv := reflect.ValueOf(target), reflect.ValueOf(repl)
	if tv.Kind() != reflect.Func || rv.Kind() != reflect.Func || tv.IsNil() || rv.IsNil() {
		return 0, 0, ErrNotFunc
	}
	if tv.Type() != rv.Type() {
		return 0, 0, fmt.Errorf("%w: target %v, replacement %v", ErrSignature, tv.Type(), rv.Type())
	}
	tpc, rpc = tv.Pointer(), rv.Pointer()
	for _, pc := range []uintptr{tpc, rpc} {
		fn := runtime.FuncForPC(pc)
		if fn == nil || fn.Entry() != pc {
			return 0, 0, ErrNotEntry
		}
	}
	// The patch window must lie inside the target (else we would clobber the
	// next function). FuncForPC(last byte) must still be the target.
	if fn := runtime.FuncForPC(tpc + windowSize - 1); fn == nil || fn.Entry() != tpc {
		return 0, 0, ErrTooSmall
	}
	return tpc, rpc, nil
}

// ---------------------------------------------------------------------------
// Activeness check (Ksplice/kpatch-style "is any thread inside the function").

// ActiveGoroutines returns, for each function name, the number of goroutines
// whose stack currently contains a frame of that function. It uses
// runtime.Stack(all=true), which stops the world while it collects the
// traces, so each result is a consistent snapshot (but stale on return).
func ActiveGoroutines(names ...string) map[string]int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	out := make(map[string]int, len(names))
	keys := make([][]byte, len(names))
	for i, nm := range names {
		keys[i] = []byte("\n" + nm + "(")
		out[nm] = 0
	}
	for _, g := range bytes.Split(buf, []byte("\n\n")) {
		for i, k := range keys {
			if bytes.Contains(g, k) || bytes.HasPrefix(g, k[1:]) {
				out[names[i]]++
			}
		}
	}
	return out
}

// WaitQuiescent polls ActiveGoroutines until none of names is on any stack
// or timeout elapses. It returns the number of polls taken.
func WaitQuiescent(timeout, poll time.Duration, names ...string) (int, error) {
	deadline := time.Now().Add(timeout)
	for i := 1; ; i++ {
		act := ActiveGoroutines(names...)
		busy := []string{}
		for _, n := range names {
			if act[n] > 0 {
				busy = append(busy, fmt.Sprintf("%s=%d", n, act[n]))
			}
		}
		if len(busy) == 0 {
			return i, nil
		}
		if time.Now().After(deadline) {
			return i, fmt.Errorf("%w: %s", ErrNotQuiescent, strings.Join(busy, ","))
		}
		time.Sleep(poll)
	}
}
