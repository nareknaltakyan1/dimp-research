package canary

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Regression (review 2026-11-08, Medium 3): an abort that races the final
// promotion must never be reported as successful while the candidate ends
// up promoted. Abort()==true => outcome is Aborted/RolledBack and the stable
// version is unchanged; AbortAndWait always reports the real outcome.
func TestAbortPromotionInterleaving(t *testing.T) {
	var promotedAfterAck, lateAborts, inTime int
	for i := 0; i < 300; i++ {
		v1, v2 := &impl{name: "v1"}, &impl{name: "v2"}
		h := newHarness(v1)
		// One very short shadow stage, then promote: the decision lands within
		// a few ms of Start, so Abort calls hit before, during and after it.
		s := Strategy{
			Stages:   []Stage{{Name: "shadow", Mode: ModeShadow, MinDuration: time.Duration(i%5) * 100 * time.Microsecond, MaxDuration: time.Second}, {Name: "promote", Mode: ModeCanary, Weight: 1}},
			Interval: 50 * time.Microsecond,
		}
		if err := h.e.Start(v2.version(), s); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(i%7) * 150 * time.Microsecond)
		r, sent, err := h.e.AbortAndWait(context.Background(), "race")
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case sent && r.Phase == PhasePromoted:
			promotedAfterAck++
		case sent:
			inTime++
			if h.e.Stable() != "v1" || r.Stable != "v1" {
				t.Fatalf("aborted rollout but stable=%s report=%s", h.e.Stable(), r.Stable)
			}
		default:
			lateAborts++
			if r.Phase == PhaseProgressing {
				t.Fatalf("AbortAndWait returned a non-final phase: %+v", r)
			}
			if r.Stable != h.e.Stable() {
				t.Fatalf("reported stable %s, engine %s", r.Stable, h.e.Stable())
			}
		}
		h.e.Wait(context.Background())
	}
	if promotedAfterAck != 0 {
		t.Fatalf("%d aborts were acknowledged but the candidate was promoted", promotedAfterAck)
	}
	t.Logf("aborted in time %d, too late (reported promoted) %d", inTime, lateAborts)
}

// Regression (Medium 8): the post-decision bookkeeping of rollout A (drain,
// Finished, ReleaseMs) must not be written into rollout B started after A's
// decision but before A's dropped version drained.
func TestFinishWritesOwnRollout(t *testing.T) {
	v1, v2, v3 := &impl{name: "v1"}, &impl{name: "v2"}, &impl{name: "v3"}
	h := newHarness(v1)
	// Pin the pre-rollout plan so A's drain blocks.
	release := make(chan struct{})
	pinned := make(chan struct{})
	go h.e.Serve(context.Background(), "", in{X: 1}, func(context.Context) error {
		close(pinned)
		<-release
		return nil
	})
	<-pinned
	if err := h.e.Start(v2.version(), ImmediateStrategy()); err != nil {
		t.Fatal(err)
	}
	a, err := h.e.WaitDecided(context.Background())
	if err != nil || a.Phase != PhasePromoted {
		t.Fatalf("A: %+v %v", a, err)
	}
	s := testStrategy()
	s.Stages[0].MinSamples = 1 << 40 // B stays in shadow
	if err := h.e.Start(v3.version(), s); err != nil {
		t.Fatal(err)
	}
	close(release) // A drains now and records Finished/ReleaseMs
	deadline := time.Now().Add(5 * time.Second)
	for !v1.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !v1.closed.Load() {
		t.Fatal("A's dropped version never drained")
	}
	time.Sleep(20 * time.Millisecond)
	b := h.e.Status().Rollout
	if b.Candidate != "v3" || b.Phase != PhaseProgressing || !b.Finished.IsZero() || b.ReleaseMs != 0 {
		t.Fatalf("rollout B corrupted by A's finish: %+v", b)
	}
	h.e.Abort("done")
	h.e.Wait(context.Background())
}

// OnDecision runs before the phase becomes visible, so an observer that sees
// the final phase also sees the caller's bookkeeping, and no new rollout can
// start while the hook runs; StartFrom refuses to start against a stable
// version other than the one checked.
func TestOnDecisionOrderingAndStartFrom(t *testing.T) {
	v1, v2, v3 := &impl{name: "v1"}, &impl{name: "v2"}, &impl{name: "v3"}
	var book atomic.Value
	book.Store("v1")
	inHook, release := make(chan struct{}), make(chan struct{})
	e := New(Config[in, out]{OnDecision: func(ph Phase, keep, drop *Version[in, out]) {
		close(inHook)
		<-release
		book.Store(keep.Name)
	}}, v1.version())
	if err := e.StartFrom("v0", v2.version(), ImmediateStrategy()); !errors.Is(err, ErrStableChanged) {
		t.Fatalf("StartFrom with stale expectation: %v", err)
	}
	if err := e.StartFrom("v1", v2.version(), ImmediateStrategy()); err != nil {
		t.Fatal(err)
	}
	<-inHook
	// Inside the hook window: the final plan is published (traffic on v2)
	// but the phase is not yet final and new rollouts are refused.
	if r := e.Status().Rollout; r.Phase != PhaseProgressing {
		t.Fatalf("phase %s visible before the hook returned", r.Phase)
	}
	if err := e.Start(v3.version(), ImmediateStrategy()); !errors.Is(err, ErrBusy) {
		t.Fatalf("Start during the hook: %v", err)
	}
	if e.Abort("late") {
		t.Fatal("abort after the decision must be refused")
	}
	close(release)
	r, err := e.WaitDecided(context.Background())
	if err != nil || r.Phase != PhasePromoted || book.Load() != "v2" {
		t.Fatalf("after hook: %+v book=%v err=%v", r, book.Load(), err)
	}
	e.Wait(context.Background())
}
