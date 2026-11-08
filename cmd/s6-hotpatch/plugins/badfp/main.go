// Package main is a deliberately incompatible hotpatch: ApplyDiscount was
// "changed" to take and return int instead of int64 (a signature change a
// hotpatch cannot express). Its manifest lists the true fingerprint, so the
// host rejects it BEFORE dlopen; the "liar" manifest built from it claims
// the expected fingerprint, so the host rejects it AFTER dlopen, by the
// run-time fingerprint check, and still applies nothing.
package main

import (
	"context"

	"github.com/nareknaltakyan1/dimp-research/cmd/s6-hotpatch/gen/biz"
)

func applyDiscountBad(ctx context.Context, cents int, coupon string, tr *biz.Trace) int {
	tr.Add("discount@bad")
	return cents / 2
}

var Patches = map[string]any{
	"biz.ApplyDiscount": applyDiscountBad,
}

func main() {}
