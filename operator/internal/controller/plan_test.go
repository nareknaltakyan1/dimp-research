package controller

import (
	"testing"
	"time"

	v1 "github.com/nareknaltakyan1/dimp-research/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pods(phases ...string) []v1.PodStatus {
	var out []v1.PodStatus
	for _, p := range phases {
		out = append(out, v1.PodStatus{Phase: p})
	}
	return out
}

func TestDecide(t *testing.T) {
	cases := []struct {
		in   []v1.PodStatus
		want FleetVerdict
	}{
		{pods(), FleetContinue},
		{pods(v1.PodPending, v1.PodPending), FleetContinue},
		{pods(v1.PodPromoted, v1.PodPatching, v1.PodPending), FleetContinue},
		{pods(v1.PodPromoted, v1.PodPromoted, v1.PodPromoted), FleetSucceeded},
		{pods(v1.PodPromoted, v1.PodPromoted, v1.PodWaiting), FleetSucceeded},
		{pods(v1.PodPromoted, v1.PodFailed, v1.PodPending), FleetRollback},
		{pods(v1.PodFailed, v1.PodPending, v1.PodPending), FleetRollback},
		{pods(v1.PodRejected, v1.PodPending), FleetFailed},
		{pods(v1.PodPromoted, v1.PodRejected), FleetRollback},
	}
	for i, c := range cases {
		if got := Decide(c.in); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestNextWave(t *testing.T) {
	p := pods(v1.PodPromoted, v1.PodPatching, v1.PodPending, v1.PodPending, v1.PodPending)
	if w := NextWave(p, 1); len(w) != 0 {
		t.Fatalf("wave full, got %v", w)
	}
	if w := NextWave(p, 3); len(w) != 2 || w[0] != 2 || w[1] != 3 {
		t.Fatalf("got %v", w)
	}
	if w := NextWave(pods(v1.PodPending, v1.PodPending), 0); len(w) != 1 {
		t.Fatalf("maxUnavailable<1 treated as 1, got %v", w)
	}
}

func TestPodStrategy(t *testing.T) {
	s := PodStrategy(v1.Strategy{
		ShadowDuration: &metav1.Duration{Duration: 3 * time.Second},
		Steps:          []v1.Step{{Weight: 10}, {Weight: 50}, {Weight: 100}},
		SLO:            v1.SLO{MaxMismatchRate: "0", MaxP99Factor: "2"},
	})
	names := []string{}
	for _, st := range s.Stages {
		names = append(names, st.Name)
	}
	if len(s.Stages) != 4 || s.Stages[0].Mode != "shadow" || s.Stages[1].Weight != 0.1 || s.Stages[3].Weight != 1 {
		t.Fatalf("stages %v", names)
	}
	if s.SLO.MaxMismatchRate != 0 || s.SLO.MaxP99Factor != 2 || s.SLO.MaxErrorRateDelta != 0.01 {
		t.Fatalf("slo %+v", s.SLO)
	}
}
