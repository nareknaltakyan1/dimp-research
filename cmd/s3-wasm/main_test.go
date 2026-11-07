package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/hotswap"
	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/svc"
	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/wasmimpl"
)

var modDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "s3mods")
	if err != nil {
		panic(err)
	}
	modDir = dir
	for _, v := range []string{"v1", "v2"} {
		cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", filepath.Join(dir, v+".wasm"), "./guest/"+v)
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if b, err := cmd.CombinedOutput(); err != nil {
			panic(string(b))
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type apiResp struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
	Count   uint64 `json:"count"`
	TS      int64  `json:"ts_ns"`
}

func TestWasmUpgradeUnderLoad(t *testing.T) {
	events := &syncBuf{}
	s, err := newServer(modDir, "v1", wasmimpl.Options{PoolSize: 4}, 10*time.Second, svc.NewEventLog(events))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	old := s.Reg.Current().Impl.(*wasmimpl.Module)

	var stop atomic.Bool
	var mu sync.Mutex
	var resps []apiResp
	var errs atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				r, err := http.Get(ts.URL + "/api?work_ms=20")
				if err != nil || r.StatusCode != 200 {
					errs.Add(1)
					continue
				}
				var a apiResp
				if json.NewDecoder(r.Body).Decode(&a) != nil || r.Header.Get("X-Version") != a.Version || a.PID != os.Getpid() {
					errs.Add(1)
				}
				r.Body.Close()
				mu.Lock()
				resps = append(resps, a)
				mu.Unlock()
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	if err := s.Upgrade("v2"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	if errs.Load() != 0 {
		t.Fatalf("%d failed requests", errs.Load())
	}
	sort.Slice(resps, func(i, j int) bool { return resps[i].Count < resps[j].Count })
	seen := map[string]int{}
	for i, a := range resps {
		seen[a.Version]++
		if a.Count != uint64(i+1) {
			t.Fatalf("count sequence broken at %d: %d", i, a.Count)
		}
	}
	if seen["v1"] == 0 || seen["v2"] == 0 || resps[len(resps)-1].Version != "v2" {
		t.Fatalf("bad distribution %v", seen)
	}
	// The old module was unloaded after draining: using it now must fail.
	rec := httptest.NewRecorder()
	old.ServeHTTP(rec, httptest.NewRequest("GET", "/api", nil))
	if rec.Code == 200 {
		t.Fatal("old module still usable after unload")
	}
	t.Logf("per-version=%v created-instances(v1)=%d events:\n%s", seen, old.Created.Load(), events.String())
}

func TestLoadRejectsInvalidModules(t *testing.T) {
	// A module without the right exports must be rejected at load time.
	if _, err := wasmimpl.Load(context.Background(), "bad", []byte("\x00asm\x01\x00\x00\x00"), wasmimpl.Options{}); err == nil {
		t.Fatal("expected load error for module without exports")
	}
	if _, err := wasmimpl.Load(context.Background(), "garbage", []byte("nope"), wasmimpl.Options{}); err == nil {
		t.Fatal("expected compile error")
	}
}

func TestBadUpgradeKeepsServing(t *testing.T) {
	events := &syncBuf{}
	s, err := newServer(modDir, "v1", wasmimpl.Options{PoolSize: 2}, time.Second, svc.NewEventLog(events))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Upgrade("does-not-exist"); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(events.String(), "upgrade_failed") || s.Status().Version != "v1" {
		t.Fatalf("expected upgrade_failed, still v1: %s", events.String())
	}
}

func loadBench(b *testing.B, interp bool) *hotswap.Registry[svc.Impl] {
	data, err := os.ReadFile(filepath.Join(modDir, "v1.wasm"))
	if err != nil {
		b.Fatal(err)
	}
	m, err := wasmimpl.Load(context.Background(), "v1", data, wasmimpl.Options{PoolSize: 16, Interpreter: interp})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { m.Close() })
	m.ImportState(new(atomic.Uint64))
	return hotswap.New[svc.Impl]("v1", m)
}

type nopWriter struct{ h http.Header }

func (w *nopWriter) Header() http.Header         { return w.h }
func (w *nopWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *nopWriter) WriteHeader(int)             {}

// BenchmarkWasmHandle: per-request cost of a guest call (compiler).
func BenchmarkWasmHandle(b *testing.B) {
	reg := loadBench(b, false)
	req := httptest.NewRequest("GET", "/api", nil)
	b.RunParallel(func(pb *testing.PB) {
		w := &nopWriter{h: http.Header{}}
		for pb.Next() {
			v := reg.Acquire()
			v.Impl.ServeHTTP(w, req)
			v.Release()
		}
	})
}

func BenchmarkWasmHandleInterpreter(b *testing.B) {
	reg := loadBench(b, true)
	req := httptest.NewRequest("GET", "/api", nil)
	b.RunParallel(func(pb *testing.PB) {
		w := &nopWriter{h: http.Header{}}
		for pb.Next() {
			v := reg.Acquire()
			v.Impl.ServeHTTP(w, req)
			v.Release()
		}
	})
}

// BenchmarkWasmLoad: cost of compiling + instantiating one module.
func BenchmarkWasmLoad(b *testing.B) {
	data, err := os.ReadFile(filepath.Join(modDir, "v2.wasm"))
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		m, err := wasmimpl.Load(context.Background(), "v2", data, wasmimpl.Options{PoolSize: 1})
		if err != nil {
			b.Fatal(err)
		}
		m.Close()
	}
}
