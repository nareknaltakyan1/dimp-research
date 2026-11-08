// Command hotpatchgen makes Go functions patchable at build time: the
// source-level analogue of GCC/Clang -fpatchable-function-entry and MSVC
// /hotpatch, without ever writing to code pages at run time.
//
// Usage:
//
//	hotpatchgen [gen] -dir ./biz -out ./gen/biz [-all] [-v]
//	    Transform the package in -dir into -out (the input is not modified).
//	    Selected functions are those whose doc comment contains a
//	    //dimp:patchable line, or every eligible function with -all.
//	    For each selected F:
//	      - F_v0 (or T_M_v0 for methods, receiver first) holds the original body,
//	      - F becomes a trampoline that enters F's slot (per-version refcount,
//	        request-pinned patch set if F has a context.Context parameter)
//	        and calls the current implementation,
//	      - zz_dimp_registry.go registers the slot under the stable symbol
//	        "pkg.F" / "pkg.(*T).M" / "pkg.T.M" with a signature fingerprint.
//
//	hotpatchgen fingerprints -dir ./plugins/v2 [-var Patches]
//	    Type-check a patch plugin and print, as JSON, the fingerprint of every
//	    value in its `var Patches = map[string]any{"sym": fn, ...}`, for the
//	    signed manifest (checked before the plugin is ever opened).
//
//	hotpatchgen check -dir ./biz -out ./gen/biz [-all]
//	    Exit 1 if -out is not what gen would produce (CI staleness check).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

func main() {
	args := os.Args[1:]
	cmd := "gen"
	if len(args) > 0 && (args[0] == "gen" || args[0] == "fingerprints" || args[0] == "check") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "gen", "check":
		err = runGen(cmd, args)
	case "fingerprints":
		err = runFingerprints(args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hotpatchgen:", err)
		os.Exit(1)
	}
}

func runGen(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dir := fs.String("dir", ".", "input package directory")
	out := fs.String("out", "", "output directory (required, must differ from -dir)")
	all := fs.Bool("all", false, "make every eligible top-level function and method patchable")
	rt := fs.String("runtime", defaultImport, "import path of the run-time library")
	sym := fs.String("sympkg", "", "package part of symbols (default: package name)")
	verbose := fs.Bool("v", false, "print the transformed functions")
	fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	ad, _ := filepath.Abs(*dir)
	ao, _ := filepath.Abs(*out)
	if ad == ao {
		return fmt.Errorf("-out must differ from -dir (the input is never modified in place)")
	}
	res, err := Generate(Options{Dir: *dir, All: *all, RuntimeImport: *rt, SymbolPkg: *sym})
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "hotpatchgen: warning:", w)
	}
	if cmd == "check" {
		return checkDir(ao, res)
	}
	if err := os.MkdirAll(ao, 0o755); err != nil {
		return err
	}
	// Remove stale .go files from a previous run (only non-test files we own).
	old, _ := filepath.Glob(filepath.Join(ao, "*.go"))
	for _, p := range old {
		if _, keep := res.Files[filepath.Base(p)]; !keep && !isTest(p) {
			os.Remove(p)
		}
	}
	for name, b := range res.Files {
		if err := os.WriteFile(filepath.Join(ao, name), b, 0o644); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "hotpatchgen: %s -> %s: %d patchable functions, %d warnings\n", *dir, *out, len(res.Funcs), len(res.Warnings))
	if *verbose {
		for _, f := range res.Funcs {
			fmt.Fprintf(os.Stderr, "  %-40s %s ctx=%v %s\n", f.Symbol, f.Fingerprint, f.CtxAware, f.Signature)
		}
	}
	return nil
}

func isTest(p string) bool { return len(p) > 8 && p[len(p)-8:] == "_test.go" }

func checkDir(out string, res *Result) error {
	var stale []string
	for name, want := range res.Files {
		got, err := os.ReadFile(filepath.Join(out, name))
		if err != nil || !bytes.Equal(got, want) {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		return fmt.Errorf("%s is stale: %v (re-run hotpatchgen gen)", out, stale)
	}
	return nil
}

func runFingerprints(args []string) error {
	fs := flag.NewFlagSet("fingerprints", flag.ExitOnError)
	dir := fs.String("dir", ".", "plugin package directory")
	v := fs.String("var", "Patches", "name of the map[string]any variable")
	fs.Parse(args)
	syms, err := Fingerprints(*dir, *v)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(syms)
}
