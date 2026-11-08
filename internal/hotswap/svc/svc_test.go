package svc

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/hotswap"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type ver struct{ name string }

func (v *ver) ServeHTTP(w http.ResponseWriter, _ *http.Request) { WriteAPIResponse(w, v.name, 1) }

// Regression (review, Low): a drain timeout is not a failed upgrade. The new
// version is serving and nothing is rolled back, so the server must emit
// "drain_timeout" (+ upgrade_done with drained=false), never "upgrade_failed",
// and must unload the old version only after it really drained.
func TestDrainTimeoutIsNotUpgradeFailed(t *testing.T) {
	buf := &syncBuf{}
	var unloaded atomic.Int32
	s := New(Config{
		InitialName:  "v1",
		Initial:      &ver{"v1"},
		Load:         func(to string) (Impl, error) { return &ver{to}, nil },
		DrainTimeout: 50 * time.Millisecond,
		Events:       NewEventLog(buf),
		Unload:       func(string, Impl) { unloaded.Add(1) },
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if r, err := http.Get(ts.URL + "/api?work_ms=400"); err == nil {
			r.Body.Close()
		}
	}()
	for s.Reg.Current().Refs() == 0 {
		time.Sleep(time.Millisecond)
	}
	err := s.Upgrade("v2")
	if !errors.Is(err, hotswap.ErrDrainTimeout) {
		t.Fatalf("want ErrDrainTimeout, got %v", err)
	}
	log := buf.String()
	if strings.Contains(log, `"ev":"upgrade_failed"`) {
		t.Fatalf("drain timeout reported as upgrade_failed:\n%s", log)
	}
	if !strings.Contains(log, `"ev":"drain_timeout"`) || !strings.Contains(log, `"drained":false`) {
		t.Fatalf("missing drain_timeout / upgrade_done drained=false:\n%s", log)
	}
	if unloaded.Load() != 0 {
		t.Fatal("old version unloaded while a request still ran on it")
	}
	<-done
	deadline := time.Now().Add(2 * time.Second)
	for unloaded.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if unloaded.Load() != 1 || !strings.Contains(buf.String(), `"ev":"old_version_drained"`) {
		t.Fatalf("old version not drained+unloaded after the straggler finished (unloads=%d):\n%s", unloaded.Load(), buf.String())
	}
}

// A load failure is still upgrade_failed, and the loaded-but-unpublished
// implementation is unloaded immediately.
func TestProbeFailureUnloadsAndFails(t *testing.T) {
	buf := &syncBuf{}
	var unloaded atomic.Int32
	s := New(Config{
		InitialName: "v1", Initial: &ver{"v1"},
		Load:   func(to string) (Impl, error) { return &ver{to}, nil },
		Probe:  func(Impl) error { return errors.New("unhealthy") },
		Events: NewEventLog(buf),
		Unload: func(string, Impl) { unloaded.Add(1) },
	})
	if err := s.Upgrade("v2"); !errors.Is(err, hotswap.ErrProbe) {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"ev":"upgrade_failed"`) || unloaded.Load() != 1 || s.Reg.Current().Name != "v1" {
		t.Fatalf("unloads=%d cur=%s log=%s", unloaded.Load(), s.Reg.Current().Name, buf.String())
	}
}
