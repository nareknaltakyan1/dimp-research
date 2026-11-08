package patchable

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"time"
)

// Txn is an applied patch-set transaction (From -> To).
type Txn struct {
	reg      *Registry
	From, To *PatchSet
	// Replaced are the versions that were current in From and are not in To
	// (the code that must become quiescent). Introduced are the new ones.
	Replaced, Introduced []*Version
	Symbols              []string
	Applied              time.Time
	ApplyLatency         time.Duration // validation + build + publish, under the writer lock
	PublishLatency       time.Duration // the atomic store alone
	revertOf             *Txn
}

// Check validates a group of patches without applying it and returns them
// normalised to the slots' exact function types.
func (r *Registry) Check(patches map[string]any) (map[*Slot]any, error) {
	if len(patches) == 0 {
		return nil, ErrEmpty
	}
	out := make(map[*Slot]any, len(patches))
	var errs []string
	for _, sym := range sortedKeys(patches) {
		impl := patches[sym]
		s, ok := r.Lookup(sym)
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: %v", sym, ErrUnknownSymbol))
			continue
		}
		if s.typ == nil {
			errs = append(errs, fmt.Sprintf("%s: slot not bound yet", sym))
			continue
		}
		canon, fp, err := FingerprintOf(impl)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", sym, err))
			continue
		}
		if fp != s.Fingerprint {
			errs = append(errs, fmt.Sprintf("%s: %v: slot %s %q, impl %s %q", sym, ErrSignature, s.Fingerprint, s.Signature, fp, canon))
			continue
		}
		// Exact check: same reflect type. Types that come from a plugin are
		// normally unified with the host's by the runtime; if not (distinct
		// type descriptors for an identical type), convert so that the
		// trampoline's type assertion, which compares descriptors, succeeds.
		t := reflect.TypeOf(impl)
		if t != s.typ {
			if !t.ConvertibleTo(s.typ) || t.String() != s.typ.String() {
				errs = append(errs, fmt.Sprintf("%s: %v: type %s not identical to %s", sym, ErrSignature, t, s.typ))
				continue
			}
			impl = reflect.ValueOf(impl).Convert(s.typ).Interface()
		}
		out[s] = impl
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("patch set rejected (nothing applied): %s", strings.Join(errs, "; "))
	}
	return out, nil
}

// Apply validates and publishes a group of patches as ONE new patch set.
// All patches become visible with a single atomic store, or (on any
// validation error) none do.
func (r *Registry) Apply(label string, patches map[string]any) (*Txn, error) {
	t0 := time.Now()
	checked, err := r.Check(patches)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	from := r.cur.Load()
	r.epoch++
	if label == "" {
		label = from.Label
	}
	to := &PatchSet{reg: r, Epoch: r.epoch, Label: label, versions: make([]*Version, len(r.slots))}
	for i, s := range r.slots {
		to.versions[i] = from.get(s)
	}
	txn := &Txn{reg: r, From: from, To: to}
	for s, impl := range checked {
		nv := &Version{slot: s, fn: impl, gen: to.Epoch, origin: label, name: funcName(impl)}
		txn.Replaced = append(txn.Replaced, to.versions[s.id])
		txn.Introduced = append(txn.Introduced, nv)
		txn.Symbols = append(txn.Symbols, s.Symbol)
		to.versions[s.id] = nv
	}
	tp := time.Now()
	r.cur.Store(to) // <- the linearisation point of the whole transaction
	txn.PublishLatency = time.Since(tp)
	txn.Applied = time.Now()
	txn.ApplyLatency = txn.Applied.Sub(t0)
	r.live = append(r.live, to)
	r.txns = append(r.txns, txn)
	r.pruneLocked()
	return txn, nil
}

// Patch replaces a single function (a transaction of size one). The new
// patch set keeps the current label unless label is given.
func (r *Registry) Patch(symbol string, impl any) (*Txn, error) {
	return r.Apply("", map[string]any{symbol: impl})
}

// Top returns the most recent applied (not reverted) transaction, or nil.
func (r *Registry) Top() *Txn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.txns) == 0 {
		return nil
	}
	return r.txns[len(r.txns)-1]
}

// Revert undoes txn, which must be the most recent applied (not reverted)
// transaction: it publishes a new patch set (new epoch, txn.From's label) in
// which every slot txn touched gets back its txn.From version; every other
// slot keeps its current version. The returned transaction can be waited on
// to drain the reverted code.
//
// Reverts nest: after Apply(t1), Apply(t2), Revert(t2), t1 is the top again
// and Revert(t1) is valid. (Earlier versions compared the current set's
// POINTER with txn.To, so Revert(t1) failed with ErrNotTop once t2 had been
// reverted, and so did any Revert after a Register/SetBaseLabel, which
// republish the current set under a new pointer.)
func (r *Registry) Revert(txn *Txn) (*Txn, error) {
	t0 := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.txns) == 0 || r.txns[len(r.txns)-1] != txn {
		return nil, ErrNotTop
	}
	from := r.cur.Load()
	// Defensive: while txn is the top of the stack every later change was
	// itself reverted, so the slots txn introduced must still hold its
	// versions. If not, refuse rather than silently undo someone else's patch.
	for _, nv := range txn.Introduced {
		if from.get(nv.slot) != nv {
			return nil, fmt.Errorf("%w: %s no longer runs the version this transaction installed", ErrNotTop, nv.slot.Symbol)
		}
	}
	r.epoch++
	to := &PatchSet{reg: r, Epoch: r.epoch, Label: txn.From.Label, versions: append([]*Version(nil), from.versions...)}
	for len(to.versions) < len(r.slots) {
		to.versions = append(to.versions, r.slots[len(to.versions)].v0)
	}
	rv := &Txn{reg: r, From: from, To: to, revertOf: txn}
	for _, nv := range txn.Introduced {
		s := nv.slot
		old := txn.From.get(s)
		rv.Replaced = append(rv.Replaced, to.versions[s.id])
		rv.Introduced = append(rv.Introduced, old)
		rv.Symbols = append(rv.Symbols, s.Symbol)
		to.versions[s.id] = old
	}
	tp := time.Now()
	r.cur.Store(to)
	rv.PublishLatency = time.Since(tp)
	rv.Applied = time.Now()
	rv.ApplyLatency = rv.Applied.Sub(t0)
	r.txns = r.txns[:len(r.txns)-1]
	r.live = append(r.live, to)
	r.pruneLocked()
	return rv, nil
}

// RevertSymbol publishes a set in which symbol is back at its build-time
// implementation (a single-function revert, independent of transactions).
func (r *Registry) RevertSymbol(symbol string) (*Txn, error) {
	s, ok := r.Lookup(symbol)
	if !ok {
		return nil, ErrUnknownSymbol
	}
	return r.Apply("", map[string]any{symbol: s.v0.fn})
}

// QuiescenceReport describes how a drain went.
type QuiescenceReport struct {
	Waited        time.Duration `json:"waited_ns"`
	Polls         int           `json:"polls"`
	StackScans    int           `json:"stack_scans"`
	StackScanTime time.Duration `json:"stack_scan_ns"`
	// StackHits counts scans that found old code on a goroutine stack
	// although every reference count was already zero: the cases only the
	// defence-in-depth scan catches (goroutines spawned by, or holding a
	// captured pointer to, the old implementation).
	StackHits  int      `json:"stack_hits"`
	HitSymbols []string `json:"hit_symbols,omitempty"`
}

// RefsClear reports whether the reference counts say the replaced code is
// idle: no pinned request remains on any non-current patch set, and no call
// is executing a replaced version.
func (t *Txn) RefsClear() bool {
	r := t.reg
	r.mu.Lock()
	cur := r.cur.Load()
	for _, ps := range r.live {
		if ps != cur && ps.pins.sum() != 0 {
			r.mu.Unlock()
			return false
		}
	}
	r.mu.Unlock()
	for _, v := range t.Replaced {
		if v.active.sum() != 0 && !t.stillCurrent(v) {
			return false
		}
	}
	return true
}

// stillCurrent: a replaced version may have become current again through a
// later revert; it is then legitimately executing.
func (t *Txn) stillCurrent(v *Version) bool { return t.reg.cur.Load().get(v.slot) == v }

// Wait blocks until the replaced code is quiescent or ctx is done / the
// timeout elapses. Quiescence = RefsClear() and, if stack scanning is
// enabled, no goroutine stack contains a frame of a replaced implementation.
func (t *Txn) Wait(ctx context.Context, timeout time.Duration) (QuiescenceReport, error) {
	var rep QuiescenceReport
	start := time.Now()
	deadline := start.Add(timeout)
	backoff := 20 * time.Microsecond
	// Symbols to look for. A name that is ALSO the name of a current version
	// (e.g. two closures from the same function literal, or a patch that
	// re-installs the same function) is ambiguous on a stack dump and is
	// skipped; the reference counts still cover it.
	curNames := map[string]bool{}
	cur := t.reg.cur.Load()
	for _, v := range cur.versions {
		curNames[v.name] = true
	}
	names := make([]string, 0, len(t.Replaced))
	for _, v := range t.Replaced {
		if v.name != "" && !t.stillCurrent(v) && !curNames[v.name] {
			names = append(names, v.name)
		}
	}
	for {
		rep.Polls++
		if t.RefsClear() {
			if !t.reg.stackScan.Load() || len(names) == 0 {
				break
			}
			ts := time.Now()
			hits := StackHolds(names)
			rep.StackScans++
			rep.StackScanTime += time.Since(ts)
			if len(hits) == 0 {
				break
			}
			rep.StackHits++
			rep.HitSymbols = hits
		}
		if time.Now().After(deadline) {
			rep.Waited = time.Since(start)
			return rep, ErrDrainTimeout
		}
		select {
		case <-ctx.Done():
			rep.Waited = time.Since(start)
			return rep, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 2*time.Millisecond {
			backoff *= 2
		}
	}
	rep.Waited = time.Since(start)
	t.reg.mu.Lock()
	t.reg.pruneLocked()
	t.reg.mu.Unlock()
	return rep, nil
}

// StackHolds scans all goroutine stacks (runtime.Stack(all=true), which
// stops the world for the duration of the dump) and returns the subset of
// names that appear as a frame, including closures and go-statement
// wrappers defined inside them ("pkg.F_v0.func1", "pkg.F_v0.gowrap1"). Only
// frame lines are matched, not "created by" lines: a goroutine created by
// old code but running other code is not executing old code.
//
// Limitation: the runtime elides frames of very deep stacks (more than 100
// frames, "...additional frames elided..."), so a frame buried in the middle
// of a deep recursion can be missed. The reference counts do not have this
// blind spot for calls that went through a trampoline.
func StackHolds(names []string) []string {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var hits []string
	for _, name := range names {
		if stackHas(buf, name) {
			hits = append(hits, name)
		}
	}
	return hits
}

func stackHas(dump []byte, name string) bool {
	nb := []byte(name)
	for len(dump) > 0 {
		line := dump
		if i := bytes.IndexByte(dump, '\n'); i >= 0 {
			line, dump = dump[:i], dump[i+1:]
		} else {
			dump = nil
		}
		if bytes.HasPrefix(line, nb) && len(line) > len(nb) {
			if c := line[len(nb)]; c == '(' || c == '.' {
				return true
			}
		}
	}
	return false
}
