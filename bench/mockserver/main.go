// Command mockserver is a trivial server that implements docs/CONTRACT.md.
// It exists only to validate the benchmark harness (bench/, scripts/) before
// the real solutions in cmd/ are ready. It is NOT a solution.
//
// Upgrade modes (-mode):
//
//	inproc  (default) atomic swap of the request handler inside one process.
//	        State (counter) is preserved; old-version requests drain.
//	restart simulates a naive restart in the same process: the listener is
//	        closed, the server is "down" for -down-ms, then reopens with the
//	        new version and a RESET counter. Used to check that the harness
//	        detects downtime, refused connections and state loss.
//
// Upgrading to a version whose name contains "broken" emits upgrade_failed and
// keeps serving the old version (rollback path, for E6).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	addr      = flag.String("addr", ":8080", "listen address")
	version   = flag.String("version", envOr("VERSION", "v1"), "initial version")
	mode      = flag.String("mode", "inproc", "upgrade mode: inproc|restart")
	swapMS    = flag.Int("swap-ms", 20, "simulated time to prepare the new version (inproc)")
	downMS    = flag.Int("down-ms", 300, "listener-down time (restart mode)")
	sigTarget = flag.String("sighup-to", "v2", "target version for SIGHUP")
	leakKB    = flag.Int("leak-kb", 0, "bytes (KiB) intentionally leaked per upgrade (harness test of memory metric)")
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type server struct {
	count     atomic.Int64
	cur       atomic.Pointer[string]
	mu        sync.Mutex // serialises upgrades
	inflight  sync.Map   // version -> *atomic.Int64
	srv       *http.Server
	ln        net.Listener
	upgrading atomic.Bool
	leak      [][]byte
}

func emit(ev map[string]any) {
	ev["ts_ns"] = time.Now().UnixNano()
	if _, ok := ev["pid"]; !ok {
		ev["pid"] = os.Getpid()
	}
	b, _ := json.Marshal(ev)
	os.Stderr.Write(append(b, '\n'))
}

func (s *server) gauge(v string) *atomic.Int64 {
	g, _ := s.inflight.LoadOrStore(v, new(atomic.Int64))
	return g.(*atomic.Int64)
}

func (s *server) api(w http.ResponseWriter, r *http.Request) {
	v := *s.cur.Load() // version is bound when the request is accepted by the handler
	g := s.gauge(v)
	g.Add(1)
	defer g.Add(-1)
	if ms, _ := strconv.Atoi(r.URL.Query().Get("work_ms")); ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	n := s.count.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Version", v)
	fmt.Fprintf(w, `{"version":%q,"pid":%d,"count":%d,"ts_ns":%d}`+"\n", v, os.Getpid(), n, time.Now().UnixNano())
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	in := map[string]int64{}
	s.inflight.Range(func(k, v any) bool { in[k.(string)] = v.(*atomic.Int64).Load(); return true })
	json.NewEncoder(w).Encode(map[string]any{"version": *s.cur.Load(), "pid": os.Getpid(), "inflight": in, "count": s.count.Load()})
}

func (s *server) upgrade(to string) {
	if !s.upgrading.CompareAndSwap(false, true) {
		emit(map[string]any{"ev": "upgrade_failed", "err": "upgrade already in progress"})
		return
	}
	defer s.upgrading.Store(false)
	s.mu.Lock()
	defer s.mu.Unlock()
	from := *s.cur.Load()
	emit(map[string]any{"ev": "upgrade_start", "from": from, "to": to})
	time.Sleep(time.Duration(*swapMS) * time.Millisecond)
	if strings.Contains(to, "broken") {
		emit(map[string]any{"ev": "upgrade_failed", "err": "simulated failure: new version failed health check; rolled back to " + from})
		return
	}
	if *leakKB > 0 {
		b := make([]byte, *leakKB*1024)
		for i := range b {
			b[i] = 1
		}
		s.leak = append(s.leak, b)
	}
	switch *mode {
	case "restart":
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		s.srv.Shutdown(ctx) // aggressive: in-flight requests beyond 50ms are cut
		cancel()
		time.Sleep(time.Duration(*downMS) * time.Millisecond)
		s.count.Store(0)
		s.cur.Store(&to)
		if err := s.listen(); err != nil {
			emit(map[string]any{"ev": "upgrade_failed", "err": err.Error()})
			return
		}
		emit(map[string]any{"ev": "new_version_serving", "version": to})
		emit(map[string]any{"ev": "old_version_drained", "version": from})
	default:
		s.cur.Store(&to)
		emit(map[string]any{"ev": "new_version_serving", "version": to})
		g := s.gauge(from)
		for g.Load() > 0 {
			time.Sleep(time.Millisecond)
		}
		emit(map[string]any{"ev": "old_version_drained", "version": from})
	}
	emit(map[string]any{"ev": "upgrade_done"})
}

func (s *server) listen() error {
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api", s.api)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
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
		go s.upgrade(to)
		w.WriteHeader(http.StatusAccepted)
	})
	s.ln = ln
	s.srv = &http.Server{Handler: mux}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("serve: %v", err)
		}
	}()
	return nil
}

func main() {
	flag.Parse()
	s := &server{}
	v := *version
	s.cur.Store(&v)
	if err := s.listen(); err != nil {
		log.Fatal(err)
	}
	emit(map[string]any{"ev": "started", "version": v, "addr": *addr})
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	for x := range sig {
		if x == syscall.SIGHUP {
			go s.upgrade(*sigTarget)
			continue
		}
		// graceful shutdown (Kubernetes sends SIGTERM)
		emit(map[string]any{"ev": "shutdown"})
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		s.srv.Shutdown(ctx)
		cancel()
		return
	}
}
