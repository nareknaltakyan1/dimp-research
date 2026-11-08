package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/types"
	"sort"
	"strconv"
	"strings"
)

// Build-time half of the signature canonicaliser. It must produce exactly
// the same string as internal/patchable.CanonType does from reflect; every
// generated registration cross-checks the two at package init.

const fingerprintLen = 16

func fingerprint(canon string) string {
	h := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(h[:])[:fingerprintLen]
}

// canonImplSig renders the implementation type of a function: for methods
// the receiver becomes the first parameter.
func canonImplSig(sig *types.Signature) (string, error) {
	var b strings.Builder
	var params []types.Type
	if r := sig.Recv(); r != nil {
		params = append(params, r.Type())
	}
	for i := 0; i < sig.Params().Len(); i++ {
		params = append(params, sig.Params().At(i).Type())
	}
	b.WriteString("func")
	if err := canonSigParts(&b, params, sig.Variadic(), sig.Results()); err != nil {
		return "", err
	}
	return b.String(), nil
}

func canonSigParts(b *strings.Builder, params []types.Type, variadic bool, results *types.Tuple) error {
	b.WriteString("(")
	for i, p := range params {
		if i > 0 {
			b.WriteString(", ")
		}
		if variadic && i == len(params)-1 {
			s, ok := p.(*types.Slice)
			if !ok {
				return fmt.Errorf("variadic parameter is not a slice: %s", p)
			}
			b.WriteString("...")
			if err := canonType(b, s.Elem()); err != nil {
				return err
			}
			continue
		}
		if err := canonType(b, p); err != nil {
			return err
		}
	}
	b.WriteString(")")
	switch results.Len() {
	case 0:
	case 1:
		b.WriteString(" ")
		return canonType(b, results.At(0).Type())
	default:
		b.WriteString(" (")
		for i := 0; i < results.Len(); i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := canonType(b, results.At(i).Type()); err != nil {
				return err
			}
		}
		b.WriteString(")")
	}
	return nil
}

func canonSig(b *strings.Builder, sig *types.Signature) error {
	var params []types.Type
	for i := 0; i < sig.Params().Len(); i++ {
		params = append(params, sig.Params().At(i).Type())
	}
	return canonSigParts(b, params, sig.Variadic(), sig.Results())
}

func canonType(b *strings.Builder, t types.Type) error {
	t = types.Unalias(t)
	switch t := t.(type) {
	case *types.Basic:
		if t.Kind() == types.UnsafePointer {
			b.WriteString("unsafe.Pointer")
			return nil
		}
		if t.Info()&types.IsUntyped != 0 {
			return fmt.Errorf("untyped %s in signature", t)
		}
		b.WriteString(types.Typ[t.Kind()].Name()) // byte -> uint8, rune -> int32
	case *types.Named:
		obj := t.Obj()
		if obj.Pkg() == nil { // error
			b.WriteString(obj.Name())
			return nil
		}
		if t.TypeArgs().Len() > 0 {
			return fmt.Errorf("instantiated generic type %s not supported", t)
		}
		b.WriteString(obj.Pkg().Name() + "." + obj.Name())
	case *types.Pointer:
		b.WriteString("*")
		return canonType(b, t.Elem())
	case *types.Slice:
		b.WriteString("[]")
		return canonType(b, t.Elem())
	case *types.Array:
		b.WriteString("[" + strconv.FormatInt(t.Len(), 10) + "]")
		return canonType(b, t.Elem())
	case *types.Map:
		b.WriteString("map[")
		if err := canonType(b, t.Key()); err != nil {
			return err
		}
		b.WriteString("]")
		return canonType(b, t.Elem())
	case *types.Chan:
		switch t.Dir() {
		case types.RecvOnly:
			b.WriteString("<-chan ")
		case types.SendOnly:
			b.WriteString("chan<- ")
		default:
			b.WriteString("chan ")
		}
		return canonType(b, t.Elem())
	case *types.Signature:
		b.WriteString("func")
		return canonSig(b, t)
	case *types.Interface:
		if !t.IsMethodSet() {
			return fmt.Errorf("constraint interface %s not supported", t)
		}
		n := t.NumMethods()
		if n == 0 {
			b.WriteString("interface{}")
			return nil
		}
		ms := make([]*types.Func, n)
		for i := range ms {
			ms[i] = t.Method(i)
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i].Name() < ms[j].Name() })
		b.WriteString("interface{")
		for i, m := range ms {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(m.Name())
			if err := canonSig(b, m.Type().(*types.Signature)); err != nil {
				return err
			}
		}
		b.WriteString("}")
	case *types.Struct:
		b.WriteString("struct{")
		for i := 0; i < t.NumFields(); i++ {
			if i > 0 {
				b.WriteString("; ")
			}
			f := t.Field(i)
			b.WriteString(f.Name() + " ")
			if err := canonType(b, f.Type()); err != nil {
				return err
			}
			if f.Embedded() {
				b.WriteString(" embedded")
			}
			if tag := t.Tag(i); tag != "" {
				b.WriteString(" " + strconv.Quote(tag))
			}
		}
		b.WriteString("}")
	case *types.TypeParam:
		return fmt.Errorf("type parameter %s not supported", t)
	default:
		return fmt.Errorf("unsupported type %T %s", t, t)
	}
	return nil
}
