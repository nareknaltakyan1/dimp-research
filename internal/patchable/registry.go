// Package patchable is the run-time half of S6 "hotpatch-ready compilation".
//
// tools/hotpatchgen rewrites every selected function F at build time into
//
//	var _dimpSlot_F = dimprt.Default.MustRegister(SlotSpec{Symbol: "pkg.F", Impl: F_v0, ...})
//	func F(args) results { hp := _dimpSlot_F.Enter(ctx); defer hp.Exit(); return hp.Impl().(func(...)...)(args) }
//	func F_v0(args) results { original body }
//
// so redirecting F later is an ordinary atomic pointer store; no code page is
// ever written. This package provides the slots, the per-version in-flight
// accounting, transactional patch sets, and quiescence detection.
//
// # Data model
//
// A Registry owns N slots (one per patchable function) and one atomic
// pointer to the *current PatchSet*. A PatchSet is immutable: an epoch
// number, a label ("v1", "v2"), and a vector of *Version, one per slot. A
// Version is one implementation of one function plus an in-flight counter.
// Applying a group of patches builds a new PatchSet (copy-on-write: unchanged
// slots share their *Version with the previous set) and publishes it with a
// single atomic store. Either all patches of the group are visible or none.
//
// # Consistency
//
// Request consistency ("a request never mixes v1 of F with v2 of G") is
// obtained by pinning: Registry.Pin (or the first context-carrying
// trampoline on a call path) loads the current PatchSet once, increments its
// pin count with increment-then-recheck, and stores it in the
// context.Context. Every trampoline whose function has a context.Context
// parameter resolves its implementation from the pinned set, so all such
// calls on that request use the same epoch, however many patch sets are
// published meanwhile. This is the analogue of the Linux livepatch per-task
// consistency model, with the request instead of the task as the unit.
//
// Functions without a context parameter cannot see the pin; they get
// per-call consistency (each call resolves the then-current version, with
// increment-then-recheck on that version's counter). hotpatchgen warns about
// them.
package patchable

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
)

// SlotSpec is what generated code passes to Register for each patchable
// function.
type SlotSpec struct {
	Symbol      string // "pkg.F", "pkg.(*T).M", "pkg.T.M"
	Fingerprint string // build-time fingerprint (go/types canonicaliser)
	Signature   string // build-time canonical signature (for humans / manifests)
	Impl        any    // the build-time implementation F_v0 (receiver first for methods)
}

// Slot is one patchable function.
type Slot struct {
	reg         *Registry
	id          int
	Symbol      string
	Fingerprint string
	Signature   string
	typ         reflect.Type
	v0          *Version
}

// Version is one implementation of one slot.
type Version struct {
	slot   *Slot
	fn     any
	gen    uint64 // epoch of the patch set that introduced it (0 = build time)
	origin string // label of the patch set that introduced it
	name   string // runtime symbol of the implementation, for stack scans
	active counter
}

// Impl returns the implementation (a func value of the slot's type).
func (v *Version) Impl() any { return v.fn }

// Active returns the number of calls currently executing this version.
func (v *Version) Active() int64 { return v.active.sum() }

// Origin is the label of the patch set that introduced this version.
func (v *Version) Origin() string { return v.origin }

// FuncName is the runtime symbol name of the implementation.
func (v *Version) FuncName() string { return v.name }

// PatchSet is an immutable assignment of one Version to every slot.
type PatchSet struct {
	reg      *Registry
	Epoch    uint64
	Label    string
	versions []*Version
	pins     counter
}

func (ps *PatchSet) get(s *Slot) *Version {
	if s.id < len(ps.versions) {
		return ps.versions[s.id]
	}
	return s.v0 // slot registered after this set was built
}

// Pins returns the number of requests currently pinned to this set.
func (ps *PatchSet) Pins() int64 { return ps.pins.sum() }

// VersionOf returns the origin label of symbol's version in this set.
func (ps *PatchSet) VersionOf(symbol string) (string, bool) {
	s, ok := ps.reg.Lookup(symbol)
	if !ok {
		return "", false
	}
	return ps.get(s).origin, true
}

// Registry holds slots and the current patch set.
type Registry struct {
	mu        sync.Mutex // serialises writers (Register, Apply, Revert)
	cur       atomic.Pointer[PatchSet]
	slots     []*Slot
	bySym     map[string]*Slot
	live      []*PatchSet // sets that may still be pinned (for Inflight / drain)
	epoch     uint64
	perCall   atomic.Bool
	stackScan atomic.Bool
	txns      []*Txn // applied, not reverted (stack)
	key       *pinKey
}

// Default is the process-wide registry that generated code registers into.
var Default = NewRegistry("v1")

// NewRegistry returns an empty registry whose base patch set has label base.
func NewRegistry(base string) *Registry {
	r := &Registry{bySym: map[string]*Slot{}, key: new(pinKey)}
	ps := &PatchSet{reg: r, Label: base}
	r.cur.Store(ps)
	r.live = []*PatchSet{ps}
	r.stackScan.Store(true)
	return r
}

// SetBaseLabel renames the current patch set's label; only meaningful before
// any patch is applied (used by services to call the build "v1").
func (r *Registry) SetBaseLabel(label string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.cur.Load()
	ns := &PatchSet{reg: r, Epoch: cur.Epoch, Label: label, versions: cur.versions}
	r.cur.Store(ns)
	r.live = append(r.live, ns)
}

// SetPerCallConsistency disables request pinning (ablation): every
// trampoline resolves the current version at call time.
func (r *Registry) SetPerCallConsistency(on bool) { r.perCall.Store(on) }

// SetStackScan enables or disables the runtime.Stack quiescence check
// (enabled by default).
func (r *Registry) SetStackScan(on bool) { r.stackScan.Store(on) }

// Register adds a slot. If spec.Impl is set the slot is bound immediately
// (see Bind); generated code registers unbound slots in package variable
// initialisers and binds them in init(), to avoid initialisation cycles.
func (r *Registry) Register(spec SlotSpec) (*Slot, error) {
	r.mu.Lock()
	if _, dup := r.bySym[spec.Symbol]; dup {
		r.mu.Unlock()
		return nil, fmt.Errorf("register %s: duplicate symbol", spec.Symbol)
	}
	s := &Slot{reg: r, id: len(r.slots), Symbol: spec.Symbol, Fingerprint: spec.Fingerprint, Signature: spec.Signature}
	s.v0 = &Version{slot: s, origin: "build"}
	r.slots = append(r.slots, s)
	r.bySym[s.Symbol] = s
	cur := r.cur.Load()
	vs := make([]*Version, len(r.slots))
	copy(vs, cur.versions)
	for i := len(cur.versions); i < len(vs); i++ {
		vs[i] = r.slots[i].v0
	}
	ns := &PatchSet{reg: r, Epoch: cur.Epoch, Label: cur.Label, versions: vs}
	r.cur.Store(ns)
	r.live = append(r.live, ns)
	r.pruneLocked()
	r.mu.Unlock()
	if spec.Impl != nil {
		if err := s.Bind(spec.Impl); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Bind sets the build-time implementation (version 0) of an unbound slot.
// It verifies that the run-time (reflect) fingerprint of impl equals the
// build-time (go/types) one recorded at registration, if any. Bind must
// happen before the slot is used concurrently (generated code calls it from
// init()).
func (s *Slot) Bind(impl any) error {
	canon, fp, err := FingerprintOf(impl)
	if err != nil {
		return fmt.Errorf("bind %s: %w", s.Symbol, err)
	}
	if s.Fingerprint != "" && fp != s.Fingerprint {
		return fmt.Errorf("bind %s: build-time fingerprint %s (%s) != run-time %s (%s): canonicaliser mismatch",
			s.Symbol, s.Fingerprint, s.Signature, fp, canon)
	}
	s.reg.mu.Lock()
	defer s.reg.mu.Unlock()
	if s.typ != nil {
		return fmt.Errorf("bind %s: already bound", s.Symbol)
	}
	s.Fingerprint, s.Signature, s.typ = fp, canon, reflect.TypeOf(impl)
	s.v0.fn, s.v0.name = impl, funcName(impl)
	return nil
}

// MustBind is Bind that panics (generated init code).
func (s *Slot) MustBind(impl any) {
	if err := s.Bind(impl); err != nil {
		panic(err)
	}
}

// MustRegister is Register that panics; used by generated code at package
// initialisation.
func (r *Registry) MustRegister(spec SlotSpec) *Slot {
	s, err := r.Register(spec)
	if err != nil {
		panic(err)
	}
	return s
}

// Lookup finds a slot by symbol.
func (r *Registry) Lookup(symbol string) (*Slot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.bySym[symbol]
	return s, ok
}

// SlotInfo describes a slot for manifests and status pages.
type SlotInfo struct {
	Symbol      string `json:"symbol"`
	Fingerprint string `json:"fingerprint"`
	Signature   string `json:"signature"`
	Origin      string `json:"origin"`
	Active      int64  `json:"active"`
}

// Slots lists all slots with their current version.
func (r *Registry) Slots() []SlotInfo {
	r.mu.Lock()
	slots := append([]*Slot(nil), r.slots...)
	r.mu.Unlock()
	cur := r.cur.Load()
	out := make([]SlotInfo, len(slots))
	for i, s := range slots {
		v := cur.get(s)
		out[i] = SlotInfo{s.Symbol, s.Fingerprint, s.Signature, v.origin, v.Active()}
	}
	return out
}

// Current returns the current patch set.
func (r *Registry) Current() *PatchSet { return r.cur.Load() }

// pin pins the current patch set: increment-then-recheck, so a set that a
// drainer has observed with zero pins after it was replaced can never gain
// a pin that executes code.
func (r *Registry) pin() (*PatchSet, uint32) {
	st := stripeOf()
	for {
		ps := r.cur.Load()
		ps.pins.add(st, 1)
		if r.cur.Load() == ps {
			return ps, st
		}
		ps.pins.add(st, -1)
	}
}

// pinKey is a per-registry context key. A pointer key makes ctx.Value's key
// comparison a single word compare (a struct key costs an interface
// equality call on every trampoline).
type pinKey struct{ _ byte }

// Pinned is a request's hold on a patch set.
type Pinned struct {
	ps *PatchSet
	st uint32
}

// Set is the pinned patch set.
func (p Pinned) Set() *PatchSet { return p.ps }

// Unpin releases the pin; call exactly once when the request ends.
func (p Pinned) Unpin() { p.ps.pins.add(p.st, -1) }

// Pin pins the current patch set for a request and returns a context that
// carries it. Call Unpin when the request ends. If ctx is already pinned to
// this registry, that set is reused (and its pin count bumped), so Pin nests.
func (r *Registry) Pin(ctx context.Context) (context.Context, Pinned) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ps, ok := ctx.Value(r.key).(*PatchSet); ok {
		st := stripeOf()
		ps.pins.add(st, 1) // already held by the outer pin; cannot be drained under us
		return ctx, Pinned{ps, st}
	}
	ps, st := r.pin()
	return context.WithValue(ctx, r.key, ps), Pinned{ps, st}
}

// FromContext returns the patch set pinned in ctx, or nil.
func (r *Registry) FromContext(ctx context.Context) *PatchSet {
	if ctx == nil {
		return nil
	}
	ps, _ := ctx.Value(r.key).(*PatchSet)
	return ps
}

// Hold is the token returned by Slot.Acquire.
type Hold struct {
	v  *Version
	st uint32
}

// Impl is the implementation to call.
func (h Hold) Impl() any { return h.v.fn }

// Release ends the call.
func (h Hold) Release() { h.v.active.add(h.st, -1) }

// Version returns the version being executed.
func (h Hold) Version() *Version { return h.v }

// Acquire is the trampoline entry for functions WITHOUT a context
// parameter: per-call consistency via increment-then-recheck on the
// version's own counter. Pair with Hold.Release.
func (s *Slot) Acquire() Hold {
	r := s.reg
	st := stripeOf()
	for {
		v := r.cur.Load().get(s)
		v.active.add(st, 1)
		if r.cur.Load().get(s) == v {
			return Hold{v, st}
		}
		v.active.add(st, -1)
	}
}

// Call is the token returned by Slot.Enter. It is a value type so that the
// hot path does not allocate when the request is already pinned.
type Call struct {
	v   *Version
	ps  *PatchSet // non-nil if this call took the pin (and must release it)
	ctx context.Context
	st  uint32 // counter stripe of the version increment
	pst uint32 // counter stripe of the pin
}

// Enter is the trampoline entry for functions WITH a context parameter. If
// ctx carries a pinned patch set, the implementation is taken from it;
// otherwise the current set is pinned for the duration of this call and the
// returned context (Call.Ctx) carries it to the callee, so everything the
// callee calls through context-aware trampolines sees the same epoch.
func (s *Slot) Enter(ctx context.Context) Call {
	r := s.reg
	if ctx == nil {
		ctx = context.Background()
	}
	if r.perCall.Load() {
		h := s.Acquire()
		return Call{v: h.v, st: h.st, ctx: ctx}
	}
	st := stripeOf()
	if ps, ok := ctx.Value(r.key).(*PatchSet); ok {
		v := ps.get(s)
		v.active.add(st, 1) // set is pinned, no recheck needed
		return Call{v: v, ctx: ctx, st: st}
	}
	ps, pst := r.pin()
	v := ps.get(s)
	v.active.add(st, 1)
	return Call{v: v, ps: ps, ctx: context.WithValue(ctx, r.key, ps), st: st, pst: pst}
}

// Exit ends the call.
func (c Call) Exit() {
	c.v.active.add(c.st, -1)
	if c.ps != nil {
		c.ps.pins.add(c.pst, -1)
	}
}

// Impl is the implementation to call.
func (c Call) Impl() any { return c.v.fn }

// Ctx is the (possibly newly pinned) context to pass to the implementation.
func (c Call) Ctx() context.Context { return c.ctx }

// Version returns the version being executed.
func (c Call) Version() *Version { return c.v }

// Inflight returns, per patch-set label, the number of pinned requests.
func (r *Registry) Inflight() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int64{}
	for _, ps := range r.live {
		out[ps.Label] += ps.pins.sum()
	}
	return out
}

// pruneLocked forgets non-current sets that have no pins. A set whose pin
// count reached zero after it stopped being current can never be pinned
// again (increment-then-recheck), so forgetting it is safe.
func (r *Registry) pruneLocked() {
	cur := r.cur.Load()
	keep := r.live[:0]
	for _, ps := range r.live {
		if ps == cur || ps.pins.sum() != 0 {
			keep = append(keep, ps)
		}
	}
	for i := len(keep); i < len(r.live); i++ {
		r.live[i] = nil
	}
	r.live = keep
}

func funcName(fn any) string {
	f := runtime.FuncForPC(reflect.ValueOf(fn).Pointer())
	if f == nil {
		return ""
	}
	return f.Name()
}

// Errors.
var (
	ErrUnknownSymbol = errors.New("patchable: unknown symbol")
	ErrSignature     = errors.New("patchable: signature mismatch")
	ErrNotTop        = errors.New("patchable: transaction is not the most recent one")
	ErrDrainTimeout  = errors.New("patchable: drain timeout")
	ErrEmpty         = errors.New("patchable: empty patch set")
)

func sortedKeys[M ~map[string]V, V any](m M) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
