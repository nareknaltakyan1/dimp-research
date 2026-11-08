package main

import (
	"bytes"
	"context"
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
	AdminToken  string // "" = no token required (development only)
	// ContractUpgrade enables the unauthenticated harness endpoint
	// /admin/upgrade (it can only load pre-staged, signed bundles).
	ContractUpgrade bool
	Wasm            wasmguest.Options
	MaxBundleBytes  int64
	Events          *svc.EventLog
	// UpgradeStrategy is used by /admin/upgrade and SIGHUP.
	UpgradeStrategy canary.Strategy
}

// PatchRequest is the body of POST /admin/patch.
type PatchRequest struct {
	Bundle       json.RawMessage  `json:"bundle"` // signing.Envelope
	Strategy     *canary.Strategy `json:"strategy,omitempty"`
	StrategyName string           `json:"strategy_name,omitempty"` // default | fast | immediate
	Reconcile    bool             `json:"reconcile,omitempty"`
	Rollback     bool             `json:"rollback,omitempty"`
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

	mu        sync.Mutex
	current   *artifact // stable
	previous  *artifact // retained for /admin/rollback
	patches   []PatchRecord
	mismatchN atomic.Int64
}

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
	v, _, err := s.load(art)
	if err != nil {
		return nil, err
	}
	s.current = art
	s.eng = canary.New(canary.Config[In, Out]{
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

// Handler serves the contract endpoints; if admin is nil the admin
// endpoints are mounted on the same mux.
func (s *Server) Handler(withAdmin bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api", s.api)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("/admin/status", s.status)
	mux.HandleFunc("/admin/upgrade", s.contractUpgrade)
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

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AdminToken)) != 1 {
				s.log.Emit("admin_denied", "path", r.URL.Path, "remote", r.RemoteAddr)
				httpJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
				return
			}
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
	Count         uint64        `json:"count"`
	Patches       []PatchRecord `json:"patches"`
}

func (s *Server) rolloutDoc() RolloutDoc {
	d := RolloutDoc{Status: s.eng.Status(), Count: s.counter.Load()}
	s.mu.Lock()
	d.RunningDigest = s.current.Digest
	if s.previous != nil {
		d.Previous = s.previous.Name
	}
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
	case errors.Is(err, canary.ErrBusy):
		return http.StatusConflict, "RolloutInProgress"
	case errors.Is(err, signing.ErrMalformedBundle), errors.Is(err, signing.ErrBadManifest), errors.Is(err, signing.ErrTooLarge):
		return http.StatusBadRequest, "MalformedBundle"
	default:
		return http.StatusUnprocessableEntity, "LoadFailed"
	}
}

// admit verifies a bundle, compiles it and starts a rollout.
func (s *Server) admit(b *signing.Bundle, strat canary.Strategy, o signing.CheckOptions, source string) (PatchRecord, error) {
	rec := PatchRecord{Time: time.Now(), Source: source}
	t0 := time.Now()
	m, err := s.cfg.Verifier.Verify(b)
	if err == nil {
		rec.Version, rec.Digest = m.Version, m.SHA256
		err = signing.Policy{Service: s.cfg.Service, StateSchema: s.cfg.StateSchema}.Check(m, s.eng.Stable(), o)
	}
	rec.VerifyMs = float64(time.Since(t0)) / 1e6
	if err != nil {
		return s.reject(rec, err)
	}
	art := &artifact{Name: m.Version, Digest: m.SHA256, Wasm: b.Module, Manifest: m}
	v, compile, err := s.load(art)
	rec.CompileMs = float64(compile) / 1e6
	if err != nil {
		return s.reject(rec, fmt.Errorf("load: %w", err))
	}
	from := s.eng.Stable()
	s.log.Emit("upgrade_start", "from", from, "to", m.Version, "verify_ms", rec.VerifyMs, "compile_ms", rec.CompileMs, "source", source)
	s.mismatchN.Store(0)
	if err := s.eng.Start(v, strat); err != nil {
		v.Close()
		return s.reject(rec, err)
	}
	s.mu.Lock()
	prev := s.current
	s.mu.Unlock()
	go s.followRollout(prev, art)
	rec.Accepted = true
	s.record(rec)
	return rec, nil
}

func (s *Server) reject(rec PatchRecord, err error) (PatchRecord, error) {
	_, reason := admissionError(err)
	rec.Error, rec.Reason = err.Error(), reason
	s.record(rec)
	s.log.Emit("patch_rejected", "version", rec.Version, "reason", reason, "err", err.Error(), "source", rec.Source)
	return rec, err
}

// followRollout updates the retained artifacts once the rollout finishes.
func (s *Server) followRollout(prev, cand *artifact) {
	r, err := s.eng.Wait(context.Background())
	if err != nil || r.Phase != canary.PhasePromoted || r.Stable != cand.Name {
		return
	}
	s.mu.Lock()
	s.current, s.previous = cand, prev
	s.mu.Unlock()
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
	if err := json.Unmarshal(body, &req); err != nil || len(req.Bundle) == 0 {
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
			rec, err = s.admit(b, strat, signing.CheckOptions{Reconcile: req.Reconcile, Rollback: req.Rollback}, "admin")
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

func (s *Server) abort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "operator abort"
	}
	ok := s.eng.Abort(reason)
	httpJSON(w, http.StatusOK, map[string]any{"aborted": ok})
}

// rollback re-installs the retained previous version (explicit rollback,
// e.g. the operator rolling back a fleet after another pod failed).
func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	prev, cur := s.previous, s.current
	s.mu.Unlock()
	if prev == nil {
		httpJSON(w, http.StatusConflict, map[string]any{"error": "no retained previous version"})
		return
	}
	if want := r.URL.Query().Get("to"); want != "" && want != prev.Name {
		httpJSON(w, http.StatusConflict, map[string]any{"error": fmt.Sprintf("retained version is %s, not %s", prev.Name, want)})
		return
	}
	v, _, err := s.load(prev)
	if err != nil {
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.log.Emit("upgrade_start", "from", cur.Name, "to", prev.Name, "source", "rollback")
	if err := s.eng.Start(v, canary.ImmediateStrategy()); err != nil {
		v.Close()
		code, _ := admissionError(err)
		httpJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	res, _ := s.eng.Wait(context.Background())
	s.mu.Lock()
	if res.Phase == canary.PhasePromoted {
		s.current, s.previous = prev, nil
	}
	s.mu.Unlock()
	s.record(PatchRecord{Time: time.Now(), Version: prev.Name, Digest: prev.Digest, Accepted: true, Source: "rollback"})
	httpJSON(w, http.StatusOK, map[string]any{"stable": s.eng.Stable(), "phase": res.Phase})
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
