package dimpclass

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/scanner"
	"go/token"
	"go/types"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Func is a top-level function or method.
type Func struct {
	Key            string // "F", "T.M" (receiver base type, pointer-ness is in Sig); "init" merges all init()s
	Name           string
	Recv           string // receiver base type name ("" for functions)
	Sig            string // canonical signature (import paths, no parameter names)
	BodyHash       string
	BodyToks       int
	Generic        bool
	Exported       bool
	CtxParam       bool     // has a context.Context parameter (S6 per-request pinning works)
	GoTargets      []string // sorted canonical targets of go statements ("func(){...}" for literals)
	Long           string   // non-empty: why this function is considered long-running
	OuterHash      string   // body hash with function-literal bodies blanked
	ReturnsClosure bool     // returns a function value / http.Handler built from a function literal
	StartupOnly    bool     // only reachable by direct calls from main()/init() (see computeStartupOnly)
	Nodes          int      // AST node count of the body
	Inlinable      bool     // heuristic: small enough for the gc inliner
	Refs           []string // qualified keys of module-local funcs/methods it references
	CallRefs       []string // subset of Refs that are direct calls f(...) / x.m(...)
	ValRefs        []string // subset of Refs used as values (handlers, callbacks, method values)
	ConstRefs      []string // qualified keys of module-local constants it uses
	TypeRefs       []string // qualified keys of module-local named types it mentions
	File           string
	Line           int
}

// Field is one struct field.
type Field struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // canonical (import paths)
	TypeSrc  string `json:"-"`    // as written (for skeleton generation)
	Tag      string `json:"tag,omitempty"`
	Embedded bool   `json:"embedded,omitempty"`
}

// TypeDecl is a package-level named type.
type TypeDecl struct {
	Name       string
	Kind       string // struct, interface, alias, other
	TypeParams string
	Underlying string // canonical text of the type expression
	Fields     []Field
	Methods    map[string]string // interface methods (name -> canonical sig), embedded as "embed:X"
	State      bool              // reachable from in-memory service state (see computeStateTypes)
	StateWhy   string
	File       string
	Line       int
	imports    map[string]string // file import name -> path (for skeletons)
}

// VarDecl is a package-level variable.
type VarDecl struct {
	Name     string
	Type     string // canonical declared or inferred type ("" if unknown)
	InitHash string // canonical text of the initializer ("" if none)
	Embed    bool
	Line     int
	File     string
}

// ConstDecl is a package-level constant.
type ConstDecl struct {
	Name  string
	Type  string
	Value string
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// canonText returns the token stream of node's source with comments and
// layout removed; tokens are joined by single spaces and every statement
// terminator (explicit or automatic) is rendered as ";".
func (s *Snapshot) canonText(n ast.Node) string {
	if n == nil {
		return ""
	}
	start, end := s.fset.Position(n.Pos()), s.fset.Position(n.End())
	src := s.src[start.Filename]
	if src == nil || start.Offset > end.Offset || end.Offset > len(src) {
		return ""
	}
	return canonTokens(src[start.Offset:end.Offset])
}

func canonTokens(src []byte) string {
	var sc scanner.Scanner
	fs := token.NewFileSet()
	f := fs.AddFile("", -1, len(src))
	sc.Init(f, src, nil, 0)
	var b strings.Builder
	prevSemi := true
	for {
		_, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		var t string
		switch {
		case tok == token.SEMICOLON:
			if prevSemi {
				continue
			}
			t = ";"
		case tok.IsLiteral():
			t = lit
		default:
			t = tok.String()
		}
		// a ";" before "}" or ")" is layout, not structure
		if (tok == token.RBRACE || tok == token.RPAREN) && strings.HasSuffix(b.String(), " ;") {
			str := b.String()
			b.Reset()
			b.WriteString(str[:len(str)-2])
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(t)
		prevSemi = tok == token.SEMICOLON
	}
	return strings.TrimSuffix(b.String(), " ;")
}

// fileImports maps the local import names of f to import paths.
func fileImports(f *ast.File) map[string]string {
	m := map[string]string{}
	for _, is := range f.Imports {
		p, _ := strconv.Unquote(is.Path.Value)
		name := ""
		if is.Name != nil {
			name = is.Name.Name
		} else if isStdPath(p) {
			name = path.Base(p)
		} else {
			name = guessPkgName(p)
		}
		m[name] = p
	}
	return m
}

// exprStr renders a type expression canonically, replacing import names by
// import paths so that renaming an import is not a type change.
func exprStr(e ast.Expr, imps map[string]string) string {
	switch x := e.(type) {
	case nil:
		return ""
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			if p, ok := imps[id.Name]; ok {
				return p + "." + x.Sel.Name
			}
		}
		return exprStr(x.X, imps) + "." + x.Sel.Name
	case *ast.StarExpr:
		return "*" + exprStr(x.X, imps)
	case *ast.ParenExpr:
		return exprStr(x.X, imps)
	case *ast.ArrayType:
		if x.Len == nil {
			return "[]" + exprStr(x.Elt, imps)
		}
		return "[" + types.ExprString(x.Len) + "]" + exprStr(x.Elt, imps)
	case *ast.Ellipsis:
		return "..." + exprStr(x.Elt, imps)
	case *ast.MapType:
		return "map[" + exprStr(x.Key, imps) + "]" + exprStr(x.Value, imps)
	case *ast.ChanType:
		switch x.Dir {
		case ast.SEND:
			return "chan<- " + exprStr(x.Value, imps)
		case ast.RECV:
			return "<-chan " + exprStr(x.Value, imps)
		}
		return "chan " + exprStr(x.Value, imps)
	case *ast.FuncType:
		return "func" + fieldListTypes(x.Params, imps, true) + results(x.Results, imps)
	case *ast.IndexExpr:
		return exprStr(x.X, imps) + "[" + exprStr(x.Index, imps) + "]"
	case *ast.IndexListExpr:
		var parts []string
		for _, i := range x.Indices {
			parts = append(parts, exprStr(i, imps))
		}
		return exprStr(x.X, imps) + "[" + strings.Join(parts, ",") + "]"
	case *ast.StructType:
		var parts []string
		for _, f := range structFields(x, imps) {
			s := f.Name + " " + f.Type
			if f.Embedded {
				s = "embed " + f.Type
			}
			if f.Tag != "" {
				s += " " + strconv.Quote(f.Tag)
			}
			parts = append(parts, s)
		}
		return "struct{" + strings.Join(parts, "; ") + "}"
	case *ast.InterfaceType:
		ms := interfaceMethods(x, imps)
		if len(ms) == 0 {
			return "any" // interface{} and any are identical
		}
		keys := make([]string, 0, len(ms))
		for k := range ms {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, k+ms[k])
		}
		return "interface{" + strings.Join(parts, "; ") + "}"
	case *ast.UnaryExpr: // ~T in constraints
		return x.Op.String() + exprStr(x.X, imps)
	case *ast.BinaryExpr: // A | B in constraints
		return exprStr(x.X, imps) + "|" + exprStr(x.Y, imps)
	}
	return types.ExprString(e)
}

func fieldListTypes(fl *ast.FieldList, imps map[string]string, paren bool) string {
	var parts []string
	if fl != nil {
		for _, f := range fl.List {
			t := exprStr(f.Type, imps)
			n := len(f.Names)
			if n == 0 {
				n = 1
			}
			for i := 0; i < n; i++ {
				parts = append(parts, t)
			}
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func results(fl *ast.FieldList, imps map[string]string) string {
	if fl == nil || len(fl.List) == 0 {
		return ""
	}
	r := fieldListTypes(fl, imps, true)
	if len(fl.List) == 1 && len(fl.List[0].Names) <= 1 {
		return " " + strings.TrimSuffix(strings.TrimPrefix(r, "("), ")")
	}
	return " " + r
}

func structFields(st *ast.StructType, imps map[string]string) []Field {
	var out []Field
	for _, f := range st.Fields.List {
		tag := ""
		if f.Tag != nil {
			tag, _ = strconv.Unquote(f.Tag.Value)
		}
		t := exprStr(f.Type, imps)
		src := types.ExprString(f.Type)
		if len(f.Names) == 0 {
			name := t
			if i := strings.LastIndex(name, "."); i >= 0 {
				name = name[i+1:]
			}
			name = strings.TrimPrefix(name, "*")
			out = append(out, Field{Name: name, Type: t, TypeSrc: src, Tag: tag, Embedded: true})
			continue
		}
		for _, n := range f.Names {
			out = append(out, Field{Name: n.Name, Type: t, TypeSrc: src, Tag: tag})
		}
	}
	return out
}

func interfaceMethods(it *ast.InterfaceType, imps map[string]string) map[string]string {
	m := map[string]string{}
	for _, f := range it.Methods.List {
		if ft, ok := f.Type.(*ast.FuncType); ok && len(f.Names) > 0 {
			for _, n := range f.Names {
				m[n.Name] = fieldListTypes(ft.Params, imps, true) + results(ft.Results, imps)
			}
			continue
		}
		m["embed:"+exprStr(f.Type, imps)] = ""
	}
	return m
}

func typeParamsStr(fl *ast.FieldList, imps map[string]string) string {
	if fl == nil || len(fl.List) == 0 {
		return ""
	}
	var parts []string
	for _, f := range fl.List {
		c := exprStr(f.Type, imps)
		for _, n := range f.Names {
			parts = append(parts, n.Name+" "+c)
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// recvBase returns the base type name of a receiver expression.
func recvBase(e ast.Expr) (string, bool) {
	ptr := false
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			ptr = true
			e = x.X
			continue
		case *ast.ParenExpr:
			e = x.X
			continue
		case *ast.IndexExpr:
			e = x.X
			continue
		case *ast.IndexListExpr:
			e = x.X
			continue
		case *ast.Ident:
			return x.Name, ptr
		}
		return "?", ptr
	}
}

// funcKey returns the qualified key of a module-local *types.Func, or "".
func funcKey(fn *types.Func, local func(string) bool) string {
	if fn.Pkg() == nil || !local(fn.Pkg().Path()) {
		return ""
	}
	sig, _ := fn.Type().(*types.Signature)
	if sig != nil && sig.Recv() != nil {
		t := sig.Recv().Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		if n, ok := types.Unalias(t).(*types.Named); ok {
			return fn.Pkg().Path() + "." + n.Origin().Obj().Name() + "." + fn.Name()
		}
		return "" // interface method
	}
	return fn.Pkg().Path() + "." + fn.Name()
}

var blockingCalls = map[string]bool{
	"ListenAndServe": true, "ListenAndServeTLS": true, "Serve": true, "ServeTLS": true,
	"RunTLS": true, "RunUnix": true, "Accept": true,
}

func extract(p *Package) {
	s := p.snap
	local := func(path string) bool { _, ok := s.pathIsLocal(path); return ok }
	var initBodies []string
	initFunc := (*Func)(nil)
	for _, f := range p.files {
		imps := fileImports(f)
		fname := s.fset.Position(f.Pos()).Filename
		for _, is := range f.Imports {
			ip, _ := strconv.Unquote(is.Path.Value)
			if ip == "C" {
				p.Cgo = true
			}
			p.Imports[ip] = true
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				fn := extractFunc(p, d, imps, local)
				fn.File = relFile(s, fname)
				if fn.Key == "init" && d.Recv == nil {
					initBodies = append(initBodies, s.canonText(d.Body))
					if initFunc == nil {
						initFunc = fn
					} else {
						initFunc.GoTargets = append(initFunc.GoTargets, fn.GoTargets...)
						initFunc.Refs = append(initFunc.Refs, fn.Refs...)
						initFunc.CallRefs = append(initFunc.CallRefs, fn.CallRefs...)
						initFunc.ValRefs = append(initFunc.ValRefs, fn.ValRefs...)
						initFunc.ConstRefs = append(initFunc.ConstRefs, fn.ConstRefs...)
						initFunc.BodyToks += fn.BodyToks
					}
					continue
				}
				if fn.Name == "_" {
					continue
				}
				p.Funcs[fn.Key] = fn
			case *ast.GenDecl:
				extractGen(p, d, imps, fname)
			}
		}
	}
	if initFunc != nil {
		initFunc.BodyHash = shortHash(strings.Join(initBodies, "\n"))
		sort.Strings(initFunc.GoTargets)
		initFunc.Refs = uniq(initFunc.Refs)
		initFunc.CallRefs = uniq(initFunc.CallRefs)
		initFunc.ValRefs = uniq(initFunc.ValRefs)
		initFunc.ConstRefs = uniq(initFunc.ConstRefs)
		p.Funcs["init"] = initFunc
	}
}

func relFile(s *Snapshot, fname string) string {
	if r, err := relPath(s.Root, fname); err == nil {
		return r
	}
	return fname
}

// pathIsLocal reports whether an import path belongs to a loaded package.
func (s *Snapshot) pathIsLocal(p string) (*Package, bool) {
	// Packages map is filled after extraction, so consult the module path.
	if p == s.ModulePath || strings.HasPrefix(p, s.ModulePath+"/") {
		return s.Packages[p], true
	}
	return nil, false
}

func extractFunc(p *Package, d *ast.FuncDecl, imps map[string]string, local func(string) bool) *Func {
	s := p.snap
	fn := &Func{Name: d.Name.Name, Key: d.Name.Name, Exported: d.Name.IsExported(), Line: s.fset.Position(d.Pos()).Line}
	recv := ""
	if d.Recv != nil && len(d.Recv.List) > 0 {
		base, ptr := recvBase(d.Recv.List[0].Type)
		fn.Recv = base
		fn.Key = base + "." + d.Name.Name
		if ptr {
			recv = "(*" + base + ")"
		} else {
			recv = "(" + base + ")"
		}
		if _, ok := d.Recv.List[0].Type.(*ast.IndexExpr); ok {
			fn.Generic = true
		}
		if st, ok := d.Recv.List[0].Type.(*ast.StarExpr); ok {
			switch st.X.(type) {
			case *ast.IndexExpr, *ast.IndexListExpr:
				fn.Generic = true
			}
		}
	}
	if d.Type.TypeParams != nil && len(d.Type.TypeParams.List) > 0 {
		fn.Generic = true
	}
	fn.Sig = recv + "func" + typeParamsStr(d.Type.TypeParams, imps) + fieldListTypes(d.Type.Params, imps, true) + results(d.Type.Results, imps)
	for _, prm := range d.Type.Params.List {
		if exprStr(prm.Type, imps) == "context.Context" {
			fn.CtxParam = true
		}
	}
	if d.Body == nil {
		fn.BodyHash = "nobody"
		return fn
	}
	text := s.canonText(d.Body)
	fn.BodyHash = shortHash(text)
	fn.OuterHash, fn.ReturnsClosure = closureInfo(p, d, imps)
	fn.BodyToks = strings.Count(text, " ") + 1

	refs, consts, typs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	calls, vals := map[string]bool{}, map[string]bool{}
	callee := map[*ast.Ident]bool{}
	inLit := map[*ast.Ident]bool{} // identifiers inside function literals run later, not at call time
	ast.Inspect(d.Body, func(n ast.Node) bool {
		if fl, ok := n.(*ast.FuncLit); ok {
			ast.Inspect(fl.Body, func(m ast.Node) bool {
				if id, ok := m.(*ast.Ident); ok {
					inLit[id] = true
				}
				return true
			})
		}
		if ce, ok := n.(*ast.CallExpr); ok {
			switch f := ast.Unparen(ce.Fun).(type) {
			case *ast.Ident:
				callee[f] = true
			case *ast.SelectorExpr:
				callee[f.Sel] = true
			case *ast.IndexExpr: // generic instantiation f[T](...)
				if id, ok := f.X.(*ast.Ident); ok {
					callee[id] = true
				}
			}
		}
		return true
	})
	nodes := 0
	hasGo, hasDefer, hasSelect, hasClosure, hasRecover := false, false, false, false, false
	ast.Inspect(d.Body, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		nodes++
		switch x := n.(type) {
		case *ast.Ident:
			switch obj := p.info.Uses[x].(type) {
			case *types.Func:
				if k := funcKey(obj, local); k != "" {
					refs[k] = true
					if callee[x] && !inLit[x] {
						calls[k] = true
					} else {
						vals[k] = true
					}
				}
			case *types.Const:
				if obj.Pkg() != nil && local(obj.Pkg().Path()) && obj.Parent() == obj.Pkg().Scope() {
					consts[obj.Pkg().Path()+"."+obj.Name()] = true
				}
			case *types.TypeName:
				if obj.Pkg() != nil && local(obj.Pkg().Path()) && obj.Parent() == obj.Pkg().Scope() {
					typs[obj.Pkg().Path()+"."+obj.Name()] = true
				}
			}
		case *ast.GoStmt:
			hasGo = true
			fn.GoTargets = append(fn.GoTargets, goTarget(s, x))
			if lit, ok := ast.Unparen(x.Call.Fun).(*ast.FuncLit); ok && fn.Long == "" {
				if why := foreverLoop(p, lit.Body); why != "" {
					fn.Long = "spawns a long-running goroutine (" + why + ")"
				}
			}
		case *ast.DeferStmt:
			hasDefer = true
		case *ast.SelectStmt:
			hasSelect = true
			if len(x.Body.List) == 0 && fn.Long == "" {
				fn.Long = "blocks forever on select{}"
			}
		case *ast.FuncLit:
			hasClosure = true
		case *ast.CallExpr:
			name := ""
			switch f := ast.Unparen(x.Fun).(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			if name == "recover" {
				hasRecover = true
			}
			if blockingCalls[name] && fn.Long == "" {
				fn.Long = "calls blocking server loop " + name + "()"
			}
		case *ast.ForStmt, *ast.RangeStmt:
			if fn.Long == "" {
				if why := foreverLoop(p, x); why != "" {
					fn.Long = why
				}
			}
		}
		return true
	})
	fn.Nodes = nodes
	fn.Inlinable = !fn.Generic && nodes <= 60 && !hasGo && !hasDefer && !hasSelect && !hasClosure && !hasRecover && !refs[p.Path+"."+fn.Key]
	fn.Refs = keys(refs)
	fn.CallRefs = keys(calls)
	fn.ValRefs = keys(vals)
	fn.ConstRefs = keys(consts)
	fn.TypeRefs = keys(typs)
	sort.Strings(fn.GoTargets)
	return fn
}

func goTarget(s *Snapshot, g *ast.GoStmt) string {
	switch f := ast.Unparen(g.Call.Fun).(type) {
	case *ast.FuncLit:
		return "func(){...}"
	default:
		return types.ExprString(f)
	}
}

// foreverLoop reports why a loop is considered long-running: a condition-
// less for loop that blocks (select, channel receive, Accept, Sleep, Wait,
// ticker), or a range over a channel.
func foreverLoop(p *Package, n ast.Node) string {
	why := ""
	ast.Inspect(n, func(n ast.Node) bool {
		if why != "" || n == nil {
			return false
		}
		switch x := n.(type) {
		case *ast.FuncLit:
			return true
		case *ast.RangeStmt:
			if tv, ok := p.info.Types[x.X]; ok && tv.Type != nil {
				if _, isChan := tv.Type.Underlying().(*types.Chan); isChan {
					why = "range over channel " + types.ExprString(x.X)
					return false
				}
			}
			if sel, ok := x.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "C" {
				why = "range over ticker channel " + types.ExprString(x.X)
				return false
			}
		case *ast.ForStmt:
			if x.Cond != nil {
				return true
			}
			if b := blocksIn(x.Body); b != "" {
				why = "for {} loop with " + b
				return false
			}
		}
		return true
	})
	return why
}

func blocksIn(body *ast.BlockStmt) string {
	what := ""
	ast.Inspect(body, func(n ast.Node) bool {
		if what != "" {
			return false
		}
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.SelectStmt:
			what = "select"
		case *ast.UnaryExpr:
			if x.Op == token.ARROW {
				what = "channel receive"
			}
		case *ast.CallExpr:
			if sel, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "Accept", "Sleep", "Wait", "ReadMessage", "Recv", "ReadFrom", "Next", "Fetch", "Receive":
					what = sel.Sel.Name + "() call"
				}
			}
		}
		return true
	})
	return what
}

func extractGen(p *Package, d *ast.GenDecl, imps map[string]string, fname string) {
	s := p.snap
	switch d.Tok {
	case token.TYPE:
		for _, sp := range d.Specs {
			ts := sp.(*ast.TypeSpec)
			td := &TypeDecl{Name: ts.Name.Name, TypeParams: typeParamsStr(ts.TypeParams, imps),
				Underlying: exprStr(ts.Type, imps), Line: s.fset.Position(ts.Pos()).Line, File: relFile(s, fname), imports: imps}
			switch t := ts.Type.(type) {
			case *ast.StructType:
				td.Kind = "struct"
				td.Fields = structFields(t, imps)
			case *ast.InterfaceType:
				td.Kind = "interface"
				td.Methods = interfaceMethods(t, imps)
			default:
				td.Kind = "other"
			}
			if ts.Assign.IsValid() {
				td.Kind = "alias"
			}
			p.Types[td.Name] = td
		}
	case token.VAR:
		embed := hasEmbed(d.Doc)
		for _, sp := range d.Specs {
			vs := sp.(*ast.ValueSpec)
			if hasEmbed(vs.Doc) {
				embed = true
			}
			if embed {
				for _, pat := range embedPatterns(d.Doc, vs.Doc) {
					p.Embeds = append(p.Embeds, path.Join(p.Dir, pat))
				}
			}
			for i, n := range vs.Names {
				if n.Name == "_" {
					continue
				}
				v := &VarDecl{Name: n.Name, Embed: embed, Line: s.fset.Position(n.Pos()).Line, File: relFile(s, fname)}
				if vs.Type != nil {
					v.Type = exprStr(vs.Type, imps)
				} else if obj := p.info.Defs[n]; obj != nil && obj.Type() != nil && !hasInvalid(obj.Type()) {
					v.Type = types.TypeString(obj.Type(), func(pk *types.Package) string { return pk.Path() })
				}
				for _, val := range vs.Values {
					ast.Inspect(val, func(n ast.Node) bool {
						if id, ok := n.(*ast.Ident); ok {
							if fo, ok := p.info.Uses[id].(*types.Func); ok {
								if k := funcKey(fo, func(q string) bool { _, ok := s.pathIsLocal(q); return ok }); k != "" {
									p.initRefs = append(p.initRefs, k)
								}
							}
						}
						return true
					})
				}
				if len(vs.Values) == len(vs.Names) {
					v.InitHash = s.canonText(vs.Values[i])
				} else if len(vs.Values) > 0 { // v1, v2 := f()
					v.InitHash = s.canonText(vs.Values[0]) + "#" + strconv.Itoa(i)
				}
				p.Vars[n.Name] = v
			}
		}
	case token.CONST:
		for _, sp := range d.Specs {
			vs := sp.(*ast.ValueSpec)
			for _, n := range vs.Names {
				if n.Name == "_" {
					continue
				}
				c := &ConstDecl{Name: n.Name}
				if obj, ok := p.info.Defs[n].(*types.Const); ok && obj.Val() != nil && obj.Val().Kind() != 0 {
					c.Value = obj.Val().ExactString()
					if !hasInvalid(obj.Type()) {
						c.Type = types.TypeString(obj.Type(), func(pk *types.Package) string { return pk.Path() })
					}
				} else {
					c.Value = "expr:" + s.canonText(vs)
				}
				p.Consts[n.Name] = c
			}
		}
	}
}

func hasEmbed(cg *ast.CommentGroup) bool {
	return len(embedPatterns(cg)) > 0
}

func embedPatterns(cgs ...*ast.CommentGroup) []string {
	var out []string
	for _, cg := range cgs {
		if cg == nil {
			continue
		}
		for _, c := range cg.List {
			if rest, ok := strings.CutPrefix(c.Text, "//go:embed "); ok {
				for _, f := range strings.Fields(rest) {
					out = append(out, strings.Trim(f, "\"`"))
				}
			}
		}
	}
	return out
}

func hasInvalid(t types.Type) bool {
	return strings.Contains(types.TypeString(t, nil), "invalid type")
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func uniq(xs []string) []string {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return keys(m)
}

// computeStateTypes marks named types that hold long-lived in-memory state.
//
// Roots: (a) the types of package-level variables (other than func, error
// and embed vars); (b) struct types with a synchronisation or container
// field (sync.Mutex/RWMutex/Map/Once/Pool, sync/atomic types, maps,
// channels) -- the signature of a long-lived shared object such as a cache,
// session table or server. Everything reachable from a root through fields,
// pointers, slices, arrays, maps and channels is state.
func computeStateTypes(s *Snapshot) {
	type tkey struct{ pkg, name string }
	why := map[tkey]string{}
	var visit func(t types.Type, reason string, depth int)
	seen := map[types.Type]bool{}
	visit = func(t types.Type, reason string, depth int) {
		if t == nil || depth > 20 || seen[t] {
			return
		}
		seen[t] = true
		switch x := types.Unalias(t).(type) {
		case *types.Named:
			obj := x.Origin().Obj()
			if obj.Pkg() != nil {
				if lp, ok := s.Packages[obj.Pkg().Path()]; ok && lp != nil {
					k := tkey{obj.Pkg().Path(), obj.Name()}
					if _, done := why[k]; !done {
						why[k] = reason
					}
				}
			}
			for i := 0; i < x.TypeArgs().Len(); i++ {
				visit(x.TypeArgs().At(i), reason, depth+1)
			}
			visit(x.Underlying(), reason, depth+1)
		case *types.Pointer:
			visit(x.Elem(), reason, depth+1)
		case *types.Slice:
			visit(x.Elem(), reason, depth+1)
		case *types.Array:
			visit(x.Elem(), reason, depth+1)
		case *types.Map:
			visit(x.Key(), reason, depth+1)
			visit(x.Elem(), reason, depth+1)
		case *types.Chan:
			visit(x.Elem(), reason, depth+1)
		case *types.Struct:
			for i := 0; i < x.NumFields(); i++ {
				visit(x.Field(i).Type(), reason, depth+1)
			}
		}
	}
	var pkgs []string
	for ip := range s.Packages {
		pkgs = append(pkgs, ip)
	}
	sort.Strings(pkgs)
	for _, ip := range pkgs {
		p := s.Packages[ip]
		for _, name := range sortedKeys(p.Vars) {
			v := p.Vars[name]
			if v.Embed {
				continue
			}
			obj := p.tpkg.Scope().Lookup(name)
			if obj == nil {
				continue
			}
			t := obj.Type()
			switch t.Underlying().(type) {
			case *types.Signature:
				continue
			}
			if types.TypeString(t, nil) == "error" {
				continue
			}
			clear(seen)
			visit(t, "reachable from package-level var "+name, 0)
		}
		for _, name := range sortedKeys(p.Types) {
			obj, _ := p.tpkg.Scope().Lookup(name).(*types.TypeName)
			if obj == nil {
				continue
			}
			st, ok := obj.Type().Underlying().(*types.Struct)
			if !ok {
				continue
			}
			if f := sharedStateField(st); f != "" {
				clear(seen)
				visit(obj.Type(), "reachable from long-lived type "+name+" (field "+f+")", 0)
			}
		}
	}
	for _, ip := range pkgs {
		p := s.Packages[ip]
		for name, td := range p.Types {
			if w, ok := why[tkey{ip, name}]; ok {
				td.State = true
				td.StateWhy = w
			}
		}
	}
}

// sharedStateField returns the name of the first field that marks a struct
// as a long-lived shared object, or "".
func sharedStateField(st *types.Struct) string {
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		ts := types.TypeString(f.Type(), nil)
		ts = strings.TrimPrefix(ts, "*")
		switch {
		case strings.HasPrefix(ts, "sync.Mutex"), strings.HasPrefix(ts, "sync.RWMutex"),
			strings.HasPrefix(ts, "sync.Map"), strings.HasPrefix(ts, "sync/atomic."),
			strings.HasPrefix(ts, "sync.Pool"):
			return f.Name()
		}
		switch f.Type().Underlying().(type) {
		case *types.Map, *types.Chan:
			return f.Name()
		}
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// computeStartupOnly marks functions that can only run during start-up:
// main(), init(), and (fixpoint) every function whose only references are
// direct calls from start-up-only functions. A function that is ever used as
// a value (handler, callback, method value, var initializer) or has no
// in-module caller (exported API, interface dispatch) is not start-up-only.
// Changing such a function has no effect on a running process: its effect is
// already baked into the live state (routes, wiring, spawned goroutines).
func computeStartupOnly(s *Snapshot) {
	callers := map[string]map[string]bool{}
	valued := map[string]bool{}
	funcs := map[string]*Func{}
	start := map[string]bool{}
	for ip, p := range s.Packages {
		for _, k := range p.initRefs {
			valued[k] = true
		}
		for k, f := range p.Funcs {
			sym := ip + "." + k
			funcs[sym] = f
			if isEntry(p, f) {
				start[sym] = true
			}
			for _, c := range f.CallRefs {
				if callers[c] == nil {
					callers[c] = map[string]bool{}
				}
				callers[c][sym] = true
			}
			for _, v := range f.ValRefs {
				valued[v] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for sym := range funcs {
			if start[sym] || valued[sym] || len(callers[sym]) == 0 {
				continue
			}
			all := true
			for c := range callers[sym] {
				if !start[c] || c == sym {
					all = false
					break
				}
			}
			if all {
				start[sym] = true
				changed = true
			}
		}
	}
	for sym, f := range funcs {
		f.StartupOnly = start[sym]
	}
}

// closureInfo hashes the body with every outermost function-literal body
// replaced by "{}", and reports whether the function is a closure factory:
// it contains a function literal and returns a func-typed value or an
// http.Handler. Changes confined to such literals only affect closures
// created after a patch; closures built at start-up (middleware chains,
// handlers) keep the old code.
func closureInfo(p *Package, d *ast.FuncDecl, imps map[string]string) (string, bool) {
	s := p.snap
	start := s.fset.Position(d.Body.Pos())
	end := s.fset.Position(d.Body.End())
	src := s.src[start.Filename]
	if src == nil || end.Offset > len(src) {
		return "", false
	}
	var lits [][2]int
	hasLit := false
	ast.Inspect(d.Body, func(n ast.Node) bool {
		if fl, ok := n.(*ast.FuncLit); ok {
			hasLit = true
			lits = append(lits, [2]int{s.fset.Position(fl.Body.Pos()).Offset, s.fset.Position(fl.Body.End()).Offset})
			return false
		}
		return true
	})
	var b []byte
	at := start.Offset
	for _, l := range lits {
		b = append(b, src[at:l[0]]...)
		b = append(b, "{}"...)
		at = l[1]
	}
	b = append(b, src[at:end.Offset]...)
	outer := shortHash(canonTokens(b))
	if !hasLit || d.Type.Results == nil {
		return outer, false
	}
	for _, r := range d.Type.Results.List {
		if t := exprStr(r.Type, imps); t == "net/http.Handler" || strings.HasPrefix(t, "func(") {
			return outer, true
		}
		if tv, ok := p.info.Types[r.Type]; ok && tv.Type != nil {
			if _, isSig := tv.Type.Underlying().(*types.Signature); isSig {
				return outer, true
			}
		}
	}
	return outer, false
}
