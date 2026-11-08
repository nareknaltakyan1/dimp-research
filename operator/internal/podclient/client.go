// Package podclient talks to an S7 pod's admin endpoint. It mirrors the
// JSON shapes of cmd/s7-inplace and internal/canary without importing the
// root module (the operator is a separate Go module).
package podclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Stage / SLO / Strategy mirror internal/canary (durations in nanoseconds).
type Stage struct {
	Name        string        `json:"name"`
	Mode        string        `json:"mode"`
	Weight      float64       `json:"weight"`
	MinDuration time.Duration `json:"min_duration"`
	MaxDuration time.Duration `json:"max_duration"`
	MinSamples  int64         `json:"min_samples"`
}

type SLO struct {
	MaxErrorRateDelta float64       `json:"max_error_rate_delta"`
	MaxP99Factor      float64       `json:"max_p99_factor"`
	P99Slack          time.Duration `json:"p99_slack"`
	MaxMismatchRate   float64       `json:"max_mismatch_rate"`
	FailFastSamples   int64         `json:"fail_fast_samples"`
	WarmupSamples     int64         `json:"warmup_samples"`
}

type Strategy struct {
	Stages       []Stage       `json:"stages"`
	SLO          SLO           `json:"slo"`
	Interval     time.Duration `json:"interval"`
	ShadowSample float64       `json:"shadow_sample"`
	DrainTimeout time.Duration `json:"drain_timeout"`
}

// PatchRecord mirrors cmd/s7-inplace.PatchRecord.
type PatchRecord struct {
	Version   string  `json:"version"`
	Digest    string  `json:"digest"`
	Accepted  bool    `json:"accepted"`
	Error     string  `json:"error"`
	Reason    string  `json:"reason"`
	VerifyMs  float64 `json:"verify_ms"`
	CompileMs float64 `json:"compile_ms"`
}

type Snapshot struct {
	Count        int64   `json:"count"`
	Errors       int64   `json:"errors"`
	ErrorRate    float64 `json:"error_rate"`
	Mismatches   int64   `json:"mismatches"`
	MismatchRate float64 `json:"mismatch_rate"`
	P99Ms        float64 `json:"p99_ms"`
}

type StageRecord struct {
	Stage     string   `json:"stage"`
	Mode      string   `json:"mode"`
	Verdict   string   `json:"verdict"`
	Reason    string   `json:"reason"`
	Stable    Snapshot `json:"stable"`
	Candidate Snapshot `json:"candidate"`
}

type Rollout struct {
	Phase      string        `json:"phase"`
	Stable     string        `json:"stable"`
	Candidate  string        `json:"candidate"`
	Stage      string        `json:"stage"`
	Reason     string        `json:"reason"`
	Stages     []StageRecord `json:"stages"`
	DecisionMs float64       `json:"decision_ms"`
	Previous   string        `json:"previous"`
}

// RolloutDoc mirrors GET /admin/rollout.
type RolloutDoc struct {
	Stable        string              `json:"stable"`
	Rollout       *Rollout            `json:"rollout"`
	Window        map[string]Snapshot `json:"window"`
	RunningDigest string              `json:"running_digest"`
	Count         uint64              `json:"count"`
	Patches       []PatchRecord       `json:"patches"`
}

// Client is an admin-endpoint client.
type Client struct {
	HTTP  *http.Client
	Token string
}

// New returns a client with sane timeouts.
func New(token string) *Client {
	return &Client{HTTP: &http.Client{Timeout: 30 * time.Second}, Token: token}
}

func (c *Client) do(ctx context.Context, method, url string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, err
}

// Push uploads a signed envelope. A non-2xx answer is returned as a
// PatchRecord with Reason (e.g. SignatureInvalid) and code.
func (c *Client) Push(ctx context.Context, base string, envelope []byte, s *Strategy, strategyName string, reconcile bool) (int, PatchRecord, error) {
	req := map[string]any{"bundle": json.RawMessage(envelope), "reconcile": reconcile}
	if s != nil {
		req["strategy"] = s
	} else {
		req["strategy_name"] = strategyName
	}
	body, _ := json.Marshal(req)
	code, b, err := c.do(ctx, http.MethodPost, base+"/admin/patch", body)
	var rec PatchRecord
	if err == nil {
		json.Unmarshal(b, &rec)
		if rec.Error == "" && code >= 300 {
			rec.Error = fmt.Sprintf("HTTP %d: %.200s", code, b)
		}
	}
	return code, rec, err
}

// Rollout reads GET /admin/rollout.
func (c *Client) Rollout(ctx context.Context, base string) (*RolloutDoc, error) {
	code, b, err := c.do(ctx, http.MethodGet, base+"/admin/rollout", nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("rollout status: HTTP %d: %.200s", code, b)
	}
	var d RolloutDoc
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Abort aborts the pod's rollout.
func (c *Client) Abort(ctx context.Context, base, reason string) error {
	_, _, err := c.do(ctx, http.MethodPost, base+"/admin/rollout/abort?reason="+reason, nil)
	return err
}

// Rollback reverts the pod to its retained previous version.
func (c *Client) Rollback(ctx context.Context, base, to string) error {
	code, b, err := c.do(ctx, http.MethodPost, base+"/admin/rollback?to="+to, nil)
	if err == nil && code != 200 {
		err = fmt.Errorf("rollback: HTTP %d: %.200s", code, b)
	}
	return err
}
