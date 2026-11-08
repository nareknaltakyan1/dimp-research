package controller

import (
	"fmt"
	"strconv"
	"time"

	v1 "github.com/nareknaltakyan1/dimp-research/operator/api/v1alpha1"
	"github.com/nareknaltakyan1/dimp-research/operator/internal/podclient"
)

// FleetVerdict is the fleet-level decision derived from per-pod states.
type FleetVerdict int

const (
	FleetContinue FleetVerdict = iota
	FleetSucceeded
	FleetRollback // some pod's in-process analysis failed: halt and revert
	FleetFailed   // a pod rejected the bundle before running it (e.g. signature)
)

// Decide derives the fleet verdict. Any analysis failure halts the whole
// fleet (one bad pod is evidence the patch is bad everywhere). A rejection
// (signature/policy) also halts; if no pod runs the patch yet there is
// nothing to roll back, so the HotPatch simply fails.
func Decide(pods []v1.PodStatus) FleetVerdict {
	if len(pods) == 0 {
		return FleetContinue
	}
	promoted, active := 0, 0
	var rejected, failed bool
	for _, p := range pods {
		switch p.Phase {
		case v1.PodFailed:
			failed = true
		case v1.PodRejected:
			rejected = true
		case v1.PodPromoted:
			promoted++
		case v1.PodWaiting:
		default:
			active++
		}
	}
	switch {
	case failed:
		return FleetRollback
	case rejected && promoted > 0:
		return FleetRollback
	case rejected:
		return FleetFailed
	case active == 0 && promoted > 0:
		return FleetSucceeded
	}
	return FleetContinue
}

// NextWave returns the indices of Pending pods to start now so that at most
// maxUnavailable pods run a rollout concurrently.
func NextWave(pods []v1.PodStatus, maxUnavailable int) []int {
	if maxUnavailable < 1 {
		maxUnavailable = 1
	}
	running := 0
	for _, p := range pods {
		if p.Phase == v1.PodPatching {
			running++
		}
	}
	var out []int
	for i, p := range pods {
		if running >= maxUnavailable {
			break
		}
		if p.Phase == v1.PodPending {
			out = append(out, i)
			running++
		}
	}
	return out
}

func parseF(s string, def float64) float64 {
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return f
}

// PodStrategy converts the CRD strategy to the in-pod schedule:
// shadow -> steps... -> promote.
func PodStrategy(s v1.Strategy) *podclient.Strategy {
	shadow := 10 * time.Second
	if s.ShadowDuration != nil {
		shadow = s.ShadowDuration.Duration
	}
	shadowSamples := int64(s.ShadowSamples)
	if shadowSamples <= 0 {
		shadowSamples = 100
	}
	steps := s.Steps
	if len(steps) == 0 {
		steps = []v1.Step{{Weight: 1}, {Weight: 10}, {Weight: 50}}
	}
	out := &podclient.Strategy{
		SLO: podclient.SLO{
			MaxErrorRateDelta: parseF(s.SLO.MaxErrorRateDelta, 0.01),
			MaxP99Factor:      parseF(s.SLO.MaxP99Factor, 1.5),
			P99Slack:          time.Duration(max(s.SLO.P99SlackMs, 2)) * time.Millisecond,
			MaxMismatchRate:   parseF(s.SLO.MaxMismatchRate, 0.001),
			FailFastSamples:   int64(s.SLO.FailFastSamples),
			WarmupSamples:     int64(s.SLO.WarmupSamples),
		},
		Interval:     100 * time.Millisecond,
		ShadowSample: 1,
		DrainTimeout: 30 * time.Second,
	}
	if out.SLO.FailFastSamples == 0 {
		out.SLO.FailFastSamples = 20
	}
	if out.SLO.WarmupSamples == 0 {
		out.SLO.WarmupSamples = 20
	}
	if shadow > 0 {
		out.Stages = append(out.Stages, podclient.Stage{Name: "shadow", Mode: "shadow", MinDuration: shadow, MaxDuration: 4*shadow + time.Minute, MinSamples: shadowSamples})
	}
	for _, st := range steps {
		if st.Weight >= 100 {
			continue
		}
		pause := 5 * time.Second
		if st.Pause != nil {
			pause = st.Pause.Duration
		}
		ms := int64(st.MinSamples)
		if ms <= 0 {
			ms = 20
		}
		out.Stages = append(out.Stages, podclient.Stage{Name: fmt.Sprintf("canary-%d", st.Weight), Mode: "canary", Weight: float64(st.Weight) / 100, MinDuration: pause, MaxDuration: 4*pause + 2*time.Minute, MinSamples: ms})
	}
	out.Stages = append(out.Stages, podclient.Stage{Name: "promote", Mode: "canary", Weight: 1})
	return out
}
