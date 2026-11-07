package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/pluginimpl"
	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/svc"
)

var pluginDir string

// buildPlugin compiles a plugin with the same flags as this test binary.
func buildPlugin(t testing.TB, pkg, out string, ldflags ...string) {
	t.Helper()
	ld := strings.Join(ldflags, " ")
	if runtime.GOOS == "darwin" {
		ld = "-extldflags=-Wl,-no_fixup_chains " + ld
	}
	args := []string{"build", "-buildmode=plugin", "-ldflags", ld, "-o", out}
	if raceEnabled {
		args = append(args, "-race")
	}
	cmd := exec.Command("go", append(args, pkg)...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
}

func setup(t *testing.T) string {
	t.Helper()
	if !pluginimpl.Supported {
		t.Skip("plugins unsupported in this build")
	}
	if pluginDir != "" {
		return pluginDir
	}
	dir, err := os.MkdirTemp("", "s2plugins")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v1", "v2"} {
		buildPlugin(t, "./plugins/"+v, filepath.Join(dir, v+".so"))
	}
	pluginDir = dir
	return dir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if pluginDir != "" {
		os.RemoveAll(pluginDir)
	}
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
	Count   uint64 `json:"count"`
}

func TestPluginUpgradeUnderLoad(t *testing.T) {
	dir := setup(t)
	events := &syncBuf{}
	s, _, err := newServer(dir, "v1", 10*time.Second, svc.NewEventLog(events))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	var stop atomic.Bool
	var mu sync.Mutex
	var resps []apiResp
	var errs atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				r, err := http.Get(ts.URL + "/api?work_ms=10")
				if err != nil || r.StatusCode != 200 {
					errs.Add(1)
					continue
				}
				var a apiResp
				if json.NewDecoder(r.Body).Decode(&a) != nil || r.Header.Get("X-Version") != a.Version {
					errs.Add(1)
				}
				r.Body.Close()
				mu.Lock()
				resps = append(resps, a)
				mu.Unlock()
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)
	if err := s.Upgrade("v2"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
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
		t.Fatalf("bad version distribution %v", seen)
	}
	if !strings.Contains(events.String(), "upgrade_done") {
		t.Fatalf("no upgrade_done:\n%s", events.String())
	}
	t.Logf("per-version=%v events:\n%s", seen, events.String())
}

// TestSamePluginPathRejected documents the "plugin already loaded"
// limitation (a rebuilt, modified plugin with the same package path cannot be
// loaded) and the unique-import-path workaround used for generations.
func TestSamePluginPathRejected(t *testing.T) {
	dir := setup(t)
	a := filepath.Join(dir, "same_a.so")
	c := filepath.Join(dir, "unique_c.so")
	buildPlugin(t, "./plugins/v2", a, "-X main.Version=a")
	// Unique import path: copy the source into its own package directory.
	gen, err := os.MkdirTemp(".", "_gentest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(gen)
	src, err := os.ReadFile("plugins/v2/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gen, "main.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	buildPlugin(t, "./"+gen, c, "-X main.Version=c")

	// v2.so has the same package path as same_a.so; whichever opens first wins.
	if _, err := pluginimpl.Open(filepath.Join(dir, "v2.so")); err != nil {
		t.Fatal(err)
	}
	_, err = pluginimpl.Open(a)
	if err == nil || !strings.Contains(err.Error(), "already loaded") {
		t.Fatalf("expected 'plugin already loaded', got %v", err)
	}
	t.Logf("modified plugin with same package path: %v", fmt.Sprint(err))
	l, err := pluginimpl.Open(c)
	if err != nil || l.Version != "c" {
		t.Fatalf("unique import path should load: %v", err)
	}
}
