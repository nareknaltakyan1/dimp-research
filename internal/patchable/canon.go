package patchable

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Signature fingerprints
//
// A patchable function is identified by a stable symbol ("pkg.F",
// "pkg.(*T).M") and a fingerprint of its signature. The fingerprint is the
// first 8 bytes (16 hex digits) of SHA-256 over a *canonical* signature
// string. The same canonical form is produced from two independent type
// representations:
//
//   - at build time from go/types (tools/hotpatchgen, canonTypes), and
//   - at run time from reflect (this file, CanonType).
//
// Every generated slot registration re-derives the fingerprint from reflect
// and panics if it disagrees with the build-time value, so each compiled
// generated package doubles as a cross-check of the two canonicalisers.
//
// Canonical form (close to Go syntax, but not meant to be parsed):
//
//	predeclared      bool int ... string uintptr unsafe.Pointer error
//	named            pkgname.Name            (package *name*, not path; see below)
//	pointer/slice    *T  []T  [N]T  map[K]V
//	channels         chan T  <-chan T  chan<- T
//	func             func(A, B, ...C) R   |  func(A) (R1, R2)
//	interface        interface{}  |  interface{M1(A) R; M2()}   (methods sorted by name)
//	struct           struct{Name T; Emb T embedded; F T "tag"}
//
// Parameter and result names never contribute. Named types are rendered as
// pkgname.Name rather than import path because hotpatchgen type-checks the
// *source* package while the binary contains the *generated* copy at another
// import path; both have the same package name. Name collisions between two
// packages with the same name are caught by the second, exact check that
// Registry.Apply performs: reflect type identity (or convertibility) against
// the slot's build-time function type.

// FingerprintLen is the number of hex digits in a fingerprint.
const FingerprintLen = 16

// Fingerprint hashes a canonical signature string.
func Fingerprint(canon string) string {
	h := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(h[:])[:FingerprintLen]
}

// FingerprintOf returns the canonical signature and fingerprint of a func
// value. It returns an error if fn is not a non-nil func.
func FingerprintOf(fn any) (canon, fp string, err error) {
	if fn == nil {
		return "", "", fmt.Errorf("patchable: nil impl")
	}
	t := reflect.TypeOf(fn)
	if t.Kind() != reflect.Func {
		return "", "", fmt.Errorf("patchable: impl is %s, not a func", t)
	}
	if reflect.ValueOf(fn).IsNil() {
		return "", "", fmt.Errorf("patchable: nil func value")
	}
	c := CanonType(t)
	return c, Fingerprint(c), nil
}

// CanonType renders t in canonical form.
func CanonType(t reflect.Type) string {
	var b strings.Builder
	canonReflect(&b, t)
	return b.String()
}

var reflectBasic = map[reflect.Kind]string{
	reflect.Bool: "bool", reflect.Int: "int", reflect.Int8: "int8", reflect.Int16: "int16",
	reflect.Int32: "int32", reflect.Int64: "int64", reflect.Uint: "uint", reflect.Uint8: "uint8",
	reflect.Uint16: "uint16", reflect.Uint32: "uint32", reflect.Uint64: "uint64",
	reflect.Uintptr: "uintptr", reflect.Float32: "float32", reflect.Float64: "float64",
	reflect.Complex64: "complex64", reflect.Complex128: "complex128", reflect.String: "string",
	reflect.UnsafePointer: "unsafe.Pointer",
}

func canonReflect(b *strings.Builder, t reflect.Type) {
	if t.Name() != "" {
		if t.PkgPath() == "" { // predeclared
			if t.Kind() == reflect.Interface {
				b.WriteString("error")
				return
			}
			b.WriteString(reflectBasic[t.Kind()])
			return
		}
		// reflect's String() for a defined type is "pkgname.Name" (plus
		// type arguments for generic instances, which hotpatchgen skips).
		b.WriteString(t.String())
		return
	}
	switch t.Kind() {
	case reflect.Pointer:
		b.WriteString("*")
		canonReflect(b, t.Elem())
	case reflect.Slice:
		b.WriteString("[]")
		canonReflect(b, t.Elem())
	case reflect.Array:
		b.WriteString("[" + strconv.Itoa(t.Len()) + "]")
		canonReflect(b, t.Elem())
	case reflect.Map:
		b.WriteString("map[")
		canonReflect(b, t.Key())
		b.WriteString("]")
		canonReflect(b, t.Elem())
	case reflect.Chan:
		switch t.ChanDir() {
		case reflect.RecvDir:
			b.WriteString("<-chan ")
		case reflect.SendDir:
			b.WriteString("chan<- ")
		default:
			b.WriteString("chan ")
		}
		canonReflect(b, t.Elem())
	case reflect.Func:
		b.WriteString("func")
		canonReflectSig(b, t)
	case reflect.Interface:
		n := t.NumMethod()
		if n == 0 {
			b.WriteString("interface{}")
			return
		}
		ms := make([]reflect.Method, n)
		for i := range ms {
			ms[i] = t.Method(i)
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
		b.WriteString("interface{")
		for i, m := range ms {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(m.Name)
			canonReflectSig(b, m.Type)
		}
		b.WriteString("}")
	case reflect.Struct:
		b.WriteString("struct{")
		for i := 0; i < t.NumField(); i++ {
			if i > 0 {
				b.WriteString("; ")
			}
			f := t.Field(i)
			b.WriteString(f.Name + " ")
			canonReflect(b, f.Type)
			if f.Anonymous {
				b.WriteString(" embedded")
			}
			if f.Tag != "" {
				b.WriteString(" " + strconv.Quote(string(f.Tag)))
			}
		}
		b.WriteString("}")
	default:
		b.WriteString(reflectBasic[t.Kind()])
	}
}

// canonReflectSig writes "(params) results" for a func type.
func canonReflectSig(b *strings.Builder, t reflect.Type) {
	b.WriteString("(")
	for i := 0; i < t.NumIn(); i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		in := t.In(i)
		if t.IsVariadic() && i == t.NumIn()-1 {
			b.WriteString("...")
			canonReflect(b, in.Elem())
			continue
		}
		canonReflect(b, in)
	}
	b.WriteString(")")
	switch t.NumOut() {
	case 0:
	case 1:
		b.WriteString(" ")
		canonReflect(b, t.Out(0))
	default:
		b.WriteString(" (")
		for i := 0; i < t.NumOut(); i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			canonReflect(b, t.Out(i))
		}
		b.WriteString(")")
	}
}
