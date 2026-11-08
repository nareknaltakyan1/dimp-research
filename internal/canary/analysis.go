package canary

import (
	"fmt"
	"time"
)

// Mode is how a stage exposes the candidate.
type Mode string

const (
	// ModeShadow: every client request is served by stable; a sample is
	// mirrored asynchronously to the candidate and the responses are diffed.
	// The candidate is invisible to clients.
	ModeShadow Mode = "shadow"
	// ModeCanary: a Weight fraction of client requests is served by the
	// candidate (client-visible).
	ModeCanary Mode = "canary"
)

// Stage is one step of a rollout.
type Stage struct {
	Name string `json:"name"`
	Mode Mode   `json:"mode"`
	// Weight is the fraction (0..1] of requests routed to the candidate in
	// ModeCanary. A canary stage with Weight >= 1 is the promotion step.
	Weight float64 `json:"weight"`
	// MinDuration and MinSamples must both be reached before the stage can
	// pass. MaxDuration bounds the stage; if it elapses without enough
	// samples the rollout fails closed (rolls back).
	MinDuration time.Duration `json:"min_duration"`
	MaxDuration time.Duration `json:"max_duration"`
	MinSamples  int64         `json:"min_samples"`
}

// SLO is the promotion criterion, evaluated per stage on that stage's window.
//
//	errRate(cand)  <= errRate(stable) + MaxErrorRateDelta
//	p99(cand)      <= max(p99(stable) * MaxP99Factor, p99(stable) + P99Slack)
//	mismatchRate   <= MaxMismatchRate                (shadow stages)
type SLO struct {
	MaxErrorRateDelta float64       `json:"max_error_rate_delta"`
	MaxP99Factor      float64       `json:"max_p99_factor"`
	P99Slack          time.Duration `json:"p99_slack"`
	MaxMismatchRate   float64       `json:"max_mismatch_rate"`
	// FailFastSamples: once the candidate has this many samples in a stage,
	// error-rate and mismatch violations roll back immediately (before
	// MinSamples). Latency is only judged at full evaluation (p99 of a small
	// sample is noise). 0 disables fail-fast.
	FailFastSamples int64 `json:"fail_fast_samples"`
	// WarmupSamples: the first N calls a candidate version receives (shadow
	// or client) are excluded from its LATENCY histogram (errors and
	// mismatches still count). A freshly loaded version pays one-off costs
	// (lazy instance creation, JIT, cold caches) that a warm stable does not;
	// without this, a correct patch is rolled back for being cold.
	WarmupSamples int64 `json:"warmup_samples"`
}

// Strategy is a complete rollout schedule.
type Strategy struct {
	Stages []Stage `json:"stages"`
	SLO    SLO     `json:"slo"`
	// Interval is the analysis period (default 100 ms).
	Interval time.Duration `json:"interval"`
	// ShadowSample is the fraction of stable responses mirrored during a
	// shadow stage (default 1.0).
	ShadowSample float64 `json:"shadow_sample"`
	// DrainTimeout bounds the wait for old plans before unloading a version
	// (it is unloaded later in the background if exceeded).
	DrainTimeout time.Duration `json:"drain_timeout"`
}

// DefaultSLO is a conservative SLO.
func DefaultSLO() SLO {
	return SLO{MaxErrorRateDelta: 0.01, MaxP99Factor: 1.5, P99Slack: 2 * time.Millisecond, MaxMismatchRate: 0.001, FailFastSamples: 20, WarmupSamples: 20}
}

// DefaultStrategy is shadow -> 1% -> 10% -> 50% -> 100%.
func DefaultStrategy() Strategy {
	st := func(name string, m Mode, w float64) Stage {
		return Stage{Name: name, Mode: m, Weight: w, MinDuration: 10 * time.Second, MaxDuration: 5 * time.Minute, MinSamples: 200}
	}
	return Strategy{
		Stages: []Stage{
			st("shadow", ModeShadow, 0),
			st("canary-1", ModeCanary, 0.01),
			st("canary-10", ModeCanary, 0.10),
			st("canary-50", ModeCanary, 0.50),
			{Name: "promote", Mode: ModeCanary, Weight: 1},
		},
		SLO:          DefaultSLO(),
		Interval:     100 * time.Millisecond,
		ShadowSample: 1,
		DrainTimeout: 30 * time.Second,
	}
}

// FastStrategy is used by the contract harness (/admin/upgrade): the same
// stages on a sub-second-per-stage schedule.
func FastStrategy() Strategy {
	s := DefaultStrategy()
	for i := range s.Stages {
		if s.Stages[i].Weight >= 1 {
			continue
		}
		s.Stages[i].MinDuration = 200 * time.Millisecond
		s.Stages[i].MaxDuration = 10 * time.Second
		s.Stages[i].MinSamples = 20
	}
	// Shadow sees every request: demand enough samples for a meaningful p99.
	s.Stages[0].MinSamples = 100
	// 1% of a few hundred rps is a handful of requests; keep the step but do
	// not demand many samples from it.
	s.Stages[1].MinSamples = 3
	s.Interval = 20 * time.Millisecond
	return s
}

// ImmediateStrategy promotes without analysis (re-patching a fresh pod with
// a version the fleet has already validated, or an operator rollback).
func ImmediateStrategy() Strategy {
	return Strategy{Stages: []Stage{{Name: "promote", Mode: ModeCanary, Weight: 1}}, Interval: 10 * time.Millisecond, DrainTimeout: 30 * time.Second}
}

func (s *Strategy) normalize() error {
	if len(s.Stages) == 0 {
		return fmt.Errorf("canary: strategy has no stages")
	}
	for i, st := range s.Stages {
		switch st.Mode {
		case ModeShadow, ModeCanary:
		case "":
			s.Stages[i].Mode = ModeCanary
		default:
			return fmt.Errorf("canary: stage %d: unknown mode %q", i, st.Mode)
		}
		if st.Weight < 0 || st.Weight > 1 {
			return fmt.Errorf("canary: stage %d: weight %v out of [0,1]", i, st.Weight)
		}
		if st.Name == "" {
			s.Stages[i].Name = fmt.Sprintf("stage-%d", i)
		}
	}
	if s.Interval <= 0 {
		s.Interval = 100 * time.Millisecond
	}
	if s.ShadowSample <= 0 || s.ShadowSample > 1 {
		s.ShadowSample = 1
	}
	if s.DrainTimeout <= 0 {
		s.DrainTimeout = 30 * time.Second
	}
	if s.SLO.MaxP99Factor <= 0 {
		s.SLO.MaxP99Factor = 1.5
	}
	return nil
}

// Verdict of one analysis step.
type Verdict int

const (
	Continue Verdict = iota // not enough evidence yet
	Pass                    // stage passed: advance
	Fail                    // SLO violated: roll back
)

func (v Verdict) String() string {
	return [...]string{"continue", "pass", "fail"}[v]
}

// Decision is the result of Analyze.
type Decision struct {
	Verdict Verdict
	Reason  string
}

// Analyze evaluates one stage window. stable is the stable version's window;
// cand is the candidate's (the shadow mirror metrics in a shadow stage, the
// client-visible candidate metrics in a canary stage). It is a pure function.
func Analyze(st Stage, slo SLO, stable, cand Snapshot, elapsed time.Duration) Decision {
	shadow := st.Mode == ModeShadow
	errLimit := stable.ErrorRate + slo.MaxErrorRateDelta
	errViolation := cand.Count > 0 && cand.ErrorRate > errLimit
	mmViolation := shadow && cand.Count > 0 && cand.MismatchRate > slo.MaxMismatchRate

	// Fail fast on correctness signals.
	if slo.FailFastSamples > 0 && cand.Count >= slo.FailFastSamples {
		if errViolation {
			return Decision{Fail, fmt.Sprintf("error rate %.4f > %.4f (stable %.4f + %.4f) after %d samples [fail-fast]", cand.ErrorRate, errLimit, stable.ErrorRate, slo.MaxErrorRateDelta, cand.Count)}
		}
		if mmViolation {
			return Decision{Fail, fmt.Sprintf("shadow mismatch rate %.4f > %.4f (%d/%d) [fail-fast]", cand.MismatchRate, slo.MaxMismatchRate, cand.Mismatches, cand.Count)}
		}
	}
	enough := cand.Count >= st.MinSamples && elapsed >= st.MinDuration
	if !enough {
		if st.MaxDuration > 0 && elapsed >= st.MaxDuration {
			return Decision{Fail, fmt.Sprintf("insufficient data: %d/%d samples after %v (fail closed)", cand.Count, st.MinSamples, elapsed.Round(time.Millisecond))}
		}
		return Decision{Continue, ""}
	}
	if errViolation {
		return Decision{Fail, fmt.Sprintf("error rate %.4f > %.4f (stable %.4f + %.4f)", cand.ErrorRate, errLimit, stable.ErrorRate, slo.MaxErrorRateDelta)}
	}
	if mmViolation {
		return Decision{Fail, fmt.Sprintf("shadow mismatch rate %.4f > %.4f (%d/%d)", cand.MismatchRate, slo.MaxMismatchRate, cand.Mismatches, cand.Count)}
	}
	if stable.Count > 0 {
		limit := stable.P99Ms * slo.MaxP99Factor
		if alt := stable.P99Ms + ms(slo.P99Slack); alt > limit {
			limit = alt
		}
		if cand.P99Ms > limit {
			return Decision{Fail, fmt.Sprintf("p99 %.3fms > %.3fms (stable p99 %.3fms)", cand.P99Ms, limit, stable.P99Ms)}
		}
	}
	return Decision{Pass, fmt.Sprintf("SLO met: %d samples, err %.4f (stable %.4f), p99 %.3fms (stable %.3fms), mismatch %.4f",
		cand.Count, cand.ErrorRate, stable.ErrorRate, cand.P99Ms, stable.P99Ms, cand.MismatchRate)}
}
