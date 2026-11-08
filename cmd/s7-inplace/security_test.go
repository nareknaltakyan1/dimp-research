package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/canary"
	"github.com/nareknaltakyan1/dimp-research/internal/canary/wasmguest"
	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/svc"
	"github.com/nareknaltakyan1/dimp-research/internal/signing"
)

type srvOpt func(*Config)

func withState(dir string) srvOpt { return func(c *Config) { c.StateDir = dir } }

// newServer starts a server; unlike newTest it can be "restarted" on the
// same state directory.
func newServer(t *testing.T, opts ...srvOpt) (*Server, *httptest.Server) {
	t.Helper()
	cfg := Config{
		Service: "s7-inplace", StateSchema: "counter/v1", ModuleDir: modDir, Initial: "v1", BundleDir: bundleDir,
		Verifier: signing.NewVerifier(pub), AdminToken: token, ContractUpgrade: true, MaxBundleAge: time.Hour,
		Wasm: wasmguest.Options{PoolSize: 2}, Events: svc.NewEventLog(&syncBuf{}), UpgradeStrategy: testStrategy(),
	}
	for _, o := range opts {
		o(&cfg)
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler(true))
	return s, ts
}

func stop(s *Server, ts *httptest.Server) { ts.Close(); s.Close() }

func adminDo(t *testing.T, method, url, tok string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := adminClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func signV(t *testing.T, version, base, module string) *signing.Bundle {
	t.Helper()
	b, err := signing.Sign(priv, signing.Manifest{Name: "s7-inplace", Version: version, BaseVersion: base, CompatibleStateSchema: "counter/v1"}, wasm[module])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pushImmediate(t *testing.T, ts *httptest.Server, b *signing.Bundle, extra map[string]any) (int, PatchRecord) {
	body := map[string]any{"bundle": envelope(t, b), "strategy_name": "immediate"}
	for k, v := range extra {
		body[k] = v
	}
	return push(t, ts.URL, token, body)
}

func waitStable(t *testing.T, s *Server, want string) {
	t.Helper()
	ctx := time.Now().Add(20 * time.Second)
	for time.Now().Before(ctx) {
		if r := s.eng.Status().Rollout; s.eng.Stable() == want && (r == nil || r.Phase != canary.PhaseProgressing) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stable %s, want %s", s.eng.Stable(), want)
}

// Regression (review 2026-11-08, High 1): a pod that promoted v3 and then
// restarted (running image code v1 again) must not accept a replay of the
// older, validly signed v2 bundle, even though v2 > v1 = running version.
// The identical promoted v3 may be restored; after an explicit rollback it
// may not, and that survives another restart.
func TestReplayOfOldBundleAfterRestartRejected(t *testing.T) {
	dir := t.TempDir()
	// The v3 module reports version "v3"; give it a non-buggy role here: the
	// test is about admission, not analysis (immediate strategy).
	v2, v3 := signV(t, "v2", "v1", "v2"), signV(t, "v3", "v2", "v3")

	s, ts := newServer(t, withState(dir))
	if code, rec := pushImmediate(t, ts, v2, nil); code != http.StatusAccepted {
		t.Fatalf("v2: %d %+v", code, rec)
	}
	waitStable(t, s, "v2")
	if code, rec := pushImmediate(t, ts, v3, nil); code != http.StatusAccepted {
		t.Fatalf("v3: %d %+v", code, rec)
	}
	waitStable(t, s, "v3")
	stop(s, ts)

	b, err := os.ReadFile(filepath.Join(dir, floorFile))
	if err != nil || !strings.Contains(string(b), `"version":"v3"`) || !strings.Contains(string(b), `"restorable":true`) {
		t.Fatalf("persisted floor: %s %v", b, err)
	}

	// Restart: image code v1 again.
	s, ts = newServer(t, withState(dir))
	if s.eng.Stable() != "v1" {
		t.Fatalf("restart should run image code, got %s", s.eng.Stable())
	}
	// Replay of the older signed v2 (base v1 == running!) is a downgrade
	// relative to the high-water mark.
	if code, rec := pushImmediate(t, ts, v2, nil); code != http.StatusConflict || rec.Reason != "VersionPolicy" {
		t.Fatalf("replay of v2 after restart: %d %+v", code, rec)
	}
	// Different code signed as v3 is not a restore.
	if code, rec := pushImmediate(t, ts, signV(t, "v3", "v2", "v2"), map[string]any{"reconcile": true}); code != http.StatusConflict {
		t.Fatalf("other code under v3: %d %+v", code, rec)
	}
	// Restoring the identical promoted v3 (operator re-patch) is allowed.
	if code, rec := pushImmediate(t, ts, v3, map[string]any{"reconcile": true}); code != http.StatusAccepted {
		t.Fatalf("restore v3: %d %+v", code, rec)
	}
	waitStable(t, s, "v3")
	// Explicit rollback to the retained artifact (image v1) ...
	if code, m := adminDo(t, http.MethodPost, ts.URL+"/admin/rollback?to=v1", token); code != http.StatusOK {
		t.Fatalf("rollback: %d %v", code, m)
	}
	waitStable(t, s, "v1")
	// ... after which v3 can never be re-installed from a bundle ...
	if code, rec := pushImmediate(t, ts, v3, map[string]any{"reconcile": true}); code != http.StatusConflict {
		t.Fatalf("replay of rolled-back v3: %d %+v", code, rec)
	}
	stop(s, ts)
	// ... not even after another restart.
	s, ts = newServer(t, withState(dir))
	defer stop(s, ts)
	if code, rec := pushImmediate(t, ts, v3, map[string]any{"reconcile": true}); code != http.StatusConflict {
		t.Fatalf("replay of rolled-back v3 after restart: %d %+v", code, rec)
	}
	// A newer version is fine.
	if code, rec := pushImmediate(t, ts, signV(t, "v4", "v3", "v2"), map[string]any{"reconcile": true}); code == http.StatusAccepted {
		t.Logf("v4 accepted (module reports v2 -> load fails is also acceptable): %+v", rec)
	} else if rec.Reason == "VersionPolicy" {
		t.Fatalf("v4 refused by version policy: %+v", rec)
	}
}

// Regression (High 1): rollback can only re-install the retained previous
// artifact; there is no request flag that admits an older signed bundle.
func TestRollbackOnlyToRetainedVersion(t *testing.T) {
	s, ts := newServer(t)
	defer stop(s, ts)
	v2, v3 := signV(t, "v2", "v1", "v2"), signV(t, "v3", "v2", "v3")
	if code, _ := adminDo(t, http.MethodPost, ts.URL+"/admin/rollback", token); code != http.StatusConflict {
		t.Fatalf("rollback with nothing retained: %d", code)
	}
	pushImmediate(t, ts, v2, nil)
	waitStable(t, s, "v2")
	pushImmediate(t, ts, v3, nil)
	waitStable(t, s, "v3")
	// Retained is v2: v1 is not a valid rollback target.
	if code, m := adminDo(t, http.MethodPost, ts.URL+"/admin/rollback?to=v1", token); code != http.StatusConflict {
		t.Fatalf("rollback to non-retained v1: %d %v", code, m)
	}
	// The old "rollback" request flag no longer exists (unknown field -> 400) ...
	v1b := signV(t, "v1", "", "v1")
	if code, rec := pushImmediate(t, ts, v1b, map[string]any{"rollback": true}); code != http.StatusBadRequest {
		t.Fatalf("rollback flag: %d %+v", code, rec)
	}
	// ... and older signed bundles are downgrades.
	for _, b := range []*signing.Bundle{v1b, v2} {
		if code, rec := pushImmediate(t, ts, b, map[string]any{"reconcile": true}); code != http.StatusConflict || rec.Reason != "VersionPolicy" {
			t.Fatalf("downgrade bundle: %d %+v", code, rec)
		}
	}
	// Digest pinning of the retained artifact.
	if code, _ := adminDo(t, http.MethodPost, ts.URL+"/admin/rollback?to=v2&digest=deadbeef", token); code != http.StatusConflict {
		t.Fatalf("rollback with wrong digest: %d", code)
	}
	if s.eng.Stable() != "v3" {
		t.Fatalf("refused rollbacks changed stable to %s", s.eng.Stable())
	}
	code, m := adminDo(t, http.MethodPost, ts.URL+"/admin/rollback?to=v2&digest="+signing.Digest(wasm["v2"]), token)
	if code != http.StatusOK || m["stable"] != "v2" {
		t.Fatalf("rollback to retained v2: %d %v", code, m)
	}
	// Only one step back: nothing is retained after a rollback.
	if code, _ := adminDo(t, http.MethodPost, ts.URL+"/admin/rollback", token); code != http.StatusConflict {
		t.Fatalf("second rollback: %d", code)
	}
}

// Regression (High 2): privileged endpoints fail closed without a token
// unless tokenless mode was explicitly enabled (main: loopback / -insecure-admin).
func TestAdminAuthFailClosed(t *testing.T) {
	s, ts := newServer(t, func(c *Config) { c.AdminToken = "" })
	defer stop(s, ts)
	for _, p := range []struct{ m, path string }{
		{http.MethodPost, "/admin/patch"}, {http.MethodGet, "/admin/rollout"}, {http.MethodPost, "/admin/rollout/abort"},
		{http.MethodPost, "/admin/rollback"}, {http.MethodPost, "/admin/upgrade?to=v2"},
	} {
		if code, _ := adminDo(t, p.m, ts.URL+p.path, ""); code != http.StatusUnauthorized {
			t.Fatalf("%s %s without token config: %d", p.m, p.path, code)
		}
		if code, _ := adminDo(t, p.m, ts.URL+p.path, "anything"); code != http.StatusUnauthorized {
			t.Fatalf("%s %s with arbitrary token: %d", p.m, p.path, code)
		}
	}
	if s.eng.Stable() != "v1" {
		t.Fatal("unauthenticated upgrade changed the running version")
	}
	// Public endpoints stay open.
	if r, err := http.Get(ts.URL + "/admin/status"); err != nil || r.StatusCode != 200 {
		t.Fatalf("status: %v", err)
	}

	// Token configured: wrong scheme / wrong token / prefix of token rejected.
	s2, ts2 := newServer(t)
	defer stop(s2, ts2)
	for _, h := range []string{"", "Basic " + token, "Bearer", "Bearer " + token[:4], "bearer " + token, token} {
		req, _ := http.NewRequest(http.MethodPost, ts2.URL+"/admin/upgrade?to=v2", nil)
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Authorization %q: %d", h, resp.StatusCode)
		}
	}
	// Explicit tokenless mode (what main sets for loopback-only listeners).
	s3, ts3 := newServer(t, func(c *Config) { c.AdminToken = ""; c.AllowTokenless = true })
	defer stop(s3, ts3)
	if code, _ := adminDo(t, http.MethodGet, ts3.URL+"/admin/rollout", ""); code != http.StatusOK {
		t.Fatalf("tokenless mode: %d", code)
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "[::1]:9090": true, "localhost:1": true, "127.1.2.3:80": true,
		":8080": false, "0.0.0.0:9090": false, "10.0.0.1:9090": false, "[::]:80": false, "bogus": false,
	} {
		if got := isLoopback(addr); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

// Regression (Medium 3 + Low): abort reports the real outcome, and the
// retained artifacts are updated synchronously with the phase transition.
func TestAbortOutcomeAndSynchronousBookkeeping(t *testing.T) {
	s, ts := newServer(t)
	defer stop(s, ts)
	// Abort a rollout stuck in shadow: 200, aborted, stable unchanged.
	strat := testStrategy()
	strat.Stages[0].MinSamples = 1 << 40
	strat.Stages[0].MaxDuration = time.Minute
	code, rec := push(t, ts.URL, token, map[string]any{"bundle": envelope(t, signV(t, "v2", "v1", "v2")), "strategy": strat})
	if code != http.StatusAccepted {
		t.Fatalf("push: %d %+v", code, rec)
	}
	code, m := adminDo(t, http.MethodPost, ts.URL+"/admin/rollout/abort?reason=test", token)
	if code != http.StatusOK || m["aborted"] != true || m["phase"] != string(canary.PhaseAborted) || m["stable"] != "v1" {
		t.Fatalf("abort: %d %v", code, m)
	}
	// Promote v2 immediately; the first observation of phase=promoted must
	// already show the new running digest and the retained previous.
	if code, rec := pushImmediate(t, ts, signV(t, "v2", "v1", "v2"), nil); code != http.StatusAccepted {
		t.Fatalf("v2 after abort (a refused candidate may be re-submitted): %d %+v", code, rec)
	}
	for {
		d := s.rolloutDoc()
		if d.Rollout != nil && d.Rollout.Phase == canary.PhasePromoted {
			if d.RunningDigest != signing.Digest(wasm["v2"]) || d.Previous != "v1" || d.HighWaterMark.Version != "v2" {
				t.Fatalf("bookkeeping lags phase: %+v", d)
			}
			break
		}
		time.Sleep(50 * time.Microsecond)
	}
	// Abort after the promotion: 409 with the real outcome.
	code, m = adminDo(t, http.MethodPost, ts.URL+"/admin/rollout/abort", token)
	if code != http.StatusConflict || m["aborted"] != false || m["stable"] != "v2" {
		t.Fatalf("late abort: %d %v", code, m)
	}
}
