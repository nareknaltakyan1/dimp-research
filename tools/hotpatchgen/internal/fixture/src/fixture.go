// Package fixture is the input of hotpatchgen's compiled golden test: it is
// transformed with -all into ../gen, which is checked in, compiled, and
// exercised by ../gen/fixture_test.go.
package fixture

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unsafe"
)

// Counter is a struct with pointer and value receiver methods.
type Counter struct {
	N    int
	Name string
}

// Add has a pointer receiver.
func (c *Counter) Add(ctx context.Context, d int) int {
	c.N += d
	return c.N
}

// Label has a value receiver and no context.
func (c Counter) Label() string { return fmt.Sprintf("%s=%d", c.Name, c.N) }

// Sum is variadic.
func Sum(ctx context.Context, base int, xs ...int) int {
	for _, x := range xs {
		base += x
	}
	return base
}

// Divide has named results and a naked return.
func Divide(a, b int) (q, r int, err error) {
	if b == 0 {
		err = errors.New("divide by zero")
		return
	}
	q, r = a/b, a%b
	return
}

// Join contains closures, defer and recover; they stay in the moved body.
func Join(ctx context.Context, parts []string) (out string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("recovered: %v", p)
		}
	}()
	up := func(s string) string { return strings.ToUpper(s) }
	for i, p := range parts {
		if p == "panic" {
			panic("boom")
		}
		parts[i] = up(p)
	}
	return strings.Join(parts, "+"), nil
}

// Unnamed parameters and a blank receiver.
func (_ *Counter) Unnamed(context.Context, int, string) bool { return true }

// Exotic exercises the canonicaliser on many type constructors.
func Exotic(m map[string][]*Counter, ch <-chan struct {
	A int `json:"a"`
	Counter
}, f func(...byte) (rune, error), p unsafe.Pointer, i interface {
	Label() string
	fmt.Stringer
}, e any, arr [4]uintptr, sc chan<- complex128) error {
	return nil
}

// NoResult has no results.
func NoResult(ctx context.Context, c *Counter) { c.N = -1 }

// Recursive calls itself through the trampoline (so a patched version is
// picked up by recursive calls too).
func Recursive(ctx context.Context, n int) int {
	if n <= 0 {
		return 0
	}
	return n + Recursive(ctx, n-1)
}

// Shadow uses a parameter name equal to a type it mentions elsewhere,
// forcing the generator to rename trampoline parameters.
type p0 int

func Shadow(Counter Counter, x p0) p0 { return x + p0(Counter.N) }

// Spawn starts a goroutine from the body (used by the stack-scan test).
func Spawn(ctx context.Context, release chan struct{}) {
	go func() { <-release }()
}
