// Package main is the v2 hotpatch for s6-hotpatch, built with
// -buildmode=plugin. It replaces three functions of the patchable business
// layer in ONE transaction:
//
//	biz.ApplyDiscount  bug fix: v1 computed cents*(pct/100) == 0 for pct < 100
//	biz.Validate       new rule: qty must not exceed 1000
//	biz.Format         new output format (marks the quote as v2)
//
// biz.(*Service).Handle and biz.(*Service).Price are not patched; they keep
// running their build-time bodies and call the new code through the
// trampolines. The plugin imports the generated package, so it must be built
// by the same toolchain with the same flags as the host (see ../../build.sh).
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/nareknaltakyan1/dimp-research/cmd/s6-hotpatch/gen/biz"
)

func validateV2(ctx context.Context, req biz.Request, tr *biz.Trace) error {
	tr.Add("validate@v2")
	if req.SKU == "" || req.Qty <= 0 || req.Qty > 1000 {
		return fmt.Errorf("%w: sku=%q qty=%d (v2: 1 <= qty <= 1000)", biz.ErrInvalid, req.SKU, req.Qty)
	}
	return nil
}

func applyDiscountV2(ctx context.Context, cents int64, coupon string, tr *biz.Trace) int64 {
	tr.Add("discount@v2")
	if pct, ok := biz.CouponPercent(coupon); ok {
		return cents - cents*pct/100
	}
	return cents
}

func formatV2(ctx context.Context, tr *biz.Trace, cents int64, parts ...string) string {
	tr.Add("format@v2")
	return fmt.Sprintf("[v2] %s => %d.%02d USD", strings.Join(parts, " * "), cents/100, cents%100)
}

// Patches maps stable symbols to replacement implementations (receiver
// first for methods). Its static fingerprints go into the signed manifest:
//
//	go run ./tools/hotpatchgen fingerprints -dir cmd/s6-hotpatch/plugins/v2
var Patches = map[string]any{
	"biz.Validate":      validateV2,
	"biz.ApplyDiscount": applyDiscountV2,
	"biz.Format":        formatV2,
}

func main() {}
