//go:build livepatch_raw && linux && (amd64 || arm64)

package livepatch

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Supported reports whether code patching is implemented on this platform.
const Supported = true

var (
	mu       sync.Mutex // serialises all code modification
	pageSize = uintptr(os.Getpagesize())
	byTarget = map[uintptr]*Patch{}
	nextCell int

	membarrierOnce sync.Once
	membarrierOK   bool
)

const (
	membarrierCmdPrivateExpeditedSyncCore         = 1 << 5
	membarrierCmdRegisterPrivateExpeditedSyncCore = 1 << 6
)

// SyncCoreAvailable reports whether membarrier(PRIVATE_EXPEDITED_SYNC_CORE)
// could be registered. It forces every CPU currently running a thread of this
// process through a core-serialising / context-synchronising event, which is
// what the x86 SDM (cross-modifying code) and the ARM ARM (CMODX + I-cache
// maintenance) require from the *executing* cores after code is modified.
func SyncCoreAvailable() bool {
	membarrierOnce.Do(func() {
		_, _, e := syscall.RawSyscall(sysMembarrier, membarrierCmdRegisterPrivateExpeditedSyncCore, 0, 0)
		membarrierOK = e == 0
	})
	return membarrierOK
}

func syncCore() {
	if membarrierOK {
		syscall.RawSyscall(sysMembarrier, membarrierCmdPrivateExpeditedSyncCore, 0, 0)
	}
}

// mprotect via RawSyscall: the normal syscall path calls entersyscall, which
// during stop-the-world would hand our P to the GC-stop state and deadlock.
func mprotect(addr, n uintptr, prot int) syscall.Errno {
	start := addr &^ (pageSize - 1)
	end := (addr + n + pageSize - 1) &^ (pageSize - 1)
	_, _, e := syscall.RawSyscall(syscall.SYS_MPROTECT, start, end-start, uintptr(prot))
	return e
}

const (
	protRX  = syscall.PROT_READ | syscall.PROT_EXEC
	protRWX = syscall.PROT_READ | syscall.PROT_WRITE | syscall.PROT_EXEC
)

// writeText performs the single aligned store of the patch word at addr.
// It must not allocate (it may run with the world stopped).
//
//go:norace
func writeText(addr uintptr, word uint64) syscall.Errno {
	// RWX (not RW): the page may also contain code that is executing right
	// now on this very thread (or in a signal handler on another one).
	if e := mprotect(addr, wordSize, protRWX); e != 0 {
		return e
	}
	storeWord(addr, word)
	flushICache(addr, wordSize)
	return mprotect(addr, wordSize, protRX)
}

// modify stores word at addr, either with the world stopped (default) or
// concurrently (UnsafeNoSTW ablation).
func modify(p *Patch, addr uintptr, word uint64) error {
	if !p.opts.UnsafeNoSTW && !stwSupported {
		return ErrNoSTW
	}
	SyncCoreAvailable()
	var errno syscall.Errno
	t0 := time.Now()
	if p.opts.UnsafeNoSTW {
		errno = writeText(addr, word)
		syncCore()
	} else {
		w := stopTheWorld(0)
		errno = writeText(addr, word)
		// Executing cores must context-synchronise before they can run the
		// new bytes. Cores parked by STW do so on exception return from the
		// futex wait; idle/spinning Ms that never left user mode are covered
		// by the membarrier IPI.
		syncCore()
		startTheWorld(w)
	}
	p.Last.STW = time.Since(t0)
	if errno != 0 {
		return fmt.Errorf("livepatch: mprotect: %w", errno)
	}
	return nil
}

// Apply redirects every future call of target to replacement.
// target and replacement must be funcs of identical type; target must be a
// top-level, non-inlined Go function (mark it //go:noinline).
func Apply(target, replacement any, opts Options) (*Patch, error) {
	t0 := time.Now()
	tpc, rpc, err := validate(target, replacement)
	if err != nil {
		return nil, err
	}
	mu.Lock()
	defer mu.Unlock()
	p := byTarget[tpc]
	if p != nil && p.applied {
		return p, ErrAlreadyPatched
	}
	if p == nil {
		if !opts.Force && !prologueOK(tpc) {
			return nil, fmt.Errorf("%w: first bytes % x", ErrPrologue, readBytes(tpc, 8))
		}
		if nextCell >= poolSize {
			return nil, ErrPoolExhausted
		}
		p = &Patch{Target: tpc, cell: nextCell, orig: loadWord(tpc)}
		nextCell++
	}
	p.opts = opts
	p.TargetName, p.ReplName = FuncName(target), FuncName(replacement)
	// 1. Point the indirection cell at the replacement (data store; nobody
	//    executes through the cell yet).
	if err := setCell(p.cell, rpc); err != nil {
		return nil, err
	}
	// 2. Build the branch word: it depends only on the cell address.
	word, err := branchWord(tpc, p.cell, p.orig)
	if err != nil {
		return nil, err
	}
	// 3. The one code modification.
	if err := modify(p, tpc, word); err != nil {
		return nil, err
	}
	p.Replacement = rpc
	p.applied = true
	byTarget[tpc] = p
	p.Last.Total = time.Since(t0)
	return p, nil
}

// Retarget atomically switches an applied patch to a new replacement by
// storing the new address into the indirection cell. No instruction bytes
// change, so no stop-the-world and no I-cache maintenance are needed: every
// call loads either the old or the new 8-byte pointer (single-copy atomic).
func (p *Patch) Retarget(replacement any) error {
	t0 := time.Now()
	mu.Lock()
	defer mu.Unlock()
	if !p.applied {
		return ErrNotApplied
	}
	rv := FuncPC(replacement)
	if rv == 0 {
		return ErrNotFunc
	}
	if err := setCell(p.cell, rv); err != nil {
		return err
	}
	p.Replacement = rv
	p.ReplName = FuncName(replacement)
	p.Last = Stats{Total: time.Since(t0)}
	return nil
}

// RetargetChecked is Retarget with the same signature check as Apply.
func (p *Patch) RetargetChecked(target, replacement any) error {
	if _, _, err := validate(target, replacement); err != nil {
		return err
	}
	return p.Retarget(replacement)
}

// Revert restores the original entry bytes. Goroutines already running the
// replacement finish there; new calls run the original code again.
func (p *Patch) Revert() error {
	t0 := time.Now()
	mu.Lock()
	defer mu.Unlock()
	if !p.applied {
		return ErrNotApplied
	}
	if err := modify(p, p.Target, p.orig); err != nil {
		return err
	}
	p.applied = false
	p.Replacement = 0
	p.Last.Total = time.Since(t0)
	return nil
}

func loadWord(addr uintptr) uint64 { return *(*uint64)(unsafe.Pointer(addr)) }

func readBytes(addr uintptr, n int) []byte {
	b := make([]byte, n)
	copy(b, unsafe.Slice((*byte)(unsafe.Pointer(addr)), n))
	return b
}

// EntryBytes returns the first n bytes at the entry of f (for diagnostics).
func EntryBytes(f any, n int) []byte { return readBytes(FuncPC(f), n) }
