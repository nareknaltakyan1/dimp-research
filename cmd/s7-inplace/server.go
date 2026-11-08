package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/canary"
	"github.com/nareknaltakyan1/dimp-research/internal/canary/wasmguest"
	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/svc"
	"github.com/nareknaltakyan1/dimp-research/internal/signing"
)

// In is the per-request input handed to a version. Count and TS are the
// host-owned state view assigned once per client request (canary Admit);
// the shadow receives an identical copy.
type In struct {
	Count uint64
	TS    int64
	X     int64
}

// Out is a version's response.
type Out struct {
	Body    []byte
	Version string
}

// artifact is the code behind a version (kept for rollback).
type artifact struct {
	Name     string
	Digest   string
	Wasm     []byte
	Manifest *signing.Manifest // nil for the image's built-in module
}

// Config of the server.
type Config struct {
	Service     string
	StateSchema string
	ModuleDir   string // built-in modules (<name>.wasm), trusted via the image
	Initial     string
	BundleDir   string // pre-staged signed bundles for /admin/upgrade (<to>/)
	Verifier    *signing.Verifier
	// AdminToken is the bearer token required on every privileged endpoint
	// (/admin/patch, /admin/rollout*, /admin/rollback, /admin/upgrade).
	AdminToken string
	// AllowTokenless lets privileged endpoints run without a token. It is
	// only set by main when every listener serving them is bound to a
	// loopback address or -insecure-admin was given. With AdminToken == ""
	// and AllowTokenless == false every privileged request is refused
	// (fail closed).
	AllowTokenless bool
	// ContractUpgrade enables the harness endpoint /admin/upgrade (it can
	// only load pre-staged, signed bundles; it is authenticated like the
	// other privileged endpoints).
	ContractUpgrade bool
	// StateDir holds the persisted anti-rollback high-water mark
	// (floor.json). "" keeps it in memory only (tests, benchmarks).
	StateDir string
	// MaxBundleAge rejects bundles whose signed manifest.created is older
	// (signing.Policy.MaxAge). 0 = no limit.
	MaxBundleAge   time.Duration
	Wasm           wasmguest.Options
	MaxBundleBytes int64
	Events         *svc.EventLog
	// UpgradeStrategy is used by /admin/upgrade and SIGHUP.
	UpgradeStrategy canary.Strategy
}

// PatchRequest is the body of POST /admin/patch. Unknown fields are
// rejected (there is deliberately no "rollback" flag: rollback is
// POST /admin/rollback, which can only re-install the retained artifact).
type PatchRequest struct {
	Bundle       json.RawMessage  `json:"bundle"` // signing.Envelope
	Strategy     *canary.Strategy `json:"strategy,omitempty"`
	StrategyName string           `json:"strategy_name,omitempty"` // default | fast | immediate
	// Reconcile relaxes only the base_version == running check (re-patching
	// a fresh pod that runs image code). Monotonicity is always enforced.
	Reconcile bool `json:"reconcile,omitempty"`
}

// PatchRecord is one admission decision (kept for /admin/rollout).
type PatchRecord struct {
	Time      time.Time `json:"time"`
	Version   string    `json:"version,omitempty"`
	Digest    string    `json:"digest,omitempty"`
	Accepted  bool      `json:"accepted"`
	Error     string    `json:"error,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	VerifyMs  float64   `json:"verify_ms"`
	CompileMs float64   `json:"compile_ms"`
	Source    string    `json:"source"`
}

// Server is the S7 service.
type Server struct {
	cfg      Config
	log      *svc.EventLog
	pid      uint32
	instance string // hostname (pod name), lets clients attribute state per pod
	counter  atomic.Uint64
	eng      *canary.Engine[In, Out]

	// mu guards current, previous, floor, pending and patches. It is held
	// across the final admission check and Engine.StartFrom, and taken by
	// the engine's OnDecision hook, so admission decisions, retained
	// artifacts and the high-water mark change atomically with rollout
	// phase transitions.
	mu        sync.Mutex
	current   *artifact // stable
	previous  *artifact // retained for /admin/rollback (the ONLY rollback target)
	floor     signing.Floor
	pending   *pendingRollout
	patches   []PatchRecord
	mismatchN atomic.Int64
}

// pendingRollout describes the rollout currently owned by the engine.
type pendingRollout struct {
	cand     *artifact
	rollback bool // re-install of s.previous via /admin/rollback
}

const floorFile = "floor.json"

// NewServer loads the initial built-in module.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Events == nil {
		cfg.Events = svc.NewEventLog(os.Stderr)
	}
	if cfg.MaxBundleBytes <= 0 {
		cfg.MaxBundleBytes = 8 << 20
	}
	if cfg.Verifier == nil {
		cfg.Verifier = signing.NewVerifier()
	}
	if len(cfg.UpgradeStrategy.Stages) == 0 {
		cfg.UpgradeStrategy = canary.FastStrategy()
	}
	host, _ := os.Hostname()
	s := &Server{cfg: cfg, log: cfg.Events, pid: uint32(os.Getpid()), instance: host}
	wasm, err := os.ReadFile(filepath.Join(cfg.ModuleDir, cfg.Initial+".wasm"))
	if err != nil {
		return nil, err
	}
	art := &artifact{Name: cfg.Initial, Digest: signing.Digest(wasm), Wasm: wasm}
	if err := s.loadFloor(art); err != nil {
		return nil, err
	}
	v, _, err := s.load(art)
	if err != nil {
		return nil, err
	}
	s.current = art
	s.eng = canary.New(canary.Config[In, Out]{
		OnDecision: s.onDecision,
		Admit: func(in In) In {
			in.Count = s.counter.Add(1)
			in.TS = time.Now().UnixNano()
			return in
		},
		Equal:  equalIgnoringVersion,
		Events: s.log.Emit,
		OnMismatch: func(in In, a, b Out) {
			if s.mismatchN.Add(1) <= 5 {
				s.log.Emit("shadow_mismatch", "x", in.X, "stable", strings.TrimSpace(string(a.Body)), "candidate", strings.TrimSpace(string(b.Body)))
			}
		},
	}, v)
	return s, nil
}

// equalIgnoringVersion compares two JSON bodies ignoring the declared
// expected divergence "version". Count, ts_ns and pid are equal by
// construction (the shadow receives the same state view).
func equalIgnoringVersion(a, b Out) bool {
	var ma, mb map[string]any
	if json.Unmarshal(a.Body, &ma) != nil || json.Unmarshal(b.Body, &mb) != nil {
		return bytes.Equal(a.Body, b.Body)
	}
	delete(ma, "version")
	delete(mb, "version")
	return reflect.DeepEqual(ma, mb)
}

// load compiles an artifact into a canary.Version.
func (s *Server) load(a *artifact) (*canary.Version[In, Out], time.Duration, error) {
	t0 := time.Now()
	m, err := wasmguest.Load(context.Background(), a.Name, a.Wasm, s.cfg.Wasm)
	if err != nil {
		return nil, 0, err
	}
	if m.Version != a.Name {
		m.Close()
		return nil, 0, fmt.Errorf("module reports version %q, manifest says %q", m.Version, a.Name)
	}
	pid := s.pid
	return &canary.Version[In, Out]{
		Name: a.Name,
		Serve: func(ctx context.Context, in In) (Out, error) {
			b, err := m.Call(ctx, in.Count, pid, in.TS, in.X)
			return Out{Body: b, Version: m.Version}, err
		},
		Shadow: func(ctx context.Context, in In) (Out, error) {
			b, err := m.CallShadow(ctx, in.Count, pid, in.TS, in.X)
			return Out{Body: b, Version: m.Version}, err
		},
		Close: func() { m.Close() },
		Meta:  a,
	}, time.Since(t0), nil
}

// loadFloor reads the persisted high-water mark (fail closed on a corrupt
// file) and raises it to at least the image's built-in version.
func (s *Server) loadFloor(builtin *artifact) error {
	s.floor = signing.Floor{Version: builtin.Name}
	if s.cfg.StateDir != "" {
		b, err := os.ReadFile(filepath.Join(s.cfg.StateDir, floorFile))
		switch {
		case err == nil:
			var f struct {
				Service string `json:"service"`
				signing.Floor
			}
			if err := json.Unmarshal(b, &f); err != nil || f.Service != s.cfg.Service {
				return fmt.Errorf("corrupt high-water mark %s (service %q): %v", floorFile, f.Service, err)
			}
			if _, err := signing.ParseVersion(f.Version); err != nil {
				return fmt.Errorf("corrupt high-water mark %s: %w", floorFile, err)
			}
			if c, _ := signing.CompareVersions(f.Version, builtin.Name); c > 0 {
				s.floor = f.Floor
			}
		case errors.Is(err, os.ErrNotExist):
		default:
			return fmt.Errorf("high-water mark: %w", err)
		}
	}
	s.log.Emit("high_water_mark", "version", s.floor.Version, "restorable", s.floor.Restorable, "persisted", s.cfg.StateDir != "")
	return s.persistFloor()
}

// persistFloor atomically writes s.floor (tmp + fsync + rename + dir fsync).
// The caller holds s.mu (or is the constructor).
func (s *Server) persistFloor() error {
	if s.cfg.StateDir == "" {
		return nil
	}
	b, _ := json.Marshal(struct {
		Service string `json:"service"`
		signing.Floor
		Updated time.Time `json:"updated"`
	}{s.cfg.Service, s.floor, time.Now().UTC()})
	tmp, err := os.CreateTemp(s.cfg.StateDir, floorFile+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(s.cfg.StateDir, floorFile)); err != nil {
		return err
	}
	if d, err := os.Open(s.cfg.StateDir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// onDecision is the engine's OnDecision hook: it runs before the rollout's
// final phase is visible, so current/previous/floor are never stale with
// respect to the phase reported by /admin/rollout.
func (s *Server) onDecision(phase canary.Phase, keep, drop *canary.Version[In, Out]) {
	keepArt, _ := keep.Meta.(*artifact)
	dropArt, _ := drop.Meta.(*artifact)
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending
	s.pending = nil
	if phase != canary.PhasePromoted || keepArt == nil {
		return // candidate refused or aborted: nothing changes
	}
	if p != nil && p.rollback {
		// keep = the retained previous artifact. The rolled-back version may
		// never be re-installed from a bundle: the floor stays where it is
		// and is no longer restorable.
		s.current, s.previous = keepArt, nil
		s.floor.Restorable = false
	} else {
		s.previous, s.current = s.current, keepArt
		if dropArt != nil && s.previous != dropArt {
			s.previous = dropArt
		}
		if c, err := signing.CompareVersions(keepArt.Name, s.floor.Version); err == nil && c >= 0 {
			s.floor = signing.Floor{Version: keepArt.Name, Digest: keepArt.Digest, Restorable: true}
		}
	}
	if err := s.persistFloor(); err != nil {
		s.log.Emit("high_water_mark_persist_failed", "err", err.Error())
	} else {
		s.log.Emit("high_water_mark", "version", s.floor.Version, "restorable", s.floor.Restorable, "persisted", s.cfg.StateDir != "")
	}
}

// Handler serves the contract endpoints (/api, /healthz, /admin/status and
// the authenticated /admin/upgrade). withAdmin additionally mounts the
// privileged endpoints on the same mux (tests only; main always serves them
// on a separate listener).
func (s *Server) Handler(withAdmin bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api", s.api)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("/admin/status", s.status)
	mux.HandleFunc("/admin/upgrade", s.auth(s.contractUpgrade))
	if withAdmin {
		s.mountAdmin(mux)
	}
	return mux
}

// AdminHandler serves only the privileged endpoints (separate port so a
// NetworkPolicy can restrict it to the operator).
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("/admin/status", s.status)
	s.mountAdmin(mux)
	return mux
}

func (s *Server) mountAdmin(mux *http.ServeMux) {
	mux.HandleFunc("/admin/patch", s.auth(s.patch))
	mux.HandleFunc("/admin/rollout", s.auth(s.rolloutStatus))
	mux.HandleFunc("/admin/rollout/abort", s.auth(s.abort))
	mux.HandleFunc("/admin/rollback", s.auth(s.rollback))
}

// auth guards a privileged endpoint. Fail closed: with no configured token
// a request passes only if the process was explicitly started in tokenless
// mode (loopback-only listeners or -insecure-admin). Tokens are compared in
// constant time over their SHA-256 digests (no length leak).
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	want := sha256.Sum256([]byte(s.cfg.AdminToken))
	return func(w http.ResponseWriter, r *http.Request) {
		ok := false
		if s.cfg.AdminToken != "" {
			hdr := r.Header.Get("Authorization")
			if got, found := strings.CutPrefix(hdr, "Bearer "); found {
				g := sha256.Sum256([]byte(got))
				ok = subtle.ConstantTimeCompare(g[:], want[:]) == 1
			}
		} else {
			ok = s.cfg.AllowTokenless
		}
		if !ok {
			s.log.Emit("admin_denied", "path", r.URL.Path, "remote", r.RemoteAddr)
			httpJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		h(w, r)
	}
}

func httpJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var work time.Duration
	if v := q.Get("work_ms"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "bad work_ms", http.StatusBadRequest)
			return
		}
		work = time.Duration(n) * time.Millisecond
	}
	var x int64
	if v := q.Get("x"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			http.Error(w, "bad x", http.StatusBadRequest)
			return
		}
		x = n
	}
	key := r.Header.Get("X-Canary-Key")
	if key == "" {
		key = q.Get("key")
	}
	var pre func(context.Context) error
	if work > 0 {
		pre = func(ctx context.Context) error {
			t := time.NewTimer(work)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	res := s.eng.Serve(r.Context(), key, In{X: x}, pre)
	if res.Err != nil {
		if r.Context().Err() != nil {
			return // client went away
		}
		w.Header().Set("X-Version", res.Version)
		http.Error(w, res.Err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Version", res.Out.Version)
	w.Header().Set("X-Instance", s.instance)
	w.Header().Set("Content-Type", "application/json")
	w.Write(res.Out.Body)
}

// StatusDoc is /admin/status (contract fields plus S7 extras).
type StatusDoc struct {
	Version       string           `json:"version"`
	PID           int              `json:"pid"`
	InFlight      map[string]int64 `json:"inflight"`
	Count         uint64           `json:"count"`
	RunningDigest string           `json:"running_digest"`
	Rollout       string           `json:"rollout"`
}

func (s *Server) statusDoc() StatusDoc {
	inf := map[string]int64{"v1": 0, "v2": 0}
	for k, n := range s.eng.InFlight() {
		inf[k] += n
	}
	st := s.eng.Status()
	phase := string(canary.PhaseIdle)
	if st.Rollout != nil {
		phase = string(st.Rollout.Phase)
	}
	s.mu.Lock()
	dig := s.current.Digest
	s.mu.Unlock()
	return StatusDoc{Version: st.Stable, PID: os.Getpid(), InFlight: inf, Count: s.counter.Load(), RunningDigest: dig, Rollout: phase}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	httpJSON(w, http.StatusOK, s.statusDoc())
}

// RolloutDoc is /admin/rollout.
type RolloutDoc struct {
	canary.Status
	RunningDigest string        `json:"running_digest"`
	Previous      string        `json:"previous,omitempty"`
	PreviousDig   string        `json:"previous_digest,omitempty"`
	HighWaterMark signing.Floor `json:"high_water_mark"`
	Count         uint64        `json:"count"`
	Patches       []PatchRecord `json:"patches"`
}

func (s *Server) rolloutDoc() RolloutDoc {
	d := RolloutDoc{Status: s.eng.Status(), Count: s.counter.Load()}
	s.mu.Lock()
	d.RunningDigest = s.current.Digest
	if s.previous != nil {
		d.Previous, d.PreviousDig = s.previous.Name, s.previous.Digest
	}
	d.HighWaterMark = s.floor
	d.Patches = append([]PatchRecord(nil), s.patches...)
	s.mu.Unlock()
	return d
}

func (s *Server) rolloutStatus(w http.ResponseWriter, r *http.Request) {
	httpJSON(w, http.StatusOK, s.rolloutDoc())
}

func (s *Server) record(p PatchRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.patches = append(s.patches, p)
	if len(s.patches) > 20 {
		s.patches = s.patches[len(s.patches)-20:]
	}
}

// admissionError maps verification failures to HTTP codes and reasons.
func admissionError(err error) (int, string) {
	switch {
	case errors.Is(err, signing.ErrUnknownKey), errors.Is(err, signing.ErrBadSignature), errors.Is(err, signing.ErrDigestMismatch):
		return http.StatusForbidden, "SignatureInvalid"
	case errors.Is(err, signing.ErrDowngrade), errors.Is(err, signing.ErrBaseMismatch):
		return http.StatusConflict, "VersionPolicy"
	case errors.Is(err, signing.ErrWrongTarget), errors.Is(err, signing.ErrSchema), errors.Is(err, signing.ErrTooOld):
		return http.StatusUnprocessableEntity, "PolicyRejected"
	case errors.Is(err, canary.ErrBusy), errors.Is(err, canary.ErrStableChanged):
		return http.StatusConflict, "RolloutInProgress"
	case errors.Is(err, signing.ErrMalformedBundle), errors.Is(err, signing.ErrBadManifest), errors.Is(err, signing.ErrTooLarge):
		return http.StatusBadRequest, "MalformedBundle"
	default:
		return http.StatusUnprocessableEntity, "LoadFailed"
	}
}

// admit verifies a bundle, compiles it and starts a rollout. The policy
// (running version, high-water mark, base_version) is re-checked under s.mu
// immediately before Engine.StartFrom, which itself refuses to start if the
// stable version moved: check-then-start is atomic w.r.t. promotions.
func (s *Server) admit(b *signing.Bundle, strat canary.Strategy, o signing.CheckOptions, source string) (PatchRecord, error) {
	rec := PatchRecord{Time: time.Now(), Source: source}
	pol := signing.Policy{Service: s.cfg.Service, StateSchema: s.cfg.StateSchema, MaxAge: s.cfg.MaxBundleAge}
	check := func(m *signing.Manifest) (string, error) { // caller holds s.mu
		f := s.floor
		o.Floor = &f
		return s.current.Name, pol.Check(m, s.current.Name, o)
	}
	t0 := time.Now()
	m, err := s.cfg.Verifier.Verify(b)
	if err == nil {
		rec.Version, rec.Digest = m.Version, m.SHA256
		s.mu.Lock()
		_, err = check(m)
		s.mu.Unlock()
	}
	rec.VerifyMs = float64(time.Since(t0)) / 1e6
	if err == nil && s.rolloutActive() {
		err = canary.ErrBusy // don't compile a candidate that cannot start
	}
	if err != nil {
		return s.reject(rec, err)
	}
	art := &artifact{Name: m.Version, Digest: m.SHA256, Wasm: b.Module, Manifest: m}
	v, compile, err := s.load(art)
	rec.CompileMs = float64(compile) / 1e6
	if err != nil {
		return s.reject(rec, fmt.Errorf("load: %w", err))
	}
	s.mu.Lock()
	from, err := check(m)
	if err == nil {
		s.mismatchN.Store(0)
		err = s.eng.StartFrom(from, v, strat)
	}
	if err != nil {
		s.mu.Unlock()
		v.Close()
		return s.reject(rec, err)
	}
	s.pending = &pendingRollout{cand: art}
	s.mu.Unlock()
	s.log.Emit("upgrade_start", "from", from, "to", m.Version, "verify_ms", rec.VerifyMs, "compile_ms", rec.CompileMs, "source", source)
	rec.Accepted = true
	s.record(rec)
	return rec, nil
}

func (s *Server) rolloutActive() bool {
	r := s.eng.Status().Rollout
	return r != nil && r.Phase == canary.PhaseProgressing
}

func (s *Server) reject(rec PatchRecord, err error) (PatchRecord, error) {
	_, reason := admissionError(err)
	rec.Error, rec.Reason = err.Error(), reason
	s.record(rec)
	s.log.Emit("patch_rejected", "version", rec.Version, "reason", reason, "err", err.Error(), "source", rec.Source)
	return rec, err
}

func strategyFor(req *PatchRequest) (canary.Strategy, error) {
	if req.Strategy != nil {
		return *req.Strategy, nil
	}
	switch req.StrategyName {
	case "", "default":
		return canary.DefaultStrategy(), nil
	case "fast":
		return canary.FastStrategy(), nil
	case "immediate":
		return canary.ImmediateStrategy(), nil
	}
	return canary.Strategy{}, fmt.Errorf("%w: unknown strategy %q", signing.ErrMalformedBundle, req.StrategyName)
}

func (s *Server) patch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxBundleBytes*2))
	if err != nil {
		httpJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	var req PatchRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || len(req.Bundle) == 0 {
		rec, _ := s.reject(PatchRecord{Time: time.Now(), Source: "admin"}, fmt.Errorf("%w: bad patch request", signing.ErrMalformedBundle))
		httpJSON(w, http.StatusBadRequest, rec)
		return
	}
	strat, err := strategyFor(&req)
	var rec PatchRecord
	if err == nil {
		var b *signing.Bundle
		b, err = signing.DecodeEnvelope(req.Bundle, s.cfg.MaxBundleBytes)
		if err == nil {
			rec, err = s.admit(b, strat, signing.CheckOptions{Reconcile: req.Reconcile}, "admin")
		} else {
			rec, _ = s.reject(PatchRecord{Time: time.Now(), Source: "admin"}, err)
		}
	}
	if err != nil {
		code, reason := admissionError(err)
		rec.Error, rec.Reason = err.Error(), reason
		httpJSON(w, code, rec)
		return
	}
	httpJSON(w, http.StatusAccepted, rec)
}

// AbortDoc is the answer of POST /admin/rollout/abort: the REAL outcome of
// the rollout that was current when the abort arrived.
type AbortDoc struct {
	Aborted   bool         `json:"aborted"` // the candidate is not serving as stable
	Sent      bool         `json:"sent"`    // abort registered before the decision
	Phase     canary.Phase `json:"phase"`
	Stable    string       `json:"stable"`
	Candidate string       `json:"candidate,omitempty"`
	Reason    string       `json:"reason,omitempty"`
}

// abort aborts the running rollout and waits for its decision. 200 = the
// candidate is not (and will not be) stable; 409 = it was promoted before
// the abort took effect (the caller must roll it back explicitly).
func (s *Server) abort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "operator abort"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ro, sent, err := s.eng.AbortAndWait(ctx, reason)
	if err != nil {
		httpJSON(w, http.StatusGatewayTimeout, map[string]any{"error": err.Error()})
		return
	}
	d := AbortDoc{Sent: sent, Phase: ro.Phase, Stable: ro.Stable, Candidate: ro.Candidate, Reason: ro.Reason}
	d.Aborted = ro.Phase != canary.PhasePromoted || ro.Candidate == "" || ro.Stable != ro.Candidate
	code := http.StatusOK
	if !d.Aborted {
		code = http.StatusConflict
	}
	s.log.Emit("rollout_abort_request", "sent", sent, "phase", string(ro.Phase), "stable", ro.Stable, "candidate", ro.Candidate)
	httpJSON(w, code, d)
}

// rollback re-installs the retained previous artifact (explicit rollback,
// e.g. the operator rolling back a fleet after another pod failed). It is the
// ONLY way to move to an older version: the target is the exact artifact
// (bytes and digest) this process ran before its last promotion, never a
// bundle supplied by the caller. Optional ?to=<version>&digest=<sha256>
// must match the retained artifact.
func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	prev, cur := s.previous, s.current
	s.mu.Unlock()
	if prev == nil {
		httpJSON(w, http.StatusConflict, map[string]any{"error": "no retained previous version", "stable": cur.Name})
		return
	}
	q := r.URL.Query()
	if want := q.Get("to"); want != "" && want != prev.Name {
		httpJSON(w, http.StatusConflict, map[string]any{"error": fmt.Sprintf("retained version is %s, not %s", prev.Name, want), "stable": cur.Name})
		return
	}
	if want := q.Get("digest"); want != "" && !strings.EqualFold(want, prev.Digest) {
		httpJSON(w, http.StatusConflict, map[string]any{"error": "retained artifact digest does not match", "stable": cur.Name})
		return
	}
	v, _, err := s.load(prev)
	if err != nil {
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.mu.Lock()
	if s.previous != prev || s.current != cur {
		s.mu.Unlock()
		v.Close()
		httpJSON(w, http.StatusConflict, map[string]any{"error": "retained/running version changed concurrently", "reason": "RolloutInProgress"})
		return
	}
	if err := s.eng.StartFrom(cur.Name, v, canary.ImmediateStrategy()); err != nil {
		s.mu.Unlock()
		v.Close()
		code, reason := admissionError(err)
		httpJSON(w, code, map[string]any{"error": err.Error(), "reason": reason})
		return
	}
	s.pending = &pendingRollout{cand: prev, rollback: true}
	s.mu.Unlock()
	s.log.Emit("upgrade_start", "from", cur.Name, "to", prev.Name, "source", "rollback")
	res, err := s.eng.WaitDecided(context.Background())
	ok := err == nil && res.Phase == canary.PhasePromoted && res.Stable == prev.Name
	rec := PatchRecord{Time: time.Now(), Version: prev.Name, Digest: prev.Digest, Accepted: ok, Source: "rollback"}
	if !ok {
		rec.Error = fmt.Sprintf("rollback ended %s (stable %s)", res.Phase, res.Stable)
	}
	s.record(rec)
	code := http.StatusOK
	if !ok {
		code = http.StatusInternalServerError
	}
	httpJSON(w, code, map[string]any{"stable": res.Stable, "phase": res.Phase, "digest": prev.Digest})
}

// contractUpgrade implements the harness endpoint: load the pre-staged
// signed bundle <bundle-dir>/<to> and run the fast rollout schedule.
func (s *Server) contractUpgrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !s.cfg.ContractUpgrade {
		http.Error(w, "disabled; use the admin endpoint", http.StatusForbidden)
		return
	}
	to := r.URL.Query().Get("to")
	if err := s.Upgrade(to); err != nil {
		code, _ := admissionError(err)
		httpJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "{\"accepted\":true,\"to\":%q}\n", to)
}

func (s *Server) nextVersion() string {
	cur := s.eng.Stable()
	if v, err := signing.ParseVersion(cur); err == nil {
		return fmt.Sprintf("v%d", v[0]+1)
	}
	return "v2"
}

// Upgrade starts a rollout from a pre-staged bundle ("" = next version).
func (s *Server) Upgrade(to string) error {
	if to == "" {
		to = s.nextVersion()
	}
	if _, err := signing.ParseVersion(to); err != nil || strings.ContainsAny(to, "/\\") {
		return fmt.Errorf("%w: bad version %q", signing.ErrMalformedBundle, to)
	}
	b, err := signing.ReadDir(filepath.Join(s.cfg.BundleDir, to), s.cfg.MaxBundleBytes)
	if err != nil {
		s.log.Emit("upgrade_failed", "err", err.Error())
		return err
	}
	_, err = s.admit(b, s.cfg.UpgradeStrategy, signing.CheckOptions{Reconcile: true}, "contract")
	if err != nil {
		s.log.Emit("upgrade_failed", "err", err.Error())
	}
	return err
}

// Close releases the engine.
func (s *Server) Close() { s.eng.Close() }
