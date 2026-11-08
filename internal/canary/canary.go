// Package canary implements in-process progressive delivery on top of
// internal/hotswap: two live versions of the business logic in ONE process,
// a shadow stage (stable serves, the candidate receives an asynchronous,
// side-effect-isolated mirror whose response is diffed), weighted canary
// stages, per-version metrics, and an automated analysis loop that promotes
// or rolls back.
//
// # Routing plans and draining
//
// The unit published through the hotswap.Registry is not a single
// implementation but a routing *plan*: {stable, candidate, mode, weight,
// metrics window}. Every stage transition, promotion and rollback is a
// Registry.Swap of a new plan. A request pins one plan for its whole
// lifetime (including an asynchronous shadow mirror it spawns), chooses ONE
// version from it, and therefore never observes a mix of versions
// (per-request version consistency). A version is unloaded only after every
// plan that referenced it has drained, which reuses the per-version epoch
// draining of S1 unchanged.
//
// # State and side-effect isolation
//
// The engine assumes the S3 state model: service state is owned by the host,
// and a version is a function of (input, state view). Config.Admit performs
// the host-level state transition (e.g. counter.Add(1)) exactly once per
// client request, BEFORE dispatch, and stores the resulting view in the
// input. The candidate's shadow call receives the same input value, i.e. a
// copy of the state view the stable version saw, and has no handle to
// mutate host state. Versions may optionally provide a separate Shadow
// function (e.g. using a separate instance pool or a read-only store view).
package canary

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/hotswap"
)

// Fn is one version's request function.
type Fn[In, Out any] func(ctx context.Context, in In) (Out, error)

// Version is one implementation.
type Version[In, Out any] struct {
	Name string
	// Serve handles a client-visible request.
	Serve Fn[In, Out]
	// Shadow handles a mirrored request with side effects isolated. If nil,
	// Serve is used, which is correct when Serve is a pure function of its
	// input (host-owned state passed by value through Admit).
	Shadow Fn[In, Out]
	// Close releases the implementation (e.g. unloads a WASM module). It is
	// called once, after the version is no longer reachable.
	Close func()
	// Meta is arbitrary caller data (e.g. the signed manifest).
	Meta any
}

type version[In, Out any] struct {
	*Version[In, Out]
	total    Metrics // client-visible requests, all stages
	shadow   Metrics // shadow mirrors, all stages
	inflight atomic.Int64
	calls    atomic.Int64 // calls received (client + shadow), for warm-up
	closed   atomic.Bool
}

func (v *version[In, Out]) close() {
	if v.closed.CompareAndSwap(false, true) && v.Close != nil {
		v.Close()
	}
}

// window holds the metrics of one stage.
type window struct {
	start  time.Time
	stable Metrics
	cand   Metrics // client-visible candidate requests (canary stages)
	shadow Metrics // mirrored candidate calls (shadow stages)
}

// plan is the unit published through the hotswap registry.
type plan[In, Out any] struct {
	stable    *version[In, Out]
	cand      *version[In, Out] // nil when no rollout is active
	mode      Mode
	weightPPM uint32 // canary weight in parts per million
	sample    float64
	warmup    int64
	stage     string
	win       *window
}

// warm reports whether a candidate call's latency should be recorded.
func (p *plan[In, Out]) warm(v *version[In, Out]) bool {
	return v.calls.Add(1) > p.warmup
}

// Config configures an Engine.
type Config[In, Out any] struct {
	// Admit applies the host-owned state transition once per client request
	// and returns the input passed to the chosen version (and, unchanged, to
	// the shadow). Optional.
	Admit func(in In) In
	// Equal compares a stable and a candidate response, ignoring declared
	// expected divergences (e.g. the version string). Default reflect.DeepEqual.
	Equal func(stable, cand Out) bool
	// ShadowConcurrency bounds in-flight mirrors; excess mirrors are dropped
	// (counted), never queued, so shadowing cannot back-pressure clients.
	ShadowConcurrency int
	// ShadowTimeout bounds one mirrored call (default 2 s).
	ShadowTimeout time.Duration
	// Events receives rollout events (optional).
	Events func(ev string, kv ...any)
	// OnMismatch is called for (a sample of) shadow mismatches (optional).
	OnMismatch func(in In, stable, cand Out)
}

// Engine routes requests between the stable version and at most one
// candidate and drives rollouts.
type Engine[In, Out any] struct {
	cfg      Config[In, Out]
	reg      *hotswap.Registry[*plan[In, Out]]
	shadowCh chan struct{}

	live sync.Map // *version -> struct{}: versions not yet closed

	mu      sync.Mutex // serialises rollouts and plan publication
	rollout *Rollout   // current or last rollout
	abort   chan string
	done    chan struct{}

	shadowWG sync.WaitGroup
}

// New creates an engine serving stable.
func New[In, Out any](cfg Config[In, Out], stable *Version[In, Out]) *Engine[In, Out] {
	if cfg.ShadowConcurrency <= 0 {
		cfg.ShadowConcurrency = 64
	}
	if cfg.ShadowTimeout <= 0 {
		cfg.ShadowTimeout = 2 * time.Second
	}
	if cfg.Equal == nil {
		cfg.Equal = func(a, b Out) bool { return reflect.DeepEqual(a, b) }
	}
	sv := &version[In, Out]{Version: stable}
	p := &plan[In, Out]{stable: sv, mode: ModeCanary, stage: "stable", win: &window{start: time.Now()}}
	e := &Engine[In, Out]{
		cfg:      cfg,
		reg:      hotswap.New("stable", p),
		shadowCh: make(chan struct{}, cfg.ShadowConcurrency),
	}
	e.live.Store(sv, struct{}{})
	return e
}

func (e *Engine[In, Out]) closeVersion(v *version[In, Out]) {
	v.close()
	e.live.Delete(v)
}

func (e *Engine[In, Out]) emit(ev string, kv ...any) {
	if e.cfg.Events != nil {
		e.cfg.Events(ev, kv...)
	}
}

// Stable returns the name of the current stable version.
func (e *Engine[In, Out]) Stable() string { return e.reg.Current().Impl.stable.Name }

// StableMeta returns the Meta of the current stable version.
func (e *Engine[In, Out]) StableMeta() any { return e.reg.Current().Impl.stable.Meta }

// InFlight returns the number of client requests currently executing per
// version name (all live versions, including draining ones).
func (e *Engine[In, Out]) InFlight() map[string]int64 {
	out := map[string]int64{}
	e.live.Range(func(k, _ any) bool {
		v := k.(*version[In, Out])
		out[v.Name] += v.inflight.Load()
		return true
	})
	return out
}

// PinnedPlans returns the number of requests (and mirrors) pinning each
// routing plan, keyed by stage name.
func (e *Engine[In, Out]) PinnedPlans() map[string]int64 { return e.reg.InFlight() }

// pick reports whether the request goes to the candidate. A non-empty key is
// hashed deterministically (sticky assignment: the same key always sees the
// same version within a stage); an empty key uses a random draw.
func (p *plan[In, Out]) pick(key string) bool {
	if p.weightPPM == 0 {
		return false
	}
	var x uint32
	if key != "" {
		h := fnv.New32a()
		h.Write([]byte(key))
		x = h.Sum32() % 1_000_000
	} else {
		x = rand.Uint32N(1_000_000)
	}
	return x < p.weightPPM
}

// ErrPanic wraps a recovered panic of a version.
var ErrPanic = errors.New("canary: version panicked")

func call[In, Out any](ctx context.Context, fn Fn[In, Out], in In) (out Out, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrPanic, r)
		}
	}()
	return fn(ctx, in)
}

// Result describes how a request was served.
type Result[Out any] struct {
	Out     Out
	Version string // version that produced Out
	Err     error
}

// Serve handles one client request. pre, if non-nil, runs after the plan is
// pinned and before the version is invoked (e.g. simulated work); it is not
// included in the measured latency.
func (e *Engine[In, Out]) Serve(ctx context.Context, key string, in In, pre func(context.Context) error) Result[Out] {
	pv := e.reg.Acquire()
	p := pv.Impl
	if pre != nil {
		if err := pre(ctx); err != nil {
			pv.Release()
			return Result[Out]{Version: p.stable.Name, Err: err}
		}
	}
	if e.cfg.Admit != nil {
		in = e.cfg.Admit(in)
	}
	target, tm, wm := p.stable, &p.stable.total, &p.win.stable
	if p.cand != nil && p.mode == ModeCanary && p.pick(key) {
		target, tm, wm = p.cand, &p.cand.total, &p.win.cand
	}
	target.inflight.Add(1)
	t0 := time.Now()
	out, err := call(ctx, target.Serve, in)
	d := time.Since(t0)
	target.inflight.Add(-1)
	lat := target != p.cand || p.warm(target)
	tm.observeL(d, err, lat)
	wm.observeL(d, err, lat)

	if err == nil && p.cand != nil && p.mode == ModeShadow && (p.sample >= 1 || rand.Float64() < p.sample) {
		select {
		case e.shadowCh <- struct{}{}:
			// The mirror inherits the plan pin; it is released when the
			// mirror completes, so the candidate cannot be unloaded under it.
			e.shadowWG.Add(1)
			go e.mirror(pv, p, in, out)
			return Result[Out]{Out: out, Version: target.Name}
		default:
			p.win.shadow.Dropped.Add(1)
			p.cand.shadow.Dropped.Add(1)
		}
	}
	pv.Release()
	return Result[Out]{Out: out, Version: target.Name, Err: err}
}

func (e *Engine[In, Out]) mirror(pv *hotswap.Version[*plan[In, Out]], p *plan[In, Out], in In, stableOut Out) {
	defer e.shadowWG.Done()
	defer pv.Release()
	defer func() { <-e.shadowCh }()
	fn := p.cand.Shadow
	if fn == nil {
		fn = p.cand.Serve
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.ShadowTimeout)
	defer cancel()
	t0 := time.Now()
	out, err := call(ctx, fn, in)
	d := time.Since(t0)
	lat := p.warm(p.cand)
	p.win.shadow.observeL(d, err, lat)
	p.cand.shadow.observeL(d, err, lat)
	if err == nil && !e.cfg.Equal(stableOut, out) {
		p.win.shadow.Mismatches.Add(1)
		p.cand.shadow.Mismatches.Add(1)
		if e.cfg.OnMismatch != nil {
			e.cfg.OnMismatch(in, stableOut, out)
		}
	}
}

// WaitShadows waits for all in-flight mirrors (tests).
func (e *Engine[In, Out]) WaitShadows() { e.shadowWG.Wait() }

// ---------------------------------------------------------------------------
// Rollouts.

// Phase of a rollout.
type Phase string

const (
	PhaseIdle        Phase = "idle"
	PhaseProgressing Phase = "progressing"
	PhasePromoted    Phase = "promoted"
	PhaseRolledBack  Phase = "rolled_back"
	PhaseAborted     Phase = "aborted"
)

// StageRecord is the outcome of one stage.
type StageRecord struct {
	Stage     string    `json:"stage"`
	Mode      Mode      `json:"mode"`
	Weight    float64   `json:"weight"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended,omitempty"`
	Verdict   string    `json:"verdict,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Stable    Snapshot  `json:"stable"`
	Candidate Snapshot  `json:"candidate"`
}

// Rollout is the state of one rollout (copy returned by Status).
type Rollout struct {
	Phase      Phase         `json:"phase"`
	Stable     string        `json:"stable"`
	Candidate  string        `json:"candidate,omitempty"`
	Stage      string        `json:"stage,omitempty"`
	StageIndex int           `json:"stage_index"`
	Mode       Mode          `json:"mode,omitempty"`
	Weight     float64       `json:"weight"`
	Started    time.Time     `json:"started,omitempty"`
	Finished   time.Time     `json:"finished,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	Stages     []StageRecord `json:"stages,omitempty"`
	// DecisionMs: rollout start -> promote/rollback decision.
	DecisionMs float64 `json:"decision_ms,omitempty"`
	// ReleaseMs: decision -> old (or failed) version drained and unloaded.
	ReleaseMs float64 `json:"release_ms,omitempty"`
	// Previous is the stable version before this rollout.
	Previous string `json:"previous,omitempty"`
}

// Status is a JSON document for /admin/rollout.
type Status struct {
	Stable   string              `json:"stable"`
	Rollout  *Rollout            `json:"rollout,omitempty"`
	Window   map[string]Snapshot `json:"window"`
	Totals   map[string]Snapshot `json:"totals"`
	InFlight map[string]int64    `json:"inflight"`
}

// ErrBusy is returned when a rollout is already in progress.
var ErrBusy = errors.New("canary: rollout already in progress")

// Status returns the current status.
func (e *Engine[In, Out]) Status() Status {
	p := e.reg.Current().Impl
	st := Status{
		Stable:   p.stable.Name,
		Window:   map[string]Snapshot{"stable": p.win.stable.Snapshot()},
		Totals:   map[string]Snapshot{p.stable.Name: p.stable.total.Snapshot()},
		InFlight: e.InFlight(),
	}
	if p.cand != nil {
		st.Window["candidate"] = p.win.cand.Snapshot()
		st.Window["shadow"] = p.win.shadow.Snapshot()
		st.Totals[p.cand.Name] = p.cand.total.Snapshot()
		st.Totals[p.cand.Name+"(shadow)"] = p.cand.shadow.Snapshot()
	}
	e.mu.Lock()
	if e.rollout != nil {
		r := *e.rollout
		r.Stages = append([]StageRecord(nil), e.rollout.Stages...)
		st.Rollout = &r
	}
	e.mu.Unlock()
	return st
}

// publish swaps in a new plan; the caller holds e.mu.
func (e *Engine[In, Out]) publish(name string, p *plan[In, Out]) *hotswap.Upgrade[*plan[In, Out]] {
	u, err := e.reg.Swap(name, p, hotswap.SwapOptions[*plan[In, Out]]{})
	if err != nil {
		// Plans implement no hooks, so Swap cannot fail.
		panic(err)
	}
	return u
}

// Start begins a rollout of cand with strategy s. It returns immediately;
// use Wait or Status to follow it.
func (e *Engine[In, Out]) Start(cand *Version[In, Out], s Strategy) error {
	if err := s.normalize(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rollout != nil && e.rollout.Phase == PhaseProgressing {
		return ErrBusy
	}
	cur := e.reg.Current().Impl
	r := &Rollout{Phase: PhaseProgressing, Stable: cur.stable.Name, Candidate: cand.Name, Started: time.Now(), Previous: cur.stable.Name}
	e.rollout = r
	e.abort = make(chan string, 1)
	e.done = make(chan struct{})
	e.emit("rollout_start", "from", cur.stable.Name, "to", cand.Name, "stages", len(s.Stages))
	cv := &version[In, Out]{Version: cand}
	e.live.Store(cv, struct{}{})
	go e.run(cur.stable, cv, s, e.abort, e.done)
	return nil
}

// Abort stops the current rollout and rolls back to stable.
func (e *Engine[In, Out]) Abort(reason string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rollout == nil || e.rollout.Phase != PhaseProgressing {
		return false
	}
	select {
	case e.abort <- reason:
	default:
	}
	return true
}

// Wait blocks until the current rollout finishes (or ctx is done) and
// returns its final state.
func (e *Engine[In, Out]) Wait(ctx context.Context) (Rollout, error) {
	e.mu.Lock()
	done := e.done
	e.mu.Unlock()
	if done == nil {
		return Rollout{Phase: PhaseIdle}, nil
	}
	select {
	case <-done:
	case <-ctx.Done():
		return Rollout{}, ctx.Err()
	}
	st := e.Status()
	return *st.Rollout, nil
}

func (e *Engine[In, Out]) setStage(i int, st Stage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.rollout
	r.Stage, r.StageIndex, r.Mode, r.Weight = st.Name, i, st.Mode, st.Weight
	r.Stages = append(r.Stages, StageRecord{Stage: st.Name, Mode: st.Mode, Weight: st.Weight, Started: time.Now()})
}

func (e *Engine[In, Out]) endStage(d Decision, stable, cand Snapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.rollout
	if n := len(r.Stages); n > 0 {
		sr := &r.Stages[n-1]
		sr.Ended, sr.Verdict, sr.Reason, sr.Stable, sr.Candidate = time.Now(), d.Verdict.String(), d.Reason, stable, cand
	}
}

func (e *Engine[In, Out]) run(stable, cand *version[In, Out], s Strategy, abort <-chan string, done chan struct{}) {
	defer close(done)
	var ups []*hotswap.Upgrade[*plan[In, Out]]
	finish := func(phase Phase, reason string, keep, drop *version[In, Out]) {
		decided := time.Now()
		e.mu.Lock()
		ups = append(ups, e.publish(keep.Name, &plan[In, Out]{stable: keep, mode: ModeCanary, stage: "stable", win: &window{start: decided}}))
		r := e.rollout
		r.Phase, r.Reason, r.Stable, r.DecisionMs = phase, reason, keep.Name, ms(decided.Sub(r.Started))
		r.Mode, r.Weight = "", 0
		if phase == PhasePromoted {
			r.Weight = 1
		}
		e.mu.Unlock()
		if phase == PhasePromoted {
			e.emit("new_version_serving", "version", keep.Name)
		} else {
			e.emit("rollout_rollback", "version", drop.Name, "reason", reason, "decision_ms", ms(decided.Sub(e.rolloutStarted())))
		}
		// Unload the dropped version once every plan that referenced it has
		// drained (shadow mirrors included).
		ctx, cancel := context.WithTimeout(context.Background(), s.DrainTimeout)
		defer cancel()
		// ups[0].Old is the pre-rollout plan, ups[i].Old the plan of stage
		// i-1, the last one the final stage: together, every plan in which
		// drop was reachable.
		drained := true
		for _, u := range ups {
			if err := u.Wait(ctx, 0); err != nil {
				drained = false
				break
			}
		}
		if drained {
			e.closeVersion(drop)
			e.emit("old_version_drained", "version", drop.Name)
		} else {
			pending := append([]*hotswap.Upgrade[*plan[In, Out]](nil), ups...)
			go func() {
				for _, u := range pending {
					<-u.Drained()
				}
				e.closeVersion(drop)
				e.emit("old_version_drained", "version", drop.Name, "late", true)
			}()
		}
		e.mu.Lock()
		e.rollout.Finished = time.Now()
		e.rollout.ReleaseMs = ms(time.Since(decided))
		e.mu.Unlock()
		if phase == PhasePromoted {
			e.emit("upgrade_done", "from", drop.Name, "to", keep.Name)
		} else {
			e.emit("upgrade_failed", "err", fmt.Sprintf("%s: %s", phase, reason), "candidate", drop.Name)
		}
	}

	for i, st := range s.Stages {
		if st.Mode == ModeCanary && st.Weight >= 1 {
			e.setStage(i, st)
			e.endStage(Decision{Pass, "promoted"}, Snapshot{}, Snapshot{})
			finish(PhasePromoted, "all stages passed", cand, stable)
			return
		}
		w := &window{start: time.Now()}
		p := &plan[In, Out]{stable: stable, cand: cand, mode: st.Mode, sample: s.ShadowSample, warmup: s.SLO.WarmupSamples, stage: st.Name, win: w}
		if st.Mode == ModeCanary {
			p.weightPPM = uint32(st.Weight * 1_000_000)
		}
		e.setStage(i, st)
		e.mu.Lock()
		ups = append(ups, e.publish(st.Name, p))
		e.mu.Unlock()
		e.emit("rollout_stage", "stage", st.Name, "mode", string(st.Mode), "weight", st.Weight, "candidate", cand.Name)

		t := time.NewTicker(s.Interval)
		var d Decision
	loop:
		for {
			select {
			case reason := <-abort:
				t.Stop()
				ss, cs := e.windowSnaps(p)
				e.endStage(Decision{Fail, "aborted: " + reason}, ss, cs)
				finish(PhaseAborted, reason, stable, cand)
				return
			case <-t.C:
				ss, cs := e.windowSnaps(p)
				d = Analyze(st, s.SLO, ss, cs, time.Since(w.start))
				if d.Verdict != Continue {
					e.endStage(d, ss, cs)
					break loop
				}
			}
		}
		t.Stop()
		if d.Verdict == Fail {
			finish(PhaseRolledBack, fmt.Sprintf("stage %s: %s", st.Name, d.Reason), stable, cand)
			return
		}
	}
	// Strategy without an explicit promote stage: promote after the last.
	finish(PhasePromoted, "all stages passed", cand, stable)
}

func (e *Engine[In, Out]) rolloutStarted() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rollout.Started
}

func (e *Engine[In, Out]) windowSnaps(p *plan[In, Out]) (stable, cand Snapshot) {
	stable = p.win.stable.Snapshot()
	if p.mode == ModeShadow {
		return stable, p.win.shadow.Snapshot()
	}
	return stable, p.win.cand.Snapshot()
}

// Replace publishes v as the new stable immediately (no analysis) and
// unloads the old stable after draining. Used for rollback to a retained
// version. It fails with ErrBusy during a rollout.
func (e *Engine[In, Out]) Replace(v *Version[In, Out]) error {
	if err := e.Start(v, ImmediateStrategy()); err != nil {
		return err
	}
	_, err := e.Wait(context.Background())
	return err
}

// Close unloads the stable version (process shutdown).
func (e *Engine[In, Out]) Close() {
	e.WaitShadows()
	e.closeVersion(e.reg.Current().Impl.stable)
}
