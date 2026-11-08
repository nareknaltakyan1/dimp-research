// Command s6-hotpatch is the S6 solution: "hotpatch-ready compilation".
//
// The business layer (./biz) is ordinary Go. At build time hotpatchgen turns
// its //dimp:patchable functions into trampolines + implementation slots
// (./gen/biz). At run time a new version arrives as a Go plugin with an
// ed25519-signed manifest; its replacement functions are verified and
// published as ONE patch-set transaction (one atomic pointer store). Every
// request pins the patch set at entry, so it runs entirely on the old or
// entirely on the new code. The counter (service state) lives in the same
// process and is untouched by the patch.
//
//	POST /admin/upgrade?to=v2   load plugins/v2.so (+ v2.manifest.json), verify, apply (202)
//	POST /admin/rollback        revert the most recent transaction (202)
//	SIGHUP                      upgrade to v2, or roll back if a patch is applied
//
// See docs/solutions/S6-hotpatch-ready-compilation.md.
package main

//go:generate go run ../../tools/hotpatchgen gen -dir biz -out gen/biz

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/nareknaltakyan1/dimp-research/cmd/s6-hotpatch/gen/biz"
	"github.com/nareknaltakyan1/dimp-research/internal/patchable"
	"github.com/nareknaltakyan1/dimp-research/internal/patchable/delivery"
)

// events writes contract JSON lines.
type events struct {
	mu  sync.Mutex
	w   io.Writer
	pid int
}

func (e *events) emit(ev string, kv ...any) {
	m := map[string]any{"ev": ev, "ts_ns": time.Now().UnixNano(), "pid": e.pid}
	for i := 0; i+1 < len(kv); i += 2 {
		m[fmt.Sprint(kv[i])] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	e.mu.Lock()
	e.w.Write(append(b, '\n'))
	e.mu.Unlock()
}

type server struct {
	reg     *patchable.Registry
	svc     *biz.Service
	loader  *delivery.Loader
	plugins string
	drain   time.Duration
	ev      *events
	upMu    sync.Mutex // one upgrade/rollback at a time
	pid     int
}

func newServer(reg *patchable.Registry, plugins string, keys []ed25519.PublicKey, drain time.Duration, ev *events) *server {
	return &server{reg: reg, svc: biz.NewService(), loader: &delivery.Loader{Reg: reg, Trusted: keys},
		plugins: plugins, drain: drain, ev: ev, pid: os.Getpid()}
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func main() {
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	addr := flag.String("addr", ":8080", "listen address")
	plugins := flag.String("plugins", filepath.Join(exeDir, "plugins"), "directory with <name>.so and <name>.manifest.json")
	trust := flag.String("trust", filepath.Join(exeDir, "keys", "trusted.pub"), "trusted ed25519 public keys (hex, one per line)")
	drain := flag.Duration("drain-timeout", 30*time.Second, "max time to wait for replaced code to become quiescent")
	consistency := flag.String("consistency", "request", "request (pin patch set per request) | call (ablation: resolve per call)")
	stackScan := flag.Bool("stackscan", true, "confirm quiescence with a runtime.Stack scan")
	flag.Parse()

	ev := &events{w: os.Stderr, pid: os.Getpid()}
	reg := patchable.Default
	reg.SetBaseLabel("v1")
	reg.SetPerCallConsistency(*consistency == "call")
	reg.SetStackScan(*stackScan)
	keys, err := delivery.ReadPublicKeys(*trust)
	if err != nil {
		log.Printf("s6: no trusted keys (%v): every upgrade will be rejected", err)
	}
	s := newServer(reg, *plugins, keys, *drain, ev)

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if reg.Top() != nil {
				go s.rollback()
			} else {
				go s.upgrade("v2")
			}
		}
	}()
	log.Printf("s6-hotpatch listening on %s (pid %d, consistency=%s, %d patchable functions)", *addr, s.pid, *consistency, len(reg.Slots()))
	if err := http.ListenAndServe(*addr, s.routes()); err != nil {
		log.Fatal(err)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api", s.api)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("/admin/status", s.status)
	mux.HandleFunc("/admin/upgrade", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		to := r.URL.Query().Get("to")
		if to == "" {
			to = "v2"
		}
		if !nameRE.MatchString(to) {
			http.Error(w, "bad patch name", http.StatusBadRequest)
			return
		}
		go s.upgrade(to)
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "{\"accepted\":true,\"to\":%q}\n", to)
	})
	mux.HandleFunc("/admin/rollback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		go s.rollback()
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "{\"accepted\":true}\n")
	})
	return mux
}

type apiResp struct {
	Version string   `json:"version"`
	PID     int      `json:"pid"`
	Count   uint64   `json:"count"`
	TsNs    int64    `json:"ts_ns"`
	Epoch   uint64   `json:"epoch"`
	Cents   int64    `json:"total_cents"`
	Quote   string   `json:"quote"`
	Impls   []string `json:"impls"`
	Error   string   `json:"error,omitempty"`
}

func (s *server) api(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := biz.Request{SKU: "widget", Qty: 3, Coupon: "SAVE10"}
	if v := q.Get("sku"); v != "" {
		req.SKU = v
	}
	if v, err := strconv.Atoi(q.Get("qty")); err == nil {
		req.Qty = v
	}
	if q.Has("coupon") {
		req.Coupon = q.Get("coupon")
	}
	req.WorkMs, _ = strconv.Atoi(q.Get("work_ms"))

	// Pin the patch set for the whole request: every trampoline below
	// resolves its implementation from this epoch.
	ctx, pin := s.reg.Pin(r.Context())
	defer pin.Unpin()
	ps := pin.Set()
	tr := &biz.Trace{}
	quote, err := s.svc.Handle(ctx, req, tr)
	resp := apiResp{Version: ps.Label, PID: s.pid, Count: quote.Count, TsNs: time.Now().UnixNano(),
		Epoch: ps.Epoch, Cents: quote.Cents, Quote: quote.Text, Impls: tr.Steps()}
	code := http.StatusOK
	if err != nil {
		resp.Error = err.Error()
		code = http.StatusUnprocessableEntity
		if !errors.Is(err, biz.ErrInvalid) {
			code = http.StatusInternalServerError
		}
	}
	w.Header().Set("X-Version", ps.Label)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(resp)
	w.Write(append(b, '\n'))
}

func (s *server) status(w http.ResponseWriter, _ *http.Request) {
	cur := s.reg.Current()
	inflight := map[string]int64{"v1": 0}
	for k, v := range s.reg.Inflight() {
		inflight[k] += v
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"version": cur.Label, "pid": s.pid, "inflight": inflight, "count": s.svc.Count(),
		"epoch": cur.Epoch, "slots": s.reg.Slots(),
	})
}

func (s *server) upgrade(to string) {
	if !s.upMu.TryLock() {
		s.ev.emit("upgrade_failed", "to", to, "err", "another upgrade is in progress")
		return
	}
	defer s.upMu.Unlock()
	start := time.Now()
	from := s.reg.Current().Label
	s.ev.emit("upgrade_start", "from", from, "to", to)
	label, patches, rep, err := s.loader.Load(s.plugins, to)
	if err != nil {
		s.ev.emit("upgrade_failed", "to", to, "err", err.Error(), "stage", "load", "load", rep)
		return
	}
	txn, err := s.reg.Apply(label, patches)
	if err != nil {
		s.ev.emit("upgrade_failed", "to", to, "err", err.Error(), "stage", "apply", "load", rep)
		return
	}
	s.finish(txn, start, from, label, "upgrade", rep)
}

func (s *server) rollback() {
	if !s.upMu.TryLock() {
		s.ev.emit("upgrade_failed", "err", "another upgrade is in progress", "kind", "rollback")
		return
	}
	defer s.upMu.Unlock()
	start := time.Now()
	top := s.reg.Top()
	if top == nil {
		s.ev.emit("upgrade_failed", "err", "nothing to roll back", "kind", "rollback")
		return
	}
	from := s.reg.Current().Label
	s.ev.emit("upgrade_start", "from", from, "to", top.From.Label, "kind", "rollback")
	txn, err := s.reg.Revert(top)
	if err != nil {
		s.ev.emit("upgrade_failed", "err", err.Error(), "kind", "rollback")
		return
	}
	s.finish(txn, start, from, txn.To.Label, "rollback", nil)
}

func (s *server) finish(txn *patchable.Txn, start time.Time, from, to, kind string, rep any) {
	s.ev.emit("new_version_serving", "version", to, "epoch", txn.To.Epoch, "kind", kind, "symbols", txn.Symbols,
		"apply_ns", txn.ApplyLatency.Nanoseconds(), "publish_ns", txn.PublishLatency.Nanoseconds(), "load", rep)
	q, err := txn.Wait(context.Background(), s.drain)
	if err != nil {
		// The new code is serving; the old code is still referenced. Report,
		// do not roll back: rolling back would not make the old code idle
		// any sooner.
		s.ev.emit("drain_timeout", "version", from, "err", err.Error(), "quiescence", q)
	} else {
		s.ev.emit("old_version_drained", "version", from, "epoch", txn.From.Epoch, "quiescence", q)
	}
	s.ev.emit("upgrade_done", "kind", kind, "from", from, "to", to, "total_ns", time.Since(start).Nanoseconds())
}
