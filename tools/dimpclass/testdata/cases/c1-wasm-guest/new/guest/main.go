//go:build wasip1

// Package main is a WASI reactor guest holding the pricing rule.
package main

func main() {}

//go:wasmexport discount
func discount(cents uint32) uint32 {
	if cents > 10000 {
		return cents * 95 / 100
	}
	return cents
}
