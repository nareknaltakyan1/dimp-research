package canary

import (
	"math"
	"sync/atomic"
	"time"
)

// Latency histogram: log-spaced buckets (ratio 1.08, i.e. <= 8% relative
// error on reported quantiles) from 1 us to ~100 s. Recording is a single
// atomic add, so the hot path is lock-free and race-free.
const (
	histMin    = float64(time.Microsecond)
	histRatio  = 1.08
	histBucket = 240
)

var histLogRatio = math.Log(histRatio)

func bucketOf(d time.Duration) int {
	f := float64(d)
	if f <= histMin {
		return 0
	}
	i := int(math.Log(f/histMin)/histLogRatio) + 1
	if i >= histBucket {
		i = histBucket - 1
	}
	return i
}

// bucketUpper returns the upper bound of bucket i.
func bucketUpper(i int) time.Duration {
	return time.Duration(histMin * math.Pow(histRatio, float64(i)))
}

// Histogram is a concurrent latency histogram.
type Histogram struct {
	b   [histBucket]atomic.Int64
	n   atomic.Int64
	sum atomic.Int64
	max atomic.Int64
}

// Record adds one observation.
func (h *Histogram) Record(d time.Duration) {
	h.b[bucketOf(d)].Add(1)
	h.n.Add(1)
	h.sum.Add(int64(d))
	for {
		m := h.max.Load()
		if int64(d) <= m || h.max.CompareAndSwap(m, int64(d)) {
			return
		}
	}
}

// Quantile returns the q-quantile (0..1) as the upper bound of its bucket.
func (h *Histogram) Quantile(q float64) time.Duration {
	n := h.n.Load()
	if n == 0 {
		return 0
	}
	rank := int64(math.Ceil(q * float64(n)))
	if rank < 1 {
		rank = 1
	}
	var c int64
	for i := range h.b {
		c += h.b[i].Load()
		if c >= rank {
			u := bucketUpper(i)
			if m := time.Duration(h.max.Load()); m > 0 && m < u {
				return m
			}
			return u
		}
	}
	return time.Duration(h.max.Load())
}

// Metrics are the per-version (or per-role) counters of one window.
type Metrics struct {
	Count      atomic.Int64 // requests completed
	Errors     atomic.Int64 // requests that returned an error
	Mismatches atomic.Int64 // shadow only: responses that differed from stable
	Dropped    atomic.Int64 // shadow only: mirrors dropped (concurrency limit)
	Latency    Histogram
}

func (m *Metrics) observe(d time.Duration, err error) { m.observeL(d, err, true) }

func (m *Metrics) observeL(d time.Duration, err error, latency bool) {
	if latency {
		m.Latency.Record(d)
	}
	if err != nil {
		m.Errors.Add(1)
	}
	m.Count.Add(1)
}

// Snapshot is a JSON-friendly copy of Metrics.
type Snapshot struct {
	Count        int64   `json:"count"`
	Errors       int64   `json:"errors"`
	ErrorRate    float64 `json:"error_rate"`
	Mismatches   int64   `json:"mismatches,omitempty"`
	MismatchRate float64 `json:"mismatch_rate,omitempty"`
	Dropped      int64   `json:"dropped,omitempty"`
	P50Ms        float64 `json:"p50_ms"`
	P99Ms        float64 `json:"p99_ms"`
	MaxMs        float64 `json:"max_ms"`
	MeanMs       float64 `json:"mean_ms"`
}

func ms(d time.Duration) float64 { return float64(d) / 1e6 }

// Snapshot returns a consistent-enough copy (fields are read independently;
// rates are computed from the loaded values so they are never > 1).
func (m *Metrics) Snapshot() Snapshot {
	s := Snapshot{
		Count:      m.Count.Load(),
		Errors:     m.Errors.Load(),
		Mismatches: m.Mismatches.Load(),
		Dropped:    m.Dropped.Load(),
		P50Ms:      ms(m.Latency.Quantile(0.50)),
		P99Ms:      ms(m.Latency.Quantile(0.99)),
		MaxMs:      ms(time.Duration(m.Latency.max.Load())),
	}
	if s.Errors > s.Count {
		s.Errors = s.Count
	}
	if s.Mismatches > s.Count {
		s.Mismatches = s.Count
	}
	if s.Count > 0 {
		s.ErrorRate = float64(s.Errors) / float64(s.Count)
		s.MismatchRate = float64(s.Mismatches) / float64(s.Count)
		if n := m.Latency.n.Load(); n > 0 {
			s.MeanMs = ms(time.Duration(m.Latency.sum.Load() / n))
		}
	}
	return s
}
