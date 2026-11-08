package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/canary"
	"github.com/nareknaltakyan1/dimp-research/internal/canary/wasmguest"
	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/svc"
	"github.com/nareknaltakyan1/dimp-research/internal/signing"
)

var (
	modDir    string
	bundleDir string
	pub       ed25519.PublicKey
	priv      ed25519.PrivateKey
	wasm      = map[string][]byte{}
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "s7mods")
	if err != nil {
		panic(err)
	}
	modDir = filepath.Join(dir, "modules")
	bundleDir = filepath.Join(dir, "bundles")
	os.MkdirAll(modDir, 0o755)
	for _, v := range []string{"v1", "v2", "v3"} {
		out := filepath.Join(modDir, v+".wasm")
		cmd := exec.Command("go", "build", "-buildmode=c-shared", "-ldflags=-s -w", "-o", out, "./guest/"+v)
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if b, err := cmd.CombinedOutput(); err != nil {
			panic(string(b))
		}
		wasm[v], _ = os.ReadFile(out)
	}
	pub, priv, _ = signing.GenerateKey()
	for v, base := range map[string]string{"v2": "v1", "v3": "v2"} {
		b, err := signing.Sign(priv, signing.Manifest{Name: "s7-inplace", Version: v, BaseVersion: base, CompatibleStateSchema: "counter/v1"}, wasm[v])
		if err != nil {
			panic(err)
		}
		if err := b.WriteDir(filepath.Join(bundleDir, v)); err != nil {
			panic(err)
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

const token = "test-token"

func newTest(t *testing.T) (*Server, *httptest.Server, *syncBuf) {
	t.Helper()
	ev := &syncBuf{}
	s, err := NewServer(Config{
		Service: "s7-inplace", StateSchema: "counter/v1", ModuleDir: modDir, Initial: "v1", BundleDir: bundleDir,
		Verifier: signing.NewVerifier(pub), AdminToken: token, ContractUpgrade: true,
		Wasm: wasmguest.Options{PoolSize: 4}, Events: svc.NewEventLog(ev), UpgradeStrategy: testStrategy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler(true))
	t.Cleanup(func() { ts.Close(); s.Close() })
	return s, ts, ev
}

func testStrategy() canary.Strategy {
	s := canary.FastStrategy()
	// Race-detector builds are slow and jittery: judge latency loosely here
	// (the unit tests in internal/canary cover the latency criterion).
	s.SLO.P99Slack = 50 * time.Millisecond
	s.SLO.MaxP99Factor = 10
	return s
}

func price(x int64) int64 { return (31*x + 7) % 1000003 }

type loadStats struct {
	total, errs, wrong atomic.Int64
	mu                 sync.Mutex
	counts             []uint64
	versions           map[string]int
}

var client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64, MaxIdleConns: 64}}

// adminClient is used for admin calls: a push compiles a WASM module and
// pre-warms its pools, which takes several seconds under -race on a busy
// machine (Linux CI container), far above the load client's 10 s budget.
var adminClient = &http.Client{Timeout: 90 * time.Second}

func runLoad(url string, workers int, stop <-chan struct{}) (*loadStats, *sync.WaitGroup) {
	ls := &loadStats{versions: map[string]int{}}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				x := rand.Int64N(1_000_000)
				r, err := client.Get(fmt.Sprintf("%s/api?x=%d&work_ms=1", url, x))
				ls.total.Add(1)
				if err != nil {
					ls.errs.Add(1)
					continue
				}
				body, _ := io.ReadAll(r.Body)
				r.Body.Close()
				if r.StatusCode != 200 {
					ls.errs.Add(1)
					continue
				}
				var a struct {
					Version string `json:"version"`
					Count   uint64 `json:"count"`
					Result  int64  `json:"result"`
				}
				if json.Unmarshal(body, &a) != nil || a.Result != price(x) {
					ls.wrong.Add(1)
				}
				ls.mu.Lock()
				ls.counts = append(ls.counts, a.Count)
				ls.versions[a.Version]++
				ls.mu.Unlock()
			}
		}()
	}
	return ls, &wg
}

func waitPhase(t *testing.T, s *Server, want ...canary.Phase) canary.Rollout {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if r := s.eng.Status().Rollout; r != nil {
			for _, w := range want {
				if r.Phase == w {
					return *r
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("rollout did not reach %v: %+v", want, s.eng.Status().Rollout)
	return canary.Rollout{}
}

func checkCounts(t *testing.T, ls *loadStats) {
	t.Helper()
	seen := map[uint64]bool{}
	for _, c := range ls.counts {
		if seen[c] {
			t.Fatalf("duplicate count %d (state not shared across versions)", c)
		}
		seen[c] = true
	}
}

// Contract harness path: /admin/upgrade?to=v2 under load -> promoted, zero
// failed requests, continuous counter, contract events.
func TestContractUpgradeUnderLoad(t *testing.T) {
	s, ts, ev := newTest(t)
	stop := make(chan struct{})
	ls, wg := runLoad(ts.URL, 8, stop)
	time.Sleep(200 * time.Millisecond)
	// The contract endpoint is privileged: without the token it is refused.
	if r, err := http.Post(ts.URL+"/admin/upgrade?to=v2", "", nil); err != nil || r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated upgrade: %v %v", err, r.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/upgrade?to=v2", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r, err := http.DefaultClient.Do(req)
	if err != nil || r.StatusCode != http.StatusAccepted {
		t.Fatalf("upgrade: %v %v", err, r.StatusCode)
	}
	ro := waitPhase(t, s, canary.PhasePromoted, canary.PhaseRolledBack)
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	if ro.Phase != canary.PhasePromoted {
		t.Fatalf("v2 not promoted: %s\nevents:\n%s", ro.Reason, ev.String())
	}
	if ls.errs.Load() != 0 || ls.wrong.Load() != 0 {
		t.Fatalf("errors %d wrong %d", ls.errs.Load(), ls.wrong.Load())
	}
	checkCounts(t, ls)
	var st StatusDoc
	resp, _ := http.Get(ts.URL + "/admin/status")
	json.NewDecoder(resp.Body).Decode(&st)
	if st.Version != "v2" || st.Count < uint64(len(ls.counts)) || st.RunningDigest != signing.Digest(wasm["v2"]) {
		t.Fatalf("status %+v", st)
	}
	for _, e := range []string{"upgrade_start", "new_version_serving", "old_version_drained", "upgrade_done"} {
		if !strings.Contains(ev.String(), `"ev":"`+e+`"`) {
			t.Fatalf("missing event %s", e)
		}
	}
	t.Logf("requests %d versions %v decision %.0fms", ls.total.Load(), ls.versions, ro.DecisionMs)
}

func push(t *testing.T, url, tok string, body any) (int, PatchRecord) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url+"/admin/patch", bytes.NewReader(b))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := adminClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rec PatchRecord
	json.NewDecoder(resp.Body).Decode(&rec)
	return resp.StatusCode, rec
}

func envelope(t *testing.T, b *signing.Bundle) json.RawMessage {
	enc, err := b.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func readBundle(t *testing.T, v string) *signing.Bundle {
	b, err := signing.ReadDir(filepath.Join(bundleDir, v), 0)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The buggy v3 (wrong results for x%10==3, +4ms) is pushed as a signed
// patch while v2 serves: shadow catches it, it is rolled back, and clients
// see zero errors and zero wrong answers.
func TestBuggyPatchCaughtInShadow(t *testing.T) {
	s, ts, ev := newTest(t)
	// Setup without traffic: a staged rollout would (correctly) fail closed
	// for lack of samples, so install v2 immediately.
	if _, err := s.admit(readBundle(t, "v2"), canary.ImmediateStrategy(), signing.CheckOptions{}, "test"); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, s, canary.PhasePromoted)
	stop := make(chan struct{})
	ls, wg := runLoad(ts.URL, 8, stop)
	time.Sleep(100 * time.Millisecond)
	code, rec := push(t, ts.URL, token, map[string]any{"bundle": envelope(t, readBundle(t, "v3")), "strategy_name": "fast"})
	if code != http.StatusAccepted {
		t.Fatalf("push v3: %d %+v", code, rec)
	}
	ro := waitPhase(t, s, canary.PhaseRolledBack, canary.PhasePromoted)
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	if ro.Phase != canary.PhaseRolledBack || s.eng.Stable() != "v2" {
		t.Fatalf("v3 should be rolled back: %+v", ro)
	}
	if len(ro.Stages) != 1 || ro.Stages[0].Mode != canary.ModeShadow {
		t.Fatalf("should be caught in shadow: %+v", ro.Stages)
	}
	if ls.errs.Load() != 0 || ls.wrong.Load() != 0 || ls.versions["v3"] != 0 {
		t.Fatalf("client impact: errs %d wrong %d v3-served %d", ls.errs.Load(), ls.wrong.Load(), ls.versions["v3"])
	}
	checkCounts(t, ls)
	if !strings.Contains(ev.String(), "shadow_mismatch") {
		t.Fatal("expected a shadow_mismatch event")
	}
	t.Logf("v3 rolled back after %.1fms: %s (verify %.2fms compile %.0fms)", ro.DecisionMs, ro.Reason, rec.VerifyMs, rec.CompileMs)
}

func TestPatchAdmissionSecurity(t *testing.T) {
	s, ts, _ := newTest(t)
	v2 := readBundle(t, "v2")

	// No / wrong token.
	if code, _ := push(t, ts.URL, "", map[string]any{"bundle": envelope(t, v2)}); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := push(t, ts.URL, "nope", map[string]any{"bundle": envelope(t, v2)}); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", code)
	}
	// Tampered: v3 code substituted under v2's signed manifest.
	evil := *v2
	evil.Module = wasm["v3"]
	if code, rec := push(t, ts.URL, token, map[string]any{"bundle": envelope(t, &evil)}); code != http.StatusForbidden || rec.Reason != "SignatureInvalid" {
		t.Fatalf("tampered: %d %+v", code, rec)
	}
	// Signed by an untrusted key.
	_, other, _ := signing.GenerateKey()
	rogue, _ := signing.Sign(other, signing.Manifest{Name: "s7-inplace", Version: "v2", BaseVersion: "v1", CompatibleStateSchema: "counter/v1"}, wasm["v2"])
	if code, rec := push(t, ts.URL, token, map[string]any{"bundle": envelope(t, rogue)}); code != http.StatusForbidden {
		t.Fatalf("rogue key: %d %+v", code, rec)
	}
	// Wrong target service.
	wrong, _ := signing.Sign(priv, signing.Manifest{Name: "payments", Version: "v2", BaseVersion: "v1", CompatibleStateSchema: "counter/v1"}, wasm["v2"])
	if code, rec := push(t, ts.URL, token, map[string]any{"bundle": envelope(t, wrong)}); code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong target: %d %+v", code, rec)
	}
	// Signed manifest whose version disagrees with the module's own version.
	lie, _ := signing.Sign(priv, signing.Manifest{Name: "s7-inplace", Version: "v4", BaseVersion: "v1", CompatibleStateSchema: "counter/v1"}, wasm["v2"])
	if code, rec := push(t, ts.URL, token, map[string]any{"bundle": envelope(t, lie)}); code != http.StatusUnprocessableEntity {
		t.Fatalf("version lie: %d %+v", code, rec)
	}
	if s.eng.Stable() != "v1" {
		t.Fatal("rejected patches must not change the running version")
	}
	// Valid patch (immediate).
	if code, rec := push(t, ts.URL, token, map[string]any{"bundle": envelope(t, v2), "strategy_name": "immediate"}); code != http.StatusAccepted {
		t.Fatalf("valid: %d %+v", code, rec)
	}
	waitPhase(t, s, canary.PhasePromoted)
	// Replay of the same signed bundle.
	if code, rec := push(t, ts.URL, token, map[string]any{"bundle": envelope(t, v2)}); code != http.StatusConflict || rec.Reason != "VersionPolicy" {
		t.Fatalf("replay: %d %+v", code, rec)
	}
	// Downgrade to an older signed version without explicit rollback.
	v1b, _ := signing.Sign(priv, signing.Manifest{Name: "s7-inplace", Version: "v1", CompatibleStateSchema: "counter/v1"}, wasm["v1"])
	if code, _ := push(t, ts.URL, token, map[string]any{"bundle": envelope(t, v1b)}); code != http.StatusConflict {
		t.Fatalf("downgrade: %d", code)
	}
	// Explicit rollback of the retained version (operator fleet rollback).
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/rollback?to=v1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("rollback: %v %d", err, resp.StatusCode)
	}
	if s.eng.Stable() != "v1" {
		t.Fatalf("after rollback stable=%s", s.eng.Stable())
	}
	// Audit trail.
	resp, _ = http.Get(ts.URL + "/admin/rollout")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rollout status must need the token: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/admin/rollout", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ = http.DefaultClient.Do(req)
	var doc RolloutDoc
	json.NewDecoder(resp.Body).Decode(&doc)
	if len(doc.Patches) < 8 {
		t.Fatalf("expected audit records, got %d", len(doc.Patches))
	}
}
