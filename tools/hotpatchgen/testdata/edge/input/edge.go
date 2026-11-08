// Package edge is a golden-test input for marker mode (no -all): only
// //dimp:patchable functions are transformed; ineligible marked functions
// are reported as warnings and left untouched.
package edge

import (
	"context"
	"fmt"
)

// Store is a toy type.
type Store struct{ m map[string]int }

// Get is patchable.
//
//dimp:patchable
func (s *Store) Get(ctx context.Context, k string) (v int, ok bool) {
	v, ok = s.m[k]
	return
}

// Put is NOT marked and stays a plain method.
func (s *Store) Put(k string, v int) { s.m[k] = v }

// Map is generic: skipped with a warning.
//
//dimp:patchable
func Map[T, U any](xs []T, f func(T) U) []U {
	out := make([]U, 0, len(xs))
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}

// Box is a generic type; its methods are skipped.
type Box[T any] struct{ V T }

// Get on a generic receiver.
//
//dimp:patchable
func (b *Box[T]) Get() T { return b.V }

// Hot carries a compiler directive: skipped with a warning.
//
//dimp:patchable
//go:noinline
func Hot(x int) int { return x * 2 }

// Format has no context parameter (warning: per-call consistency only) and
// a closure.
//
//dimp:patchable
func Format(prefix string, xs ...any) string {
	f := func(x any) string { return fmt.Sprint(x) }
	s := prefix
	for _, x := range xs {
		s += f(x)
	}
	return s
}

// Pair has grouped named parameters and an unnamed func-typed result.
//
//dimp:patchable
func Pair(ctx context.Context, a, b string) func() (string, string) {
	return func() (string, string) { return a, b }
}
