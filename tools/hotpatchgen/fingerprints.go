package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
)

// SymbolFP is one manifest entry.
type SymbolFP struct {
	Fingerprint string `json:"fingerprint"`
	Signature   string `json:"signature"`
}

// Fingerprints type-checks a patch package and returns symbol -> fingerprint
// for every entry of the package-level map variable varName, whose
// initialiser must be a composite literal with constant string keys.
func Fingerprints(dir, varName string) (map[string]SymbolFP, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	files, err := readPackage(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var asts []*ast.File
	for _, f := range files {
		if f.excluded {
			continue
		}
		a, err := parser.ParseFile(fset, filepath.Join(dir, f.name), f.src, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		asts = append(asts, a)
	}
	if len(asts) == 0 {
		return nil, fmt.Errorf("%s: no Go files", dir)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := conf.Check(asts[0].Name.Name, fset, asts, info); err != nil {
		return nil, fmt.Errorf("type-check %s: %w", dir, err)
	}
	var lit *ast.CompositeLit
	for _, a := range asts {
		for _, d := range a.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if n.Name == varName && i < len(vs.Values) {
						if cl, ok := vs.Values[i].(*ast.CompositeLit); ok {
							lit = cl
						}
					}
				}
			}
		}
	}
	if lit == nil {
		return nil, fmt.Errorf("%s: no `var %s = map[string]any{...}` composite literal", dir, varName)
	}
	out := map[string]SymbolFP{}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			return nil, fmt.Errorf("%s: element is not key: value", fset.Position(el.Pos()))
		}
		ktv := info.Types[kv.Key]
		if ktv.Value == nil || ktv.Value.Kind() != constant.String {
			return nil, fmt.Errorf("%s: key must be a constant string", fset.Position(kv.Key.Pos()))
		}
		sym := constant.StringVal(ktv.Value)
		sig, ok := types.Unalias(info.Types[kv.Value].Type).(*types.Signature)
		if !ok {
			return nil, fmt.Errorf("%s: %s: value is not a func", fset.Position(kv.Value.Pos()), sym)
		}
		if sig.TypeParams().Len() > 0 {
			return nil, fmt.Errorf("%s: generic func", sym)
		}
		var b strings.Builder
		b.WriteString("func")
		if err := canonSig(&b, sig); err != nil {
			return nil, fmt.Errorf("%s: %v", sym, err)
		}
		out[sym] = SymbolFP{fingerprint(b.String()), b.String()}
	}
	return out, nil
}
