package main

import (
	"bytes"
	"flag"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nareknaltakyan1/dimp-research/internal/patchable"
	_ "github.com/nareknaltakyan1/dimp-research/tools/hotpatchgen/internal/fixture/gen" // registers fixture slots
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestGoldenFixture: generating internal/fixture/src with -all must reproduce
// the checked-in, compiled and tested internal/fixture/gen exactly.
func TestGoldenFixture(t *testing.T) {
	res, err := Generate(Options{Dir: "internal/fixture/src", All: true})
	if err != nil {
		t.Fatal(err)
	}
	compareDir(t, res, "internal/fixture/gen", "")
	if len(res.Funcs) != 11 {
		t.Errorf("transformed %d functions, want 11", len(res.Funcs))
	}
	// Build-time fingerprints == run-time fingerprints of the compiled slots.
	for _, f := range res.Funcs {
		s, ok := patchable.Default.Lookup(f.Symbol)
		if !ok {
			t.Errorf("%s not registered", f.Symbol)
			continue
		}
		if s.Fingerprint != f.Fingerprint || s.Signature != f.Signature {
			t.Errorf("%s: go/types %s %q vs reflect %s %q", f.Symbol, f.Fingerprint, f.Signature, s.Fingerprint, s.Signature)
		}
	}
}

// TestGoldenEdge: marker mode with ineligible functions and warnings.
func TestGoldenEdge(t *testing.T) {
	res, err := Generate(Options{Dir: "testdata/edge/input"})
	if err != nil {
		t.Fatal(err)
	}
	compareDir(t, res, "testdata/edge/golden", ".golden")
	typeCheck(t, res, "testdata/edge/input")
	got := strings.Join(res.Warnings, "\n") + "\n"
	wp := "testdata/edge/golden/warnings.golden"
	if *update {
		os.WriteFile(wp, []byte(got), 0o644)
	}
	want, _ := os.ReadFile(wp)
	if got != string(want) {
		t.Errorf("warnings differ:\n--- got\n%s--- want\n%s", got, want)
	}
}

func compareDir(t *testing.T, res *Result, dir, suffix string) {
	t.Helper()
	var names []string
	for n := range res.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(dir, n+suffix)
		if *update {
			os.MkdirAll(dir, 0o755)
			if err := os.WriteFile(p, res.Files[n], 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("missing golden %s (run go test -update)", p)
			continue
		}
		if !bytes.Equal(want, res.Files[n]) {
			t.Errorf("%s differs from generated output (run go test ./tools/hotpatchgen -update)", p)
		}
	}
}

func TestInputNeverTransformedTwice(t *testing.T) {
	if _, err := Generate(Options{Dir: "internal/fixture/gen", All: true}); err == nil || !strings.Contains(err.Error(), "already transformed") {
		t.Fatalf("expected refusal, got %v", err)
	}
}

// TestFingerprintsCmd: static fingerprints of a patch plugin's Patches map
// equal the fingerprints of the slots they target.
func TestFingerprintsCmd(t *testing.T) {
	fps, err := Fingerprints("testdata/patchplugin", "Patches")
	if err != nil {
		t.Fatal(err)
	}
	if len(fps) != 3 {
		t.Fatalf("got %d symbols", len(fps))
	}
	for sym, fp := range fps {
		s, ok := patchable.Default.Lookup(sym)
		if !ok {
			t.Fatalf("%s unknown", sym)
		}
		if s.Fingerprint != fp.Fingerprint {
			t.Errorf("%s: plugin %s %q, slot %s %q", sym, fp.Fingerprint, fp.Signature, s.Fingerprint, s.Signature)
		}
	}
}

// typeCheck type-checks generated files (as the current platform's build
// would see them), resolving imports from dir inside the module.
func typeCheck(t *testing.T, res *Result, dir string) {
	t.Helper()
	abs, _ := filepath.Abs(dir)
	fset := token.NewFileSet()
	var files []*ast.File
	for name, src := range res.Files {
		if strings.Contains(name, "_linux") || strings.Contains(name, "_darwin") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(abs, name), src, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := conf.Check("edge", fset, files, nil); err != nil {
		t.Fatalf("generated code does not type-check: %v", err)
	}
}
