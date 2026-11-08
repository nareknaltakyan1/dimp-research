package client

import (
	"context"
	"errors"
	"net/http"
	"time"
)

const maxRetries = 3

const backoff = 50 * time.Millisecond

var errExhausted = errors.New("retries exhausted")

// Get fetches url, retrying transient failures.
func Get(ctx context.Context, c *http.Client, url string) (*http.Response, error) {
	for i := 0; i < maxRetries; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := c.Do(req)
		if err == nil && resp.StatusCode < 500 {
			return resp, nil
		}
		time.Sleep(backoff * time.Duration(i+1))
	}
	return nil, errExhausted
}

// Budget reports the worst-case retry delay.
func Budget(ctx context.Context) time.Duration {
	var d time.Duration
	for i := 1; i <= maxRetries; i++ {
		d += backoff * time.Duration(i)
	}
	return d
}

// Describe is unrelated to retries.
func Describe(ctx context.Context) string { return "http client" }
