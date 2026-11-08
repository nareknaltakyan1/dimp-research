// Package biz is the business layer of the S6 service, written as ordinary
// Go. It is NOT compiled into the service directly: at build time
// tools/hotpatchgen turns every //dimp:patchable function into a trampoline
// plus an implementation slot and writes the result to ../gen/biz, which is
// what cmd/s6-hotpatch and its patch plugins import.
//
// Call graph of one /api request (all through trampolines):
//
//	(*Service).Handle -> Validate -> (*Service).Price -> ApplyDiscount -> Format
//
// Every step records "<step>@<version>" in the request's Trace, so a test
// can prove that one request never mixed versions from two patch sets.
package biz

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Request is one pricing request.
type Request struct {
	SKU    string
	Qty    int
	Coupon string
	WorkMs int // simulated slow work between validation and pricing
}

// Quote is the result.
type Quote struct {
	Count uint64 // service-wide request counter (the contract's state)
	Cents int64
	Text  string
}

// Trace records which implementation of each step served a request.
type Trace struct {
	mu    sync.Mutex
	steps []string
}

// Add appends a step.
func (t *Trace) Add(step string) {
	t.mu.Lock()
	t.steps = append(t.steps, step)
	t.mu.Unlock()
}

// Steps returns a copy of the steps.
func (t *Trace) Steps() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.steps...)
}

// Service holds the in-memory state that must survive upgrades. Because a
// hotpatch replaces code, not data, the state survives by construction.
type Service struct {
	count  atomic.Uint64
	prices map[string]int64
}

// NewService returns a service with a small catalogue.
func NewService() *Service {
	return &Service{prices: map[string]int64{"widget": 1999, "gadget": 4999, "gizmo": 250}}
}

// Count returns the request counter (not patchable: a state accessor).
func (s *Service) Count() uint64 { return s.count.Load() }

// NextCount increments the request counter.
func (s *Service) NextCount() uint64 { return s.count.Add(1) }

// UnitPrice looks up a SKU.
func (s *Service) UnitPrice(sku string) (int64, bool) {
	p, ok := s.prices[sku]
	return p, ok
}

var coupons = map[string]int64{"SAVE10": 10, "SAVE25": 25}

// CouponPercent returns a coupon's discount in percent.
func CouponPercent(code string) (int64, bool) {
	p, ok := coupons[code]
	return p, ok
}

// ErrInvalid is returned for invalid requests.
var ErrInvalid = errors.New("invalid request")

// Handle serves one request.
//
//dimp:patchable
func (s *Service) Handle(ctx context.Context, req Request, tr *Trace) (q Quote, err error) {
	tr.Add("handle@v1")
	q.Count = s.NextCount()
	if err = Validate(ctx, req, tr); err != nil {
		return q, err
	}
	if req.WorkMs > 0 {
		select {
		case <-time.After(time.Duration(req.WorkMs) * time.Millisecond):
		case <-ctx.Done():
			return q, ctx.Err()
		}
	}
	if q.Cents, err = s.Price(ctx, req, tr); err != nil {
		return q, err
	}
	q.Text = Format(ctx, tr, q.Cents, req.SKU, fmt.Sprint(req.Qty))
	return q, nil
}

// Validate checks a request.
//
//dimp:patchable
func Validate(ctx context.Context, req Request, tr *Trace) error {
	tr.Add("validate@v1")
	if req.SKU == "" || req.Qty <= 0 {
		return fmt.Errorf("%w: sku=%q qty=%d", ErrInvalid, req.SKU, req.Qty)
	}
	return nil
}

// Price computes the total in cents.
//
//dimp:patchable
func (s *Service) Price(ctx context.Context, req Request, tr *Trace) (int64, error) {
	tr.Add("price@v1")
	unit, ok := s.UnitPrice(req.SKU)
	if !ok {
		return 0, fmt.Errorf("%w: unknown sku %q", ErrInvalid, req.SKU)
	}
	return ApplyDiscount(ctx, unit*int64(req.Qty), req.Coupon, tr), nil
}

// ApplyDiscount applies a coupon.
//
//dimp:patchable
func ApplyDiscount(ctx context.Context, cents int64, coupon string, tr *Trace) int64 {
	tr.Add("discount@v1")
	if pct, ok := CouponPercent(coupon); ok {
		// BUG (fixed by the v2 hotpatch): pct/100 is integer division, so
		// every coupon below 100% gives no discount at all.
		return cents - cents*(pct/100)
	}
	return cents
}

// Format renders a quote line.
//
//dimp:patchable
func Format(ctx context.Context, tr *Trace, cents int64, parts ...string) string {
	tr.Add("format@v1")
	return fmt.Sprintf("%s = $%d.%02d", strings.Join(parts, " x "), cents/100, cents%100)
}
