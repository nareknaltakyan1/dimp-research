package dimpclass

import (
	"bytes"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// expectations pin the intended outcome of every corpus case, so the golden
// files cannot silently drift to a wrong classification.
var expectations = map[string]struct {
	dominant  string
	mechanism string
	patchSize int
	classes   string
}{
	"c0-config-comments":      {C0, "none", 0, "C0"},
	"c1-handler-bugfix":       {C1, "S6", 1, "C1"},
	"c1-const-change":         {C1, "S6", 2, "C1"},
	"c1-method-body":          {C1, "S6", 1, "C1"},
	"c1-wasm-guest":           {C1, "S3", 1, "C1"},
	"c2-signature-change":     {C2, "S6", 4, "C1,C2"},
	"c2-interface-change":     {C2, "S5", 2, "C1,C2"},
	"c3-cache-entry-field":    {C3, "S5+xform", 1, "C1,C3"},
	"c4-dependency-bump":      {C4, "S5", 0, "C0,C4"},
	"c5-background-goroutine": {C5, "S5", 2, "C5"},
	"c5-main-new-route":       {C5, "S5", 1, "C2,C5"},
	"c5-closure-factory":      {C5, "S5", 1, "C5"},
	"c5-startup-wiring":       {C5, "S5", 1, "C5"},
}

func TestGoldenCorpus(t *testing.T) {
	cases, err := os.ReadDir("testdata/cases")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 8 {
		t.Fatalf("corpus has %d cases, want >= 8", len(cases))
	}
	for _, c := range cases {
		name := c.Name()
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("testdata", "cases", name)
			r, err := Analyze(filepath.Join(dir, "old"), filepath.Join(dir, "new"))
			if err != nil {
				t.Fatal(err)
			}
			want, ok := expectations[name]
			if !ok {
				t.Fatalf("no expectation for case %s", name)
			}
			if r.Dominant != want.dominant || r.Recommendation.Mechanism != want.mechanism ||
				r.PatchSetSize != want.patchSize || strings.Join(r.Classes, ",") != want.classes {
				t.Errorf("got dominant=%s mechanism=%s patch=%d classes=%v; want %+v",
					r.Dominant, r.Recommendation.Mechanism, r.PatchSetSize, r.Classes, want)
			}
			js, err := r.JSON()
			if err != nil {
				t.Fatal(err)
			}
			md := []byte(r.Markdown())
			golden(t, filepath.Join(dir, "expected.json"), js)
			golden(t, filepath.Join(dir, "expected.md"), md)
			if r.StateXform != "" {
				golden(t, filepath.Join(dir, "expected_xform.go.txt"), []byte(r.StateXform))
				typeCheckSource(t, r.StateXform)
			}
		})
	}
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("%s differs from output (run go test -update and review the diff)\n--- got ---\n%s", path, got)
	}
}

// typeCheckSource checks that a generated transformer skeleton compiles.
func typeCheckSource(t *testing.T, src string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "xform.go", src, 0)
	if err != nil {
		t.Fatalf("skeleton does not parse: %v", err)
	}
	conf := types.Config{Importer: importerFunc(importStd)}
	if _, err := conf.Check("statexform", fset, []*ast.File{f}, nil); err != nil {
		t.Fatalf("skeleton does not type-check: %v\n%s", err, src)
	}
}

func TestCanonTokensIgnoresLayoutAndComments(t *testing.T) {
	a := canonTokens([]byte("{\n\tx := f(a,\n\t\tb) // c\n\n\treturn x;\n}"))
	b := canonTokens([]byte("{ x := f(a, b); /* d */ return x }"))
	if a != b {
		t.Fatalf("canonical forms differ:\n%q\n%q", a, b)
	}
	if canonTokens([]byte("{ return 1 }")) == canonTokens([]byte("{ return 2 }")) {
		t.Fatal("different literals must differ")
	}
}

func TestParseGoMod(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "go.mod")
	os.WriteFile(p, []byte(`module example.com/x // comment

go 1.24.0

toolchain go1.25.1

require (
	github.com/a/b v1.2.3
	github.com/c/d v0.1.0 // indirect
)

require github.com/e/f v2.0.0+incompatible

replace github.com/a/b => ../b
`), 0o644)
	gm, err := ParseGoModFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if gm.Module != "example.com/x" || gm.Go != "1.24.0" || gm.Toolchain != "go1.25.1" {
		t.Fatalf("header: %+v", gm)
	}
	if gm.Require["github.com/a/b"] != "v1.2.3" || gm.Require["github.com/e/f"] != "v2.0.0+incompatible" || !gm.Indirect["github.com/c/d"] {
		t.Fatalf("require: %+v", gm.Require)
	}
	if gm.Replace["github.com/a/b"] != "../b" {
		t.Fatalf("replace: %+v", gm.Replace)
	}
}

func TestGuessPkgName(t *testing.T) {
	for in, want := range map[string]string{
		"github.com/go-chi/chi/v5":    "chi",
		"gopkg.in/yaml.v3":            "yaml",
		"github.com/mattn/go-sqlite3": "sqlite3",
		"github.com/gin-gonic/gin":    "gin",
	} {
		if got := guessPkgName(in); got != want {
			t.Errorf("guessPkgName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIdentical checks that a version compared with itself is C0/none.
func TestIdentical(t *testing.T) {
	dir := filepath.Join("testdata", "cases", "c3-cache-entry-field", "new")
	r, err := Analyze(dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Dominant != C0 || r.Recommendation.Mechanism != "none" || len(r.Findings) != 0 {
		t.Fatalf("self-diff not empty: %+v", r)
	}
}
