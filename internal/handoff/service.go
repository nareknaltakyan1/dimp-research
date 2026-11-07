package handoff

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

// Service is the contract workload (docs/CONTRACT.md section 1) plus the
// bookkeeping needed for a lossless, duplicate-free count handoff.
//
// Count handoff ("seal and reserve"): when the old process has stopped
// accepting it calls Seal, which atomically (under mu) marks the service
// sealed and returns count+inflight. Every request admitted before the seal
// will increment count exactly once (increment and inflight-- happen together
// under mu), so the old process will use exactly the numbers
// (count, count+inflight] and the new process can start at count+inflight
// without duplicates or gaps. Requests that reach the old handler after the
// seal (e.g. a connection accepted earlier whose request bytes arrive late)
// are not admitted; they are reverse-proxied to the new process, which owns
// the counter from then on.
type Service struct {
	Version string
	PID     int

	mu       sync.Mutex
	count    uint64
	inflight int64
	sealed   bool
	ready    bool
	proxy    *httputil.ReverseProxy

	// Hooks (optional).
	OnUpgrade func(to, bin string) error
}

// NewService creates a service starting at the given count.
func NewService(version string, count uint64) *Service {
	return &Service{Version: version, PID: os.Getpid(), count: count}
}

// SetReady toggles /healthz.
func (s *Service) SetReady(v bool) { s.mu.Lock(); s.ready = v; s.mu.Unlock() }

// Count returns the current counter.
func (s *Service) Count() uint64 { s.mu.Lock(); defer s.mu.Unlock(); return s.count }

// SetCount sets the counter (state import).
func (s *Service) SetCount(n uint64) { s.mu.Lock(); s.count = n; s.mu.Unlock() }

// Inflight returns the number of admitted, unfinished /api requests.
func (s *Service) Inflight() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.inflight }

// Seal stops admitting /api requests and returns the first count value the
// successor must use. Late requests are forwarded to forwardAddr (host:port)
// if non-empty, else answered 503.
func (s *Service) Seal(forwardAddr string) uint64 {
	var p *httputil.ReverseProxy
	if forwardAddr != "" {
		u := &url.URL{Scheme: "http", Host: forwardAddr}
		p = httputil.NewSingleHostReverseProxy(u)
		p.Transport = &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: 30 * time.Second}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sealed = true
	s.proxy = p
	return s.count + uint64(s.inflight)
}

// Unseal reverts Seal (rollback path).
func (s *Service) Unseal() {
	s.mu.Lock()
	s.sealed = false
	s.proxy = nil
	s.mu.Unlock()
}

// Handler returns the HTTP mux implementing the contract.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api", s.handleAPI)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		ok := s.ready && !s.sealed
		s.mu.Unlock()
		if !ok {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/admin/status", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		st := map[string]any{
			"version":  s.Version,
			"pid":      s.PID,
			"inflight": map[string]int64{s.Version: s.inflight},
			"count":    s.count,
			"sealed":   s.sealed,
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("/admin/upgrade", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if s.OnUpgrade == nil {
			http.Error(w, "upgrade not supported", http.StatusNotImplemented)
			return
		}
		if err := s.OnUpgrade(r.URL.Query().Get("to"), r.URL.Query().Get("bin")); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("upgrade started\n"))
	})
	return mux
}

func (s *Service) handleAPI(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.sealed {
		p := s.proxy
		s.mu.Unlock()
		if p == nil {
			w.Header().Set("Connection", "close")
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		r.Header.Set("X-Forwarded-By-Pid", strconv.Itoa(s.PID))
		p.ServeHTTP(w, r)
		return
	}
	s.inflight++
	s.mu.Unlock()

	if ms, _ := strconv.Atoi(r.URL.Query().Get("work_ms")); ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}

	s.mu.Lock()
	s.count++
	n := s.count
	s.inflight--
	s.mu.Unlock()

	w.Header().Set("X-Version", s.Version)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"version":%q,"pid":%d,"count":%d,"ts_ns":%d}`+"\n", s.Version, s.PID, n, time.Now().UnixNano())
}

// LoopbackAddr turns a listener address like [::]:8080 into 127.0.0.1:8080.
func LoopbackAddr(a net.Addr) string {
	if ta, ok := a.(*net.TCPAddr); ok {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(ta.Port))
	}
	return a.String()
}
