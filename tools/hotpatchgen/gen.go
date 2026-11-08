package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Options configure one transformation.
type Options struct {
	Dir string // input package directory
	// All selects every eligible top-level function and method; otherwise
	// only functions whose doc comment contains a //dimp:patchable line.
	All bool
	// RuntimeImport is the import path of the run-time library.
	RuntimeImport string
	// RegistryExpr is the registry expression (relative to the runtime
	// import alias), default "Default".
	RegistryExpr string
	// SymbolPkg overrides the package part of symbols (default: package name).
	SymbolPkg string
}

// Result is the generated package: file name -> contents.
type Result struct {
	Files    map[string][]byte
	Warnings []string
	Funcs    []FuncReport
}

// FuncReport describes one transformed function (also printed with -v).
type FuncReport struct {
	Symbol      string `json:"symbol"`
	Impl        string `json:"impl"`
	Fingerprint string `json:"fingerprint"`
	Signature   string `json:"signature"`
	CtxAware    bool   `json:"ctx_aware"`
}

const (
	marker        = "//dimp:patchable"
	markerDone    = "//dimp:trampoline"
	runtimeAlias  = "dimprt"
	registryFile  = "zz_dimp_registry.go"
	defaultImport = "github.com/nareknaltakyan1/dimp-research/internal/patchable"
)

type srcFile struct {
	name     string
	src      []byte
	ast      *ast.File
	tf       *token.File
	excluded bool // excluded by build constraints for this platform: copied verbatim
	tagged   bool // has a //go:build line: never transformed
}

type fn struct {
	file      *srcFile
	decl      *ast.FuncDecl
	obj       *types.Func
	sig       *types.Signature
	symbol    string
	implName  string
	slotVar   string
	canon, fp string
	ctxIndex  int // index into params of the first context.Context, -1 if none
}

// Generate transforms the package in opts.Dir.
func Generate(opts Options) (*Result, error) {
	if opts.RuntimeImport == "" {
		opts.RuntimeImport = defaultImport
	}
	if opts.RegistryExpr == "" {
		opts.RegistryExpr = "Default"
	}
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	files, err := readPackage(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var astFiles []*ast.File
	for _, f := range files {
		if f.excluded {
			continue
		}
		f.ast, err = parser.ParseFile(fset, filepath.Join(dir, f.name), f.src, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		f.tf = fset.File(f.ast.Pos())
		if f.name == registryFile || hasIdentPrefix(f.ast, "_dimpSlot_") {
			return nil, fmt.Errorf("%s: package already transformed by hotpatchgen", f.name)
		}
		astFiles = append(astFiles, f.ast)
	}
	if len(astFiles) == 0 {
		return nil, fmt.Errorf("%s: no Go files", dir)
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := conf.Check(astFiles[0].Name.Name, fset, astFiles, info)
	if err != nil {
		return nil, fmt.Errorf("type-check %s: %w", dir, err)
	}
	symPkg := opts.SymbolPkg
	if symPkg == "" {
		symPkg = pkg.Name()
	}

	res := &Result{Files: map[string][]byte{}}
	warn := func(pos token.Pos, format string, a ...any) {
		p := fset.Position(pos)
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s:%d: %s", filepath.Base(p.Filename), p.Line, fmt.Sprintf(format, a...)))
	}

	var all []*fn
	byFile := map[*srcFile][]*fn{}
	for _, f := range files {
		if f.tagged {
			// Platform-specific files are never transformed: the registry
			// would reference code that does not exist on other platforms.
			if bytes.Contains(f.src, []byte(marker)) {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: file has build constraints; its functions are not made patchable", f.name))
			}
			continue
		}
		if f.excluded {
			continue
		}
		for _, d := range f.ast.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			marked := hasMarker(fd.Doc)
			if !opts.All && !marked {
				continue
			}
			reason := ineligible(fd, pkg, f)
			if reason != "" {
				if marked || (opts.All && reason != "init" && reason != "main") {
					warn(fd.Pos(), "skipping %s: %s", fd.Name.Name, reason)
				}
				continue
			}
			obj := info.Defs[fd.Name].(*types.Func)
			sig := obj.Type().(*types.Signature)
			x := &fn{file: f, decl: fd, obj: obj, sig: sig, ctxIndex: -1}
			x.symbol, x.implName, x.slotVar = names(symPkg, fd)
			if pkg.Scope().Lookup(x.implName) != nil {
				return nil, fmt.Errorf("%s: cannot generate %s: name already declared", x.symbol, x.implName)
			}
			x.canon, err = canonImplSig(sig)
			if err != nil {
				warn(fd.Pos(), "skipping %s: %v", x.symbol, err)
				continue
			}
			x.fp = fingerprint(x.canon)
			for i := 0; i < sig.Params().Len(); i++ {
				if isContext(sig.Params().At(i).Type()) {
					x.ctxIndex = i
					break
				}
			}
			if x.ctxIndex < 0 {
				warn(fd.Pos(), "%s has no context.Context parameter: per-call (not per-request) consistency", x.symbol)
			}
			all = append(all, x)
			byFile[f] = append(byFile[f], x)
		}
	}

	for _, f := range files {
		out := f.src
		if fns := byFile[f]; len(fns) > 0 {
			out, err = rewriteFile(f, fns)
			if err != nil {
				return nil, err
			}
		}
		hdr := fmt.Sprintf("// Code generated by hotpatchgen from %s. DO NOT EDIT.\n\n", f.name)
		res.Files[f.name] = append([]byte(hdr), out...)
	}
	reg, err := registry(pkg.Name(), opts, all)
	if err != nil {
		return nil, err
	}
	res.Files[registryFile] = reg
	for _, x := range all {
		res.Funcs = append(res.Funcs, FuncReport{x.symbol, x.implName, x.fp, x.canon, x.ctxIndex >= 0})
	}
	return res, nil
}

func readPackage(dir string) ([]*srcFile, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*srcFile
	ctx := build.Default
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		ok, err := ctx.MatchFile(dir, n)
		if err != nil {
			return nil, err
		}
		out = append(out, &srcFile{name: n, src: src, excluded: !ok, tagged: hasBuildLine(src)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func hasBuildLine(src []byte) bool {
	for _, ln := range bytes.Split(src, []byte("\n")) {
		t := bytes.TrimSpace(ln)
		if bytes.HasPrefix(t, []byte("//go:build")) {
			return true
		}
		if bytes.HasPrefix(t, []byte("package ")) {
			return false
		}
	}
	return false
}

func hasMarker(cg *ast.CommentGroup) bool {
	if cg == nil {
		return false
	}
	for _, c := range cg.List {
		if strings.TrimSpace(c.Text) == marker {
			return true
		}
	}
	return false
}

func hasIdentPrefix(f *ast.File, prefix string) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && strings.HasPrefix(id.Name, prefix) {
			found = true
		}
		return !found
	})
	return found
}

// ineligible returns why fd cannot be made patchable, or "".
func ineligible(fd *ast.FuncDecl, pkg *types.Package, f *srcFile) string {
	switch {
	case fd.Body == nil:
		return "no body (assembly or linkname)"
	case fd.Recv == nil && fd.Name.Name == "init":
		return "init"
	case fd.Recv == nil && fd.Name.Name == "main" && pkg.Name() == "main":
		return "main"
	case fd.Name.Name == "_":
		return "blank name"
	case fd.Type.TypeParams != nil && fd.Type.TypeParams.NumFields() > 0:
		return "generic function (type parameters are not supported: a slot needs one concrete func type; see S6 doc)"
	}
	if fd.Recv != nil {
		t := fd.Recv.List[0].Type
		if st, ok := t.(*ast.StarExpr); ok {
			t = st.X
		}
		switch t.(type) {
		case *ast.Ident:
		default:
			return "method of a generic type (type parameters are not supported)"
		}
	}
	if fd.Doc != nil {
		for _, c := range fd.Doc.List {
			if strings.HasPrefix(c.Text, "//go:") {
				return "compiler directive " + strings.Fields(c.Text)[0] + " (semantics would not carry over to the trampoline)"
			}
		}
	}
	return ""
}

// names returns symbol, implementation name and slot variable name.
func names(symPkg string, fd *ast.FuncDecl) (string, string, string) {
	if fd.Recv == nil {
		return symPkg + "." + fd.Name.Name, fd.Name.Name + "_v0", "_dimpSlot_" + fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	ptr := false
	if st, ok := t.(*ast.StarExpr); ok {
		t, ptr = st.X, true
	}
	tn := t.(*ast.Ident).Name
	sym := symPkg + "." + tn + "." + fd.Name.Name
	if ptr {
		sym = symPkg + ".(*" + tn + ")." + fd.Name.Name
	}
	return sym, tn + "_" + fd.Name.Name + "_v0", "_dimpSlot_" + tn + "_" + fd.Name.Name
}

func isContext(t types.Type) bool {
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "context" && n.Obj().Name() == "Context"
}

// ---- rewriting ----

type edit struct {
	start, end int
	text       string
}

func (f *srcFile) off(p token.Pos) int { return f.tf.Offset(p) }
func (f *srcFile) text(n ast.Node) string {
	return string(f.src[f.off(n.Pos()):f.off(n.End())])
}

type param struct {
	name     string // original name ("" if unnamed)
	typ      string // type expression source text (with "..." for variadic)
	variadic bool
}

func expand(f *srcFile, fl *ast.FieldList) []param {
	var out []param
	if fl == nil {
		return nil
	}
	for _, fld := range fl.List {
		_, variadic := fld.Type.(*ast.Ellipsis)
		t := f.text(fld.Type)
		if len(fld.Names) == 0 {
			out = append(out, param{"", t, variadic})
			continue
		}
		for _, n := range fld.Names {
			out = append(out, param{n.Name, t, variadic})
		}
	}
	return out
}

// identsIn collects identifiers used in the parameter, result and receiver
// type expressions: generated local names must not shadow them inside the
// trampoline body, where the func type is spelled out again.
func identsIn(fd *ast.FuncDecl) map[string]bool {
	used := map[string]bool{}
	visit := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, fld := range fl.List {
			ast.Inspect(fld.Type, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					used[id.Name] = true
				}
				return true
			})
		}
	}
	visit(fd.Recv)
	visit(fd.Type.Params)
	visit(fd.Type.Results)
	return used
}

func rewriteFile(f *srcFile, fns []*fn) ([]byte, error) {
	var edits []edit
	for _, x := range fns {
		e, err := rewriteFunc(f, x)
		if err != nil {
			return nil, err
		}
		edits = append(edits, e...)
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var b bytes.Buffer
	last := 0
	for _, e := range edits {
		b.Write(f.src[last:e.start])
		b.WriteString(e.text)
		last = e.end
	}
	b.Write(f.src[last:])
	out, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("%s: generated code does not parse (hotpatchgen bug): %v\n%s", f.name, err, b.String())
	}
	return out, nil
}

func rewriteFunc(f *srcFile, x *fn) ([]edit, error) {
	fd := x.decl
	var edits []edit
	// Mark the doc comment so the output is recognisably transformed.
	if fd.Doc != nil {
		for _, c := range fd.Doc.List {
			if strings.TrimSpace(c.Text) == marker {
				edits = append(edits, edit{f.off(c.Pos()), f.off(c.End()), markerDone})
			}
		}
	}
	params := expand(f, fd.Type.Params)
	results := expand(f, fd.Type.Results)
	used := identsIn(fd)

	// Local names for the trampoline: keep the original names when they are
	// all present, non-blank and do not shadow an identifier the func type
	// mentions; otherwise use p0..pN. Same for results and the receiver.
	keep := func(ps []param) bool {
		for _, p := range ps {
			if p.name == "" || p.name == "_" || used[p.name] {
				return false
			}
		}
		return true
	}
	taken := map[string]bool{}
	for k := range used {
		taken[k] = true
	}
	fresh := func(base string) string {
		n := base
		for i := 1; taken[n]; i++ {
			n = fmt.Sprintf("%s_%d", base, i)
		}
		taken[n] = true
		return n
	}
	pnames := make([]string, len(params))
	keepParams := keep(params)
	for i, p := range params {
		if keepParams {
			pnames[i] = p.name
			taken[p.name] = true
		} else {
			pnames[i] = fresh(fmt.Sprintf("p%d", i))
		}
	}
	keepResults := len(results) > 0 && keep(results)
	if keepResults {
		for _, r := range results {
			if taken[r.name] {
				keepResults = false
			}
		}
	}
	if keepResults {
		for _, r := range results {
			taken[r.name] = true
		}
	}
	var recvName, recvType string
	if fd.Recv != nil {
		rf := fd.Recv.List[0]
		recvType = f.text(rf.Type)
		if len(rf.Names) == 1 && rf.Names[0].Name != "_" && !taken[rf.Names[0].Name] {
			recvName = rf.Names[0].Name
			taken[recvName] = true
		} else {
			recvName = fresh("recv")
		}
	}
	hp := fresh("hp")

	// func type of the implementation
	var ft strings.Builder
	ft.WriteString("func(")
	var ftParams []string
	if fd.Recv != nil {
		ftParams = append(ftParams, recvType)
	}
	for _, p := range params {
		ftParams = append(ftParams, p.typ)
	}
	ft.WriteString(strings.Join(ftParams, ", ") + ")")
	ft.WriteString(resultList(results, false))

	// trampoline
	var tb strings.Builder
	tb.WriteString("func ")
	if fd.Recv != nil {
		tb.WriteString("(" + recvName + " " + recvType + ") ")
	}
	tb.WriteString(fd.Name.Name + "(")
	var ps []string
	for i, p := range params {
		ps = append(ps, pnames[i]+" "+p.typ)
	}
	tb.WriteString(strings.Join(ps, ", ") + ")")
	if keepResults {
		tb.WriteString(resultList(results, true))
	} else {
		tb.WriteString(resultList(results, false))
	}
	tb.WriteString(" {\n")
	var args []string
	if fd.Recv != nil {
		args = append(args, recvName)
	}
	for i, p := range params {
		a := pnames[i]
		if i == x.ctxIndex {
			a = hp + ".Ctx()"
		}
		if p.variadic {
			a += "..."
		}
		args = append(args, a)
	}
	if x.ctxIndex >= 0 {
		fmt.Fprintf(&tb, "\t%s := %s.Enter(%s)\n\tdefer %s.Exit()\n", hp, x.slotVar, pnames[x.ctxIndex], hp)
	} else {
		fmt.Fprintf(&tb, "\t%s := %s.Acquire()\n\tdefer %s.Release()\n", hp, x.slotVar, hp)
	}
	// comma-ok assertion: the slot is unbound (Impl() == nil) only while
	// package variables are being initialised, before the generated init()
	// binds the build-time implementation; then call it directly.
	fv := fresh("impl")
	call := fmt.Sprintf("%s(%s)", fv, strings.Join(args, ", "))
	direct := fmt.Sprintf("%s(%s)", x.implName, strings.Join(args, ", "))
	ret := "\t\t"
	if len(results) > 0 {
		ret = "\t\treturn "
	}
	ok := fresh("ok")
	fmt.Fprintf(&tb, "\tif %s, %s := %s.Impl().(%s); %s {\n%s%s\n", fv, ok, hp, ft.String(), ok, ret, call)
	if len(results) > 0 {
		fmt.Fprintf(&tb, "\t}\n\treturn %s\n}", direct)
	} else {
		fmt.Fprintf(&tb, "\t\treturn\n\t}\n\t%s\n}", direct)
	}

	// implementation: the original declaration, renamed; for methods the
	// receiver becomes the first parameter.
	var ib strings.Builder
	fmt.Fprintf(&ib, "\n\n// %s is the build-time implementation of %s (moved here by hotpatchgen).\n", x.implName, x.symbol)
	if fd.Recv == nil {
		ib.Write(f.src[f.off(fd.Type.Func):f.off(fd.Name.Pos())])
		ib.WriteString(x.implName)
		ib.Write(f.src[f.off(fd.Name.End()):f.off(fd.End())])
	} else {
		rf := fd.Recv.List[0]
		rn := "_"
		if len(rf.Names) == 1 {
			rn = rf.Names[0].Name
		}
		ib.WriteString("func " + x.implName + "(" + rn + " " + recvType)
		named := len(params) > 0 && params[0].name != ""
		if len(params) > 0 && named {
			ib.WriteString(", ")
			ib.Write(f.src[f.off(fd.Type.Params.Opening)+1 : f.off(fd.Type.Params.Closing)])
		} else {
			for _, p := range params {
				ib.WriteString(", _ " + p.typ)
			}
		}
		ib.WriteString(")")
		ib.Write(f.src[f.off(fd.Type.Params.Closing)+1 : f.off(fd.End())])
	}
	edits = append(edits, edit{f.off(fd.Type.Func), f.off(fd.End()), tb.String() + ib.String()})
	return edits, nil
}

func resultList(rs []param, withNames bool) string {
	switch {
	case len(rs) == 0:
		return ""
	case len(rs) == 1 && !withNames:
		return " " + rs[0].typ
	}
	var parts []string
	for _, r := range rs {
		if withNames {
			parts = append(parts, r.name+" "+r.typ)
		} else {
			parts = append(parts, r.typ)
		}
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func registry(pkgName string, opts Options, fns []*fn) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by hotpatchgen. DO NOT EDIT.\n\npackage %s\n\n", pkgName)
	if len(fns) == 0 {
		return format.Source(b.Bytes())
	}
	fmt.Fprintf(&b, "import %s %q\n\n", runtimeAlias, opts.RuntimeImport)
	b.WriteString("// Hotpatch slots: one per patchable function, registered under a stable\n// symbol with the signature fingerprint computed from go/types at build time.\n// The initialisers deliberately do not reference the implementations: a\n// recursive F would otherwise form an initialisation cycle\n// (slot -> F_v0 -> F -> slot).\nvar (\n")
	for _, x := range fns {
		fmt.Fprintf(&b, "\t%s = %s.%s.MustRegister(%s.SlotSpec{Symbol: %q, Fingerprint: %q, Signature: %q})\n",
			x.slotVar, runtimeAlias, opts.RegistryExpr, runtimeAlias, x.symbol, x.fp, x.canon)
	}
	b.WriteString(")\n\n")
	b.WriteString("// init binds the build-time implementations (version 0); MustBind re-derives\n// each fingerprint from reflect and panics if it disagrees with go/types.\nfunc init() {\n")
	for _, x := range fns {
		fmt.Fprintf(&b, "\t%s.MustBind(%s)\n", x.slotVar, x.implName)
	}
	b.WriteString("}\n")
	out, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("registry: %v\n%s", err, b.String())
	}
	return out, nil
}
