// Package main is a golden-test input for `hotpatchgen fingerprints`.
package main

import (
	"context"

	fixture "github.com/nareknaltakyan1/dimp-research/tools/hotpatchgen/internal/fixture/gen"
)

func sumV2(ctx context.Context, base int, xs ...int) int { return base }

var Patches = map[string]any{
	"fixture.Sum":            sumV2,
	"fixture.(*Counter).Add": func(c *fixture.Counter, ctx context.Context, d int) int { return d },
	"fixture.Counter.Label":  fixture.Counter_Label_v0,
}

func main() {}
