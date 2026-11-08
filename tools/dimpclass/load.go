// Package dimpclass is S8: an update classifier and mechanism selector.
//
// Given two versions (old/new) of a Go package or module, it loads and
// type-checks both with go/types, computes a structured semantic diff
// (functions, methods, types, struct layouts, package-level state,
// constants, init/main, goroutine structure, long-running loops, imports,
// go.mod), classifies the update into the classes C0..C5 and recommends the
// cheapest safe zero-downtime mechanism (S6 transactional patch set, S3 Wasm
// module swap, S5 FD-passing handoff with or without a state transformer, or
// a Kubernetes rolling update as fallback). See
// docs/solutions/S8-update-classifier.md.
//
// Only the standard library is used. Standard-library imports are
// type-checked from export data (go/importer "gc"); imports from inside the
// analysed module are type-checked from source; third-party imports are
// replaced by empty fake packages, so their identifiers are opaque. All
// equality decisions are made on canonical source text (comments and layout
// removed, import names replaced by import paths), so the fake packages
// never make two versions look different; go/types is used for name
// resolution (call graph, constant users, state-type reachability).
package dimpclass

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Snapshot is one loaded version (old or new) of the code under analysis.
type Snapshot struct {
	Root       string // directory that was loaded
	ModulePath string
	ModRoot    string // directory containing go.mod ("" if none)
	GoMod      *GoMod
	Packages   map[string]*Package // by import path
	// Other holds a content hash of every file that is not a non-test Go
	// source of a loaded package (tests, configs, docs, Dockerfiles,
	// assets), keyed by slash path relative to Root.
	Other      map[string]string
	TypeErrors int

	fset *token.FileSet
	src  map[string][]byte // file name -> contents

	packageMode bool
}

// Package is one type-checked package of a snapshot.
type Package struct {
	Path    string // import path
	Name    string
	Dir     string // slash path relative to Snapshot.Root ("." for root)
	Wasm    bool   // guest package built for wasip1 / js,wasm (S3)
	Cgo     bool
	Funcs   map[string]*Func
	Types   map[string]*TypeDecl
	Vars    map[string]*VarDecl
	Consts  map[string]*ConstDecl
	Imports map[string]bool
	Embeds  []string // //go:embed patterns, slash paths relative to Snapshot.Root

	initRefs []string // module-local funcs referenced from package-level var initializers

	files []*ast.File
	info  *types.Info
	tpkg  *types.Package
	snap  *Snapshot
}

// stdImporter is shared by all snapshots (standard-library export data is
// identical for both versions and expensive to load).
var (
	stdMu       sync.Mutex
	stdFset     = token.NewFileSet()
	stdImporter = importer.ForCompiler(stdFset, "gc", nil)
	stdCache    = map[string]*types.Package{}
	stdFailed   = map[string]bool{}
)

func importStd(p string) (*types.Package, error) {
	stdMu.Lock()
	defer stdMu.Unlock()
	if pkg, ok := stdCache[p]; ok {
		return pkg, nil
	}
	if stdFailed[p] {
		return nil, fmt.Errorf("std import %q failed", p)
	}
	pkg, err := stdImporter.Import(p)
	if err != nil {
		stdFailed[p] = true
		return nil, err
	}
	stdCache[p] = pkg
	return pkg, nil
}

// isStdPath reports whether an import path looks like a standard-library
// path (no dot in the first element).
func isStdPath(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return !strings.Contains(first, ".") && p != "C"
}

// guessPkgName guesses the package name of a third-party import path.
func guessPkgName(p string) string {
	elems := strings.Split(p, "/")
	last := elems[len(elems)-1]
	if len(elems) > 1 && len(last) >= 2 && last[0] == 'v' && strings.Trim(last[1:], "0123456789") == "" {
		last = elems[len(elems)-2]
	}
	if i := strings.Index(last, ".v"); i > 0 { // gopkg.in/yaml.v3
		last = last[:i]
	}
	last = strings.TrimPrefix(last, "go-")
	last = strings.TrimSuffix(last, "-go")
	last = strings.TrimSuffix(last, ".go")
	r := strings.NewReplacer("-", "", ".", "")
	return r.Replace(last)
}

// buildContexts are tried in order; the first one that yields Go files for a
// directory is used. wasip1 is tried last so S3 guest packages are found.
func buildContexts() []build.Context {
	linux := build.Default
	linux.GOOS, linux.GOARCH = "linux", "amd64"
	linux.CgoEnabled = true
	wasi := build.Default
	wasi.GOOS, wasi.GOARCH = "wasip1", "wasm"
	wasi.CgoEnabled = false
	return []build.Context{linux, wasi}
}

// Load loads and type-checks the package or module rooted at dir.
//
// If dir contains go.mod, every package of the module is loaded (vendor,
// testdata, hidden and nested-module directories are skipped). Otherwise
// only the package in dir is loaded and go.mod is searched upwards to
// determine the module path.
func Load(dir string) (*Snapshot, error) { return LoadAs(dir, "") }

// LoadAs is Load, but in package mode (dir has no go.mod) the package is
// given the import path asPath if it is non-empty. Analyze uses it so that
// two copies of one package in different directories compare as the same
// package.
func LoadAs(dir, asPath string) (*Snapshot, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("dimpclass: %s is not a directory", dir)
	}
	s := &Snapshot{Root: abs, Packages: map[string]*Package{}, Other: map[string]string{}, fset: token.NewFileSet(), src: map[string][]byte{}}

	moduleMode := false
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err == nil {
		moduleMode = true
		s.ModRoot = abs
	} else {
		for d := filepath.Dir(abs); ; d = filepath.Dir(d) {
			if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
				s.ModRoot = d
				break
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	if s.ModRoot != "" {
		gm, err := ParseGoModFile(filepath.Join(s.ModRoot, "go.mod"))
		if err != nil {
			return nil, err
		}
		s.GoMod = gm
		s.ModulePath = gm.Module
	}
	if s.ModulePath == "" {
		s.ModulePath = filepath.Base(abs)
	}
	s.packageMode = !moduleMode
	importPathOf := func(d string) string {
		base := abs
		prefix := s.ModulePath
		if !moduleMode && s.ModRoot != "" {
			base = s.ModRoot
		}
		rel, _ := filepath.Rel(base, d)
		if !moduleMode && asPath != "" && d == abs {
			return asPath
		}
		if rel == "." {
			return prefix
		}
		return path.Join(prefix, filepath.ToSlash(rel))
	}

	// Collect package directories and the "other" files.
	type pdir struct {
		dir   string
		files []string
		ctx   build.Context
	}
	var dirs []pdir
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != abs {
				if name == "vendor" || name == "testdata" || name == "node_modules" ||
					strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir // nested module
				}
				if !moduleMode {
					// package mode: only the root package's directory,
					// but still hash sibling non-Go files of the root.
					return filepath.SkipDir
				}
			}
			var goFiles []string
			var ctxUsed build.Context
			for _, ctx := range buildContexts() {
				goFiles = goFiles[:0]
				ents, _ := os.ReadDir(p)
				for _, e := range ents {
					n := e.Name()
					if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
						continue
					}
					if ok, _ := ctx.MatchFile(p, n); ok {
						goFiles = append(goFiles, filepath.Join(p, n))
					}
				}
				if len(goFiles) > 0 {
					ctxUsed = ctx
					break
				}
			}
			if len(goFiles) > 0 {
				dirs = append(dirs, pdir{p, append([]string(nil), goFiles...), ctxUsed})
			}
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	srcSet := map[string]bool{}
	for _, d := range dirs {
		for _, f := range d.files {
			srcSet[f] = true
		}
	}
	// Hash every other regular file below Root (including excluded dirs
	// such as testdata, but not .git).
	_ = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != abs && (d.Name() == ".git" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if p != abs && !moduleMode {
				return filepath.SkipDir // package mode: root directory only
			}
			return nil
		}
		if !d.Type().IsRegular() || srcSet[p] {
			return nil
		}
		rel, _ := filepath.Rel(abs, p)
		s.Other[filepath.ToSlash(rel)] = hashFile(p)
		return nil
	})

	// Parse all package dirs.
	byPath := map[string]*Package{}
	for _, d := range dirs {
		ip := importPathOf(d.dir)
		rel, _ := filepath.Rel(abs, d.dir)
		pkg := &Package{Path: ip, Dir: filepath.ToSlash(rel), snap: s,
			Funcs: map[string]*Func{}, Types: map[string]*TypeDecl{}, Vars: map[string]*VarDecl{},
			Consts: map[string]*ConstDecl{}, Imports: map[string]bool{}}
		pkg.Wasm = d.ctx.GOOS == "wasip1"
		sort.Strings(d.files)
		names := map[string]int{}
		var files []*ast.File
		for _, f := range d.files {
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			s.src[f] = b
			af, err := parser.ParseFile(s.fset, f, b, parser.ParseComments)
			if af == nil {
				return nil, fmt.Errorf("dimpclass: parse %s: %v", f, err)
			}
			if err != nil {
				s.TypeErrors++
			}
			files = append(files, af)
			names[af.Name.Name]++
		}
		// If a directory has several package clauses (e.g. a stray
		// "package main" generator with an ignore tag that matched), keep
		// the majority package.
		best, bestN := "", -1
		for n, c := range names {
			if c > bestN || (c == bestN && n < best) {
				best, bestN = n, c
			}
		}
		for _, af := range files {
			if af.Name.Name == best {
				pkg.files = append(pkg.files, af)
			}
		}
		pkg.Name = best
		byPath[ip] = pkg
	}

	// Type-check in dependency order via a recursive importer.
	state := map[string]int{} // 0 unvisited, 1 in progress, 2 done
	var check func(p *Package)
	var imp importerFunc
	imp = func(ip string) (*types.Package, error) {
		if ip == "unsafe" {
			return types.Unsafe, nil
		}
		if lp, ok := byPath[ip]; ok {
			if state[ip] == 1 {
				return fakePackage(ip, lp.Name), nil // import cycle (invalid code)
			}
			check(lp)
			return lp.tpkg, nil
		}
		if isStdPath(ip) {
			if pk, err := importStd(ip); err == nil {
				return pk, nil
			}
		}
		return fakePackage(ip, guessPkgName(ip)), nil
	}
	check = func(p *Package) {
		if state[p.Path] != 0 {
			return
		}
		state[p.Path] = 1
		conf := types.Config{
			Importer:    imp,
			FakeImportC: true,
			Error:       func(error) { s.TypeErrors++ },
			Sizes:       types.SizesFor("gc", "amd64"),
		}
		p.info = &types.Info{
			Types: map[ast.Expr]types.TypeAndValue{},
			Defs:  map[*ast.Ident]types.Object{},
			Uses:  map[*ast.Ident]types.Object{},
		}
		tp, _ := conf.Check(p.Path, s.fset, p.files, p.info)
		if tp == nil {
			tp = types.NewPackage(p.Path, p.Name)
		}
		p.tpkg = tp
		state[p.Path] = 2
	}
	paths := make([]string, 0, len(byPath))
	for ip := range byPath {
		paths = append(paths, ip)
	}
	sort.Strings(paths)
	for _, ip := range paths {
		check(byPath[ip])
	}
	for _, ip := range paths {
		p := byPath[ip]
		extract(p)
		s.Packages[ip] = p
	}
	computeStateTypes(s)
	computeStartupOnly(s)
	return s, nil
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

var (
	fakeMu    sync.Mutex
	fakeCache = map[string]*types.Package{}
)

// fakePackage returns an empty, complete package standing in for code we do
// not type-check. A fresh one per (path,name) is cached globally; it has no
// objects, so every selector on it is an (ignored) type error.
func fakePackage(p, name string) *types.Package {
	fakeMu.Lock()
	defer fakeMu.Unlock()
	k := p + "\x00" + name
	if pk, ok := fakeCache[k]; ok {
		return pk
	}
	pk := types.NewPackage(p, name)
	pk.MarkComplete()
	fakeCache[k] = pk
	return pk
}

func hashFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "unreadable"
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// GoMod is the subset of go.mod that matters for update classification.
type GoMod struct {
	Module    string            `json:"module"`
	Go        string            `json:"go,omitempty"`
	Toolchain string            `json:"toolchain,omitempty"`
	Require   map[string]string `json:"require,omitempty"`  // path -> version
	Indirect  map[string]bool   `json:"indirect,omitempty"` // path -> // indirect
	Replace   map[string]string `json:"replace,omitempty"`  // "old[ ver]" -> "new[ ver]"
	Exclude   []string          `json:"exclude,omitempty"`
}

// ParseGoModFile parses a go.mod file with a small line-oriented parser
// (golang.org/x/mod is not allowed).
func ParseGoModFile(p string) (*GoMod, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gm := &GoMod{Require: map[string]string{}, Indirect: map[string]bool{}, Replace: map[string]string{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	block := ""
	for sc.Scan() {
		line := sc.Text()
		comment := ""
		if i := strings.Index(line, "//"); i >= 0 {
			comment = line[i+2:]
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == ")" {
			block = ""
			continue
		}
		fields := strings.Fields(line)
		verb := block
		if block == "" {
			verb = fields[0]
			fields = fields[1:]
			if len(fields) == 1 && fields[0] == "(" {
				block = verb
				continue
			}
		}
		unq := func(s string) string { return strings.Trim(s, "\"`") }
		switch verb {
		case "module":
			if len(fields) > 0 {
				gm.Module = unq(fields[0])
			}
		case "go":
			if len(fields) > 0 {
				gm.Go = fields[0]
			}
		case "toolchain":
			if len(fields) > 0 {
				gm.Toolchain = fields[0]
			}
		case "require":
			if len(fields) >= 2 {
				gm.Require[unq(fields[0])] = fields[1]
				if strings.Contains(comment, "indirect") {
					gm.Indirect[unq(fields[0])] = true
				}
			}
		case "replace":
			if i := indexOf(fields, "=>"); i > 0 {
				gm.Replace[strings.Join(fields[:i], " ")] = strings.Join(fields[i+1:], " ")
			}
		case "exclude":
			gm.Exclude = append(gm.Exclude, strings.Join(fields, " "))
		}
	}
	return gm, sc.Err()
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}
