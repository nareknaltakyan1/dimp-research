// Package svc wires a hotswap.Registry into the HTTP/event-log contract
// shared by all solutions (see docs/CONTRACT.md). It is used by S1 (compiled
// in versions), S2 (Go plugins) and S3 (WASM modules); they differ only in
// how a new implementation is *loaded* and how an old one is *unloaded*.
package svc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/hotswap"
)

// Impl is one version of the business logic. It must write the full /api
// response (status, X-Version header, JSON body) and increment the shared
// counter. The contract's work_ms sleep is done by the Server while the
// version is pinned, so the implementation does not need to.
type Impl interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}

// EventLog writes contract events as JSON lines (default: stderr).
type EventLog struct {
	mu  sync.Mutex
	w   io.Writer
	pid int
}

// NewEventLog returns an EventLog writing to w.
func NewEventLog(w io.Writer) *EventLog { return &EventLog{w: w, pid: os.Getpid()} }

// Emit writes {"ev":ev,"ts_ns":...,"pid":..., kv...}.
func (l *EventLog) Emit(ev string, kv ...any) {
	m := map[string]any{"ev": ev, "ts_ns": time.Now().UnixNano(), "pid": l.pid}
	for i := 0; i+1 < len(kv); i += 2 {
		m[fmt.Sprint(kv[i])] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w.Write(append(b, '\n'))
}

// Config describes a solution.
type Config struct {
	InitialName string
	Initial     Impl
	// Load builds (loads, compiles, instantiates) the implementation named to.
	Load func(to string) (Impl, error)
	// Next returns the default upgrade target for SIGHUP / missing ?to=.
	Next func(current string) string
	// Count returns the service-wide request counter (for /admin/status).
	Count func() uint64
	// Probe optionally validates a loaded implementation before publication.
	Probe func(Impl) error
	// Unload is called on an implementation that is no longer reachable:
	// the old version after it drained, or a loaded version that failed
	// before publication. May be nil (e.g. plugins cannot be unloaded).
	Unload func(name string, impl Impl)
	// DrainTimeout bounds how long Upgrade waits for the old version.
	DrainTimeout time.Duration
	// Events defaults to stderr.
	Events *EventLog
}

// Server is a contract-compliant HTTP server backed by a hotswap.Registry.
type Server struct {
	cfg  Config
	Reg  *hotswap.Registry[Impl]
	Log  *EventLog
	busy atomic.Bool
}

// New creates a Server.
func New(cfg Config) *Server {
	if cfg.Events == nil {
		cfg.Events = NewEventLog(os.Stderr)
	}
	if cfg.DrainTimeout == 0 {
		cfg.DrainTimeout = 30 * time.Second
	}
	if cfg.Next == nil {
		cfg.Next = func(string) string { return "v2" }
	}
	return &Server{cfg: cfg, Reg: hotswap.New(cfg.InitialName, cfg.Initial), Log: cfg.Events}
}

// ErrBusy is returned when an upgrade is already in progress.
var ErrBusy = errors.New("upgrade already in progress")

var validName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Handler returns the contract HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api", s.api)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("/admin/status", s.status)
	mux.HandleFunc("/admin/upgrade", s.upgrade)
	return mux
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	var work time.Duration
	if q := r.URL.Query().Get("work_ms"); q != "" {
		ms, err := strconv.Atoi(q)
		if err != nil || ms < 0 {
			http.Error(w, "bad work_ms", http.StatusBadRequest)
			return
		}
		work = time.Duration(ms) * time.Millisecond
	}
	v := s.Reg.Acquire() // pin the version for the whole request
	defer v.Release()
	if work > 0 {
		t := time.NewTimer(work)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}
	v.Impl.ServeHTTP(w, r)
}

// Status is the /admin/status document.
type Status struct {
	Version  string           `json:"version"`
	PID      int              `json:"pid"`
	InFlight map[string]int64 `json:"inflight"`
	Count    uint64           `json:"count"`
}

// Status returns the current status.
func (s *Server) Status() Status {
	inf := map[string]int64{"v1": 0, "v2": 0}
	for k, n := range s.Reg.InFlight() {
		inf[k] += n
	}
	var c uint64
	if s.cfg.Count != nil {
		c = s.cfg.Count()
	}
	return Status{Version: s.Reg.Current().Name, PID: os.Getpid(), InFlight: inf, Count: c}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.Status())
}

func (s *Server) upgrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	to := r.URL.Query().Get("to")
	if to != "" && !validName.MatchString(to) {
		http.Error(w, "bad version name", http.StatusBadRequest)
		return
	}
	if !s.busy.CompareAndSwap(false, true) {
		http.Error(w, ErrBusy.Error(), http.StatusConflict)
		return
	}
	go func() {
		defer s.busy.Store(false)
		s.upgradeLocked(to)
	}()
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "{\"accepted\":true,\"to\":%q}\n", to)
}

// Upgrade performs a synchronous upgrade (used by SIGHUP and tests).
func (s *Server) Upgrade(to string) error {
	if !s.busy.CompareAndSwap(false, true) {
		return ErrBusy
	}
	defer s.busy.Store(false)
	return s.upgradeLocked(to)
}

func (s *Server) fail(err error) error {
	s.Log.Emit("upgrade_failed", "err", err.Error())
	return err
}

func (s *Server) upgradeLocked(to string) error {
	from := s.Reg.Current().Name
	if to == "" {
		to = s.cfg.Next(from)
	}
	s.Log.Emit("upgrade_start", "from", from, "to", to)
	t0 := time.Now()
	impl, err := s.cfg.Load(to)
	if err != nil {
		return s.fail(fmt.Errorf("load %s: %w", to, err))
	}
	loadDur := time.Since(t0)
	u, err := s.Reg.Swap(to, impl, hotswap.SwapOptions[Impl]{
		Probe: s.cfg.Probe,
		OnPublished: func(nv, _ *hotswap.Version[Impl]) {
			s.Log.Emit("new_version_serving", "version", nv.Name, "load_ns", loadDur.Nanoseconds())
		},
	})
	if err != nil {
		if errors.Is(err, hotswap.ErrVerify) && u != nil {
			// Published, then rolled back: the failed version may still have
			// requests; unload it only after it drained.
			go func() {
				<-u.New.Drained()
				s.unload(u.New)
			}()
		} else if s.cfg.Unload != nil {
			s.cfg.Unload(to, impl) // never published: nothing can reference it
		}
		return s.fail(err)
	}
	if err := u.Wait(context.Background(), s.cfg.DrainTimeout); err != nil {
		// The new version is serving and the upgrade is NOT rolled back; the
		// old version is never cut off. This is "drain_timeout", not
		// "upgrade_failed" (which the contract reserves for upgrades that left
		// or put the old version back in service). Finish draining (and
		// unloading) in the background.
		s.Log.Emit("drain_timeout", "version", u.Old.Name, "new_version", u.New.Name,
			"inflight", u.Old.Refs(), "err", err.Error())
		go func() {
			<-u.Drained()
			s.finishDrain(u)
		}()
		s.Log.Emit("upgrade_done", "from", from, "to", to, "total_ns", time.Since(t0).Nanoseconds(), "drained", false)
		return err
	}
	s.finishDrain(u)
	s.Log.Emit("upgrade_done", "from", from, "to", to, "total_ns", time.Since(t0).Nanoseconds())
	return nil
}

func (s *Server) finishDrain(u *hotswap.Upgrade[Impl]) {
	s.Log.Emit("old_version_drained", "version", u.Old.Name,
		"drain_ns", u.Old.DrainedAt().Sub(u.New.PublishedAt).Nanoseconds())
	s.unload(u.Old)
}

// unload releases a drained version through the registry, which refuses if
// the same implementation was republished (so Rollback can never publish a
// closed module, and a closed module is never current).
func (s *Server) unload(v *hotswap.Version[Impl]) {
	if s.cfg.Unload == nil {
		return
	}
	if err := s.Reg.Unload(v, func(impl Impl) { s.cfg.Unload(v.Name, impl) }); err != nil {
		s.Log.Emit("unload_skipped", "version", v.Name, "err", err.Error())
	}
}

// Run serves on addr until SIGINT/SIGTERM; SIGHUP triggers Upgrade("").
func (s *Server) Run(addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigs {
			if sig == syscall.SIGHUP {
				go func() {
					if err := s.Upgrade(""); errors.Is(err, ErrBusy) {
						s.Log.Emit("upgrade_failed", "err", err.Error())
					}
				}()
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			srv.Shutdown(ctx)
			cancel()
			return
		}
	}()
	s.Log.Emit("listening", "addr", addr, "version", s.Reg.Current().Name)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// WriteAPIResponse writes the contract /api response. Helper for in-process
// implementations (plugins and WASM guests format the body themselves).
func WriteAPIResponse(w http.ResponseWriter, version string, count uint64) {
	w.Header().Set("X-Version", version)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{\"version\":%q,\"pid\":%d,\"count\":%d,\"ts_ns\":%d}\n", version, os.Getpid(), count, time.Now().UnixNano())
}
