package dimpclass

import (
	"fmt"
	"go/types"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// Update classes.
const (
	C0 = "C0" // no runtime code change (config, docs, tests, comments, formatting)
	C1 = "C1" // body-only changes in patchable functions (incl. constant values)
	C2 = "C2" // signature / added / removed functions, non-state type or global changes
	C3 = "C3" // state-schema change of in-memory state (needs a state transformer)
	C4 = "C4" // dependency / toolchain / runtime / native / embedded-asset change
	C5 = "C5" // long-running code, goroutine structure, main/init (quiescence risk)
)

// ClassDescriptions documents the taxonomy.
var ClassDescriptions = map[string]string{
	C0: "no runtime code change (config, docs, tests, comments, formatting)",
	C1: "body-only change in patchable functions (constant changes patch their users)",
	C2: "signature/added/removed functions or non-state type/global changes (call-graph closure)",
	C3: "state-schema change of in-memory state (state transformer required)",
	C4: "dependency, toolchain, native code or embedded-asset change (new binary)",
	C5: "long-running loop, goroutine-structure, main() or init() change (quiescence risk)",
}

// severity orders classes for the dominant class. C3 ranks highest because
// it needs both a new process and a hand-checked state transformer; C4 and
// C5 need a new process; C0-C2 can be applied in-process.
var severity = map[string]int{C0: 0, C1: 1, C2: 2, C4: 3, C5: 4, C3: 5}

// Finding is one element of the semantic diff.
type Finding struct {
	Class     string `json:"class"`
	Kind      string `json:"kind"`
	Symbol    string `json:"symbol,omitempty"`
	Detail    string `json:"detail,omitempty"`
	S6Blocker string `json:"s6_blocker,omitempty"` // why this change cannot be an S6 patch
}

// PatchFunc is one member of the proposed patch set.
type PatchFunc struct {
	Symbol    string `json:"symbol"`
	Reason    string `json:"reason"`
	File      string `json:"file,omitempty"`
	CtxParam  bool   `json:"ctx_param"`
	Inlinable bool   `json:"inlinable,omitempty"`
}

// Recommendation is the selected mechanism.
type Recommendation struct {
	Mechanism     string   `json:"mechanism"` // none | S6 | S3 | S5 | S5+xform
	InProcess     bool     `json:"in_process"`
	Summary       string   `json:"summary"`
	Justification []string `json:"justification"`
	Alternatives  []string `json:"alternatives,omitempty"`
	Risks         []string `json:"risks,omitempty"`
}

// Report is the full classifier output.
type Report struct {
	Tool           string         `json:"tool"`
	Old            string         `json:"old"`
	New            string         `json:"new"`
	Module         string         `json:"module"`
	Classes        []string       `json:"classes"`
	Dominant       string         `json:"dominant_class"`
	Recommendation Recommendation `json:"recommendation"`
	PatchSet       []PatchFunc    `json:"patch_set"`
	PatchSetSize   int            `json:"patch_set_size"`
	InlineCallers  []string       `json:"inline_sensitive_callers,omitempty"`
	Findings       []Finding      `json:"findings"`
	StateXform     string         `json:"state_transformer,omitempty"`
	Stats          Stats          `json:"stats"`
}

// Stats are size counters for the two versions.
type Stats struct {
	OldPackages    int `json:"old_packages"`
	NewPackages    int `json:"new_packages"`
	OldFuncs       int `json:"old_funcs"`
	NewFuncs       int `json:"new_funcs"`
	OldTypeErrors  int `json:"old_type_errors"`
	NewTypeErrors  int `json:"new_type_errors"`
	ChangedFuncs   int `json:"changed_funcs"`
	ChangedTypes   int `json:"changed_types"`
	ChangedGlobals int `json:"changed_globals"`
}

// Analyze loads two directories and classifies the update.
func Analyze(oldDir, newDir string) (*Report, error) {
	o, err := Load(oldDir)
	if err != nil {
		return nil, fmt.Errorf("old: %w", err)
	}
	as := ""
	if o.packageMode && len(o.Packages) == 1 {
		for ip := range o.Packages {
			as = ip
		}
	}
	n, err := LoadAs(newDir, as)
	if err != nil {
		return nil, fmt.Errorf("new: %w", err)
	}
	r := Classify(o, n)
	r.Old, r.New = oldDir, newDir
	return r, nil
}

type classifier struct {
	o, n     *Snapshot
	r        *Report
	patch    map[string]*PatchFunc
	inline   map[string]bool
	risks    []string
	xform    []xformItem
	changedT map[string]bool
}

func (c *classifier) add(f Finding) { c.r.Findings = append(c.r.Findings, f) }

func (c *classifier) risk(format string, args ...any) {
	c.risks = append(c.risks, fmt.Sprintf(format, args...))
}

// Classify compares two loaded snapshots.
func Classify(o, n *Snapshot) *Report {
	c := &classifier{o: o, n: n, r: &Report{Tool: "dimpclass/1", Module: n.ModulePath},
		patch: map[string]*PatchFunc{}, inline: map[string]bool{}, changedT: map[string]bool{}}
	c.r.Findings = []Finding{}
	c.r.Stats = Stats{OldPackages: len(o.Packages), NewPackages: len(n.Packages),
		OldTypeErrors: o.TypeErrors, NewTypeErrors: n.TypeErrors}
	for _, p := range o.Packages {
		c.r.Stats.OldFuncs += len(p.Funcs)
	}
	for _, p := range n.Packages {
		c.r.Stats.NewFuncs += len(p.Funcs)
	}

	c.diffGoMod()
	c.diffOtherFiles()
	c.diffPackages()
	c.finish()
	return c.r
}

// ---- go.mod ---------------------------------------------------------------

func (c *classifier) diffGoMod() {
	og, ng := c.o.GoMod, c.n.GoMod
	if og == nil && ng == nil {
		return
	}
	if og == nil || ng == nil {
		c.add(Finding{Class: C4, Kind: "gomod", Detail: "go.mod added or removed"})
		return
	}
	if og.Module != ng.Module {
		c.add(Finding{Class: C4, Kind: "gomod-module", Detail: og.Module + " -> " + ng.Module})
	}
	if og.Go != ng.Go {
		c.add(Finding{Class: C4, Kind: "gomod-go", Detail: "go directive " + og.Go + " -> " + ng.Go,
			S6Blocker: "language/toolchain version changed"})
	}
	if og.Toolchain != ng.Toolchain {
		c.add(Finding{Class: C4, Kind: "gomod-toolchain", Detail: "toolchain " + nz(og.Toolchain) + " -> " + nz(ng.Toolchain),
			S6Blocker: "toolchain changed (patch plugins must be built with the host's exact toolchain)"})
	}
	for _, p := range sortedKeys(unionKeys(og.Require, ng.Require)) {
		ov, oOK := og.Require[p]
		nv, nOK := ng.Require[p]
		ind := ""
		if ng.Indirect[p] || (!nOK && og.Indirect[p]) {
			ind = " (indirect)"
		}
		switch {
		case !oOK:
			c.add(Finding{Class: C4, Kind: "dep-added", Symbol: p, Detail: nv + ind, S6Blocker: "dependency graph changed"})
		case !nOK:
			c.add(Finding{Class: C4, Kind: "dep-removed", Symbol: p, Detail: ov + ind, S6Blocker: "dependency graph changed"})
		case ov != nv:
			c.add(Finding{Class: C4, Kind: "dep-version", Symbol: p, Detail: ov + " -> " + nv + ind, S6Blocker: "dependency version changed"})
		}
	}
	for _, k := range sortedKeys(unionKeys(og.Replace, ng.Replace)) {
		if og.Replace[k] != ng.Replace[k] {
			c.add(Finding{Class: C4, Kind: "dep-replace", Symbol: k, Detail: nz(og.Replace[k]) + " -> " + nz(ng.Replace[k]), S6Blocker: "dependency graph changed"})
		}
	}
}

func nz(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func unionKeys[V any](a, b map[string]V) map[string]bool {
	m := map[string]bool{}
	for k := range a {
		m[k] = true
	}
	for k := range b {
		m[k] = true
	}
	return m
}

// ---- non-Go files -------------------------------------------------------------

func (c *classifier) diffOtherFiles() {
	var embeds []string
	for _, p := range c.n.Packages {
		embeds = append(embeds, p.Embeds...)
	}
	for _, p := range c.o.Packages {
		embeds = append(embeds, p.Embeds...)
	}
	for _, f := range sortedKeys(unionKeys(c.o.Other, c.n.Other)) {
		oh, oOK := c.o.Other[f]
		nh, nOK := c.n.Other[f]
		if oOK && nOK && oh == nh {
			continue
		}
		verb := "modified"
		if !oOK {
			verb = "added"
		} else if !nOK {
			verb = "removed"
		}
		base := path.Base(f)
		ext := strings.ToLower(path.Ext(f))
		fd := Finding{Symbol: f, Detail: verb}
		switch {
		case base == "go.mod":
			continue // handled by diffGoMod (root) ; nested modules are out of scope
		case base == "go.sum" || base == "go.work.sum":
			fd.Class, fd.Kind = C0, "file-gosum"
			fd.Detail += " (checksums only; the build list is determined by go.mod)"
		case matchesEmbed(f, embeds):
			fd.Class, fd.Kind = C4, "embed-asset"
			fd.Detail += " (compiled into the binary via //go:embed)"
			fd.S6Blocker = "embedded asset is data in the binary, not a function"
		case strings.HasSuffix(f, "_test.go") || strings.Contains("/"+f, "/testdata/"):
			fd.Class, fd.Kind = C0, "file-test"
		case strings.HasPrefix(strings.ToLower(base), "dockerfile") || strings.HasSuffix(strings.ToLower(base), ".dockerfile") || base == "Containerfile":
			fd.Class, fd.Kind = C4, "file-container"
			fd.S6Blocker = "container image / base runtime changed"
		case ext == ".c" || ext == ".h" || ext == ".s" || ext == ".cc" || ext == ".cpp" || ext == ".syso" || ext == ".m":
			fd.Class, fd.Kind = C4, "file-native"
			fd.S6Blocker = "native/assembly code changed"
		case ext == ".go":
			// Go file that is not part of the analysed build (other GOOS,
			// ignore tag, generator, nested module, excluded dir).
			fd.Class, fd.Kind = C0, "file-go-unbuilt"
			fd.Detail += " (not in the linux/amd64 or wasip1 build of an analysed package)"
		case ext == ".yaml" || ext == ".yml" || ext == ".json" || ext == ".toml" || ext == ".ini" ||
			ext == ".env" || ext == ".conf" || ext == ".cfg" || ext == ".properties" || ext == ".hcl" ||
			strings.HasSuffix(base, ".env.example"):
			fd.Class, fd.Kind = C0, "file-config"
		case ext == ".md" || ext == ".txt" || ext == ".rst" || ext == ".adoc" || strings.HasPrefix(base, "LICENSE") ||
			base == "AUTHORS" || base == "CODEOWNERS" || strings.HasPrefix(f, ".github/"):
			fd.Class, fd.Kind = C0, "file-docs"
		default:
			fd.Class, fd.Kind = C0, "file-asset"
			fd.Detail += " (not compiled in; read at run time if at all)"
		}
		c.add(fd)
	}
}

func matchesEmbed(f string, pats []string) bool {
	for _, p := range pats {
		p = strings.TrimPrefix(p, "./")
		if ok, _ := path.Match(p, f); ok {
			return true
		}
		if p == f || strings.HasPrefix(f, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

// ---- Go packages ----------------------------------------------------------------

func (c *classifier) diffPackages() {
	all := sortedKeys(unionKeys(c.o.Packages, c.n.Packages))
	// Imports newly linked into the program.
	oldImps := map[string]bool{}
	for _, p := range c.o.Packages {
		for i := range p.Imports {
			oldImps[i] = true
		}
	}
	linked := linkedClosure(c.o)
	newImps := map[string]bool{}
	for _, ip := range sortedKeys(c.n.Packages) {
		for _, i := range sortedKeys(c.n.Packages[ip].Imports) {
			if oldImps[i] || newImps[i] {
				continue
			}
			if _, local := c.n.Packages[i]; local {
				continue
			}
			newImps[i] = true
			if i == "C" {
				c.add(Finding{Class: C4, Kind: "cgo", Symbol: ip, Detail: "import \"C\" added", S6Blocker: "cgo code added"})
				continue
			}
			if linked[i] {
				c.add(Finding{Class: C1, Kind: "import-added", Symbol: i, Detail: "first direct import in " + ip + " (already linked into the old binary transitively)"})
				continue
			}
			c.add(Finding{Class: C2, Kind: "import-added", Symbol: i, Detail: "first import in " + ip + " (newly linked package; its init() runs when the patch is loaded)"})
		}
	}

	for _, ip := range all {
		op, nOK := c.o.Packages[ip], c.n.Packages[ip] != nil
		np := c.n.Packages[ip]
		switch {
		case op == nil:
			cls, blk := C2, ""
			if np.Name == "main" {
				cls, blk = C5, "new program entry point"
			}
			c.add(Finding{Class: cls, Kind: "package-added", Symbol: ip, Detail: fmt.Sprintf("%d funcs, %d types", len(np.Funcs), len(np.Types)), S6Blocker: blk})
			for _, k := range sortedKeys(np.Funcs) {
				f := np.Funcs[k]
				if f.Long != "" || len(f.GoTargets) > 0 {
					c.add(Finding{Class: C5, Kind: "goroutine-structure", Symbol: ip + "." + k, Detail: "new package spawns goroutines / long-running code: " + nz(f.Long),
						S6Blocker: "new long-running goroutine"})
				}
				c.addPatch(np, f, "in added package")
			}
		case !nOK:
			c.add(Finding{Class: C2, Kind: "package-removed", Symbol: ip})
		default:
			c.diffPackage(op, np)
		}
	}
}

func (c *classifier) addPatch(p *Package, f *Func, reason string) {
	sym := p.Path + "." + f.Key
	if pf, ok := c.patch[sym]; ok {
		if !strings.Contains(pf.Reason, reason) {
			pf.Reason += "; " + reason
		}
		return
	}
	c.patch[sym] = &PatchFunc{Symbol: sym, Reason: reason, File: f.File, CtxParam: f.CtxParam, Inlinable: f.Inlinable}
}

func isEntry(p *Package, f *Func) bool {
	return (p.Name == "main" && f.Key == "main") || f.Key == "init"
}

func (c *classifier) diffPackage(op, np *Package) {
	ip := np.Path
	if op.Wasm != np.Wasm {
		c.add(Finding{Class: C4, Kind: "build-target", Symbol: ip, Detail: "package moved between native and wasm builds", S6Blocker: "build target changed"})
	}
	// ---- functions
	for _, k := range sortedKeys(unionKeys(op.Funcs, np.Funcs)) {
		of, nf := op.Funcs[k], np.Funcs[k]
		sym := ip + "." + k
		switch {
		case of == nil:
			c.r.Stats.ChangedFuncs++
			switch {
			case isEntry(np, nf):
				c.add(Finding{Class: C5, Kind: k + "-changed", Symbol: sym, Detail: "added", S6Blocker: k + "() is never re-executed in a running process"})
			case nf.Long != "":
				c.add(Finding{Class: C5, Kind: "long-running-added", Symbol: sym, Detail: nf.Long, S6Blocker: "new long-running code"})
				c.addPatch(np, nf, "added")
			case len(nf.GoTargets) > 0:
				c.add(Finding{Class: C5, Kind: "goroutine-structure", Symbol: sym, Detail: "added function spawns goroutines: " + strings.Join(nf.GoTargets, ", "), S6Blocker: "goroutine structure changed"})
				c.addPatch(np, nf, "added")
			default:
				c.add(Finding{Class: C2, Kind: "func-added", Symbol: sym, Detail: nf.Sig})
				c.addPatch(np, nf, "added")
			}
		case nf == nil:
			c.r.Stats.ChangedFuncs++
			if isEntry(op, of) {
				c.add(Finding{Class: C5, Kind: k + "-changed", Symbol: sym, Detail: "removed", S6Blocker: k + "() is never re-executed"})
				continue
			}
			c.add(Finding{Class: C2, Kind: "func-removed", Symbol: sym, Detail: of.Sig})
			c.callerClosure(op, np, sym, "calls removed "+k)
		default:
			sigChanged := of.Sig != nf.Sig
			bodyChanged := of.BodyHash != nf.BodyHash
			if !sigChanged && !bodyChanged {
				continue
			}
			c.r.Stats.ChangedFuncs++
			if sigChanged {
				fd := Finding{Class: C2, Kind: "func-signature", Symbol: sym, Detail: of.Sig + " -> " + nf.Sig}
				if nf.Generic || of.Generic {
					fd.S6Blocker = "generic function (hotpatchgen skips generic instances)"
				}
				c.add(fd)
				c.callerClosure(op, np, sym, "calls "+k+" whose signature changed")
			}
			goChanged := strings.Join(of.GoTargets, "|") != strings.Join(nf.GoTargets, "|")
			switch {
			case isEntry(np, nf):
				c.add(Finding{Class: C5, Kind: k + "-changed", Symbol: sym, Detail: fmt.Sprintf("body changed (%d -> %d tokens)", of.BodyToks, nf.BodyToks),
					S6Blocker: k + "() runs once at start-up (never re-executed / never returns)"})
				continue
			case of.StartupOnly:
				c.add(Finding{Class: C5, Kind: "startup-only-changed", Symbol: sym, Detail: fmt.Sprintf("only called (directly) from start-up code main()/init(); body changed (%d -> %d tokens)", of.BodyToks, nf.BodyToks),
					S6Blocker: "start-up-only code already ran; its effect is baked into the live state, so patching it changes nothing"})
			case !sigChanged && of.ReturnsClosure && nf.ReturnsClosure && of.OuterHash == nf.OuterHash && !goChanged:
				c.add(Finding{Class: C5, Kind: "closure-factory-changed", Symbol: sym, Detail: "change is confined to the function literal(s) this factory returns",
					S6Blocker: "closures created before the patch (e.g. middleware chains built at start-up) keep the old code; only newly created closures see the fix"})
			case goChanged:
				c.add(Finding{Class: C5, Kind: "goroutine-structure", Symbol: sym,
					Detail:    fmt.Sprintf("go statements [%s] -> [%s]", strings.Join(of.GoTargets, ", "), strings.Join(nf.GoTargets, ", ")),
					S6Blocker: "goroutine structure changed (spawned goroutines outlive the patch transaction)"})
			case of.Long != "" || nf.Long != "":
				c.add(Finding{Class: C5, Kind: "long-running-changed", Symbol: sym, Detail: firstNonEmpty(of.Long, nf.Long),
					S6Blocker: "long-running function may never quiesce; running activations keep old code"})
			case !sigChanged:
				fd := Finding{Class: C1, Kind: "func-body", Symbol: sym, Detail: fmt.Sprintf("body changed (%d -> %d tokens)", of.BodyToks, nf.BodyToks)}
				if nf.Generic {
					fd.S6Blocker = "generic function (hotpatchgen skips generic instances)"
				}
				c.add(fd)
			}
			reason := "body changed"
			if sigChanged {
				reason = "signature changed"
			}
			c.addPatch(np, nf, reason)
			if of.Inlinable {
				c.inlineCallers(op, sym)
			}
		}
	}

	// ---- constants
	for _, k := range sortedKeys(unionKeys(op.Consts, np.Consts)) {
		oc, nc := op.Consts[k], np.Consts[k]
		sym := ip + "." + k
		switch {
		case oc == nil:
			c.add(Finding{Class: C1, Kind: "const-added", Symbol: sym, Detail: "= " + nc.Value + " (inlined into the (changed) code that uses it)"})
		case nc == nil:
			c.add(Finding{Class: C1, Kind: "const-removed", Symbol: sym})
		case oc.Value != nc.Value || oc.Type != nc.Type:
			c.r.Stats.ChangedGlobals++
			c.add(Finding{Class: C1, Kind: "const-value", Symbol: sym, Detail: oc.Value + " -> " + nc.Value + " (compile-time constant: every user must be patched)"})
			c.constUsers(sym, k)
		}
	}

	// ---- types
	for _, k := range sortedKeys(unionKeys(op.Types, np.Types)) {
		ot, nt := op.Types[k], np.Types[k]
		sym := ip + "." + k
		switch {
		case ot == nil:
			c.add(Finding{Class: C2, Kind: "type-added", Symbol: sym, Detail: nt.Kind})
		case nt == nil:
			c.add(Finding{Class: C2, Kind: "type-removed", Symbol: sym, Detail: ot.Kind})
		default:
			c.diffType(ip, ot, nt)
		}
	}

	// ---- variables
	for _, k := range sortedKeys(unionKeys(op.Vars, np.Vars)) {
		ov, nv := op.Vars[k], np.Vars[k]
		sym := ip + "." + k
		switch {
		case ov == nil:
			c.r.Stats.ChangedGlobals++
			c.add(Finding{Class: C2, Kind: "var-added", Symbol: sym, Detail: nz(nv.Type)})
			c.risk("new package-level variable %s: under S6 it lives in the patch plugin and is re-initialised by every later patch generation", sym)
		case nv == nil:
			c.r.Stats.ChangedGlobals++
			c.add(Finding{Class: C2, Kind: "var-removed", Symbol: sym, Detail: nz(ov.Type) + " (its in-memory value is dropped)"})
		case ov.Type != "" && nv.Type != "" && ov.Type != nv.Type && (strings.HasPrefix(ov.Type, "func(") || strings.HasPrefix(nv.Type, "func(")):
			c.r.Stats.ChangedGlobals++
			c.add(Finding{Class: C2, Kind: "var-func-type", Symbol: sym, Detail: ov.Type + " -> " + nv.Type + " (function-valued global: code hook, not data state)",
				S6Blocker: "type of a package-level variable changed"})
		case ov.Type != "" && nv.Type != "" && ov.Type != nv.Type:
			c.r.Stats.ChangedGlobals++
			c.add(Finding{Class: C3, Kind: "var-type", Symbol: sym, Detail: ov.Type + " -> " + nv.Type, S6Blocker: "type of in-memory global changed"})
			c.xform = append(c.xform, xformItem{kind: "var", pkg: np, name: k, oldType: ov.Type, newType: nv.Type})
		case ov.InitHash != nv.InitHash:
			c.r.Stats.ChangedGlobals++
			c.add(Finding{Class: C2, Kind: "var-init", Symbol: sym, Detail: "initializer changed",
				S6Blocker: "package-level initializer ran once at start-up; S6 patches functions, not data"})
		}
	}
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// callerClosure adds every old-version referrer of sym to the patch set (they
// must be redirected to the new function), using the new version's body.
func (c *classifier) callerClosure(op, np *Package, sym, reason string) {
	for _, p := range c.o.Packages {
		for _, k := range sortedKeys(p.Funcs) {
			f := p.Funcs[k]
			if !contains(f.Refs, sym) {
				continue
			}
			caller := p.Path + "." + k
			npk := c.n.Packages[p.Path]
			if npk == nil || npk.Funcs[k] == nil {
				continue // caller removed as well
			}
			nf := npk.Funcs[k]
			if isEntry(npk, nf) || nf.Long != "" {
				c.add(Finding{Class: C5, Kind: "long-running-caller", Symbol: caller, Detail: reason + "; caller is " + firstNonEmpty(nf.Long, k+"()"),
					S6Blocker: "caller in the call-graph closure never returns"})
			}
			c.addPatch(npk, nf, reason)
		}
	}
	if np != nil && op != nil && op.Name != "main" {
		name := sym[strings.LastIndex(sym, ".")+1:]
		if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' {
			c.risk("%s is exported: callers outside the analysed module are not in the closure", sym)
		}
	}
}

func (c *classifier) inlineCallers(op *Package, sym string) {
	for _, p := range c.o.Packages {
		for _, k := range sortedKeys(p.Funcs) {
			if contains(p.Funcs[k].Refs, sym) && p.Path+"."+k != sym {
				c.inline[p.Path+"."+k] = true
			}
		}
	}
}

// constUsers adds the users of a changed constant to the patch set.
func (c *classifier) constUsers(sym, name string) {
	tokRe := " " + name + " "
	for _, ip := range sortedKeys(c.n.Packages) {
		p := c.n.Packages[ip]
		for _, k := range sortedKeys(p.Funcs) {
			f := p.Funcs[k]
			if !contains(f.ConstRefs, sym) {
				continue
			}
			if isEntry(p, f) || f.Long != "" {
				c.add(Finding{Class: C5, Kind: "long-running-const-user", Symbol: ip + "." + k,
					Detail:    "uses changed constant " + sym + "; " + firstNonEmpty(f.Long, k+"() never re-executes"),
					S6Blocker: "constant is compiled into code that never returns"})
			}
			c.addPatch(p, f, "uses changed constant "+name)
		}
		if ip != symPkgOf(sym) {
			continue
		}
		for _, vk := range sortedKeys(p.Vars) {
			if strings.Contains(" "+p.Vars[vk].InitHash+" ", tokRe) {
				c.add(Finding{Class: C2, Kind: "var-init", Symbol: ip + "." + vk, Detail: "initializer uses changed constant " + name,
					S6Blocker: "package-level initializer ran once at start-up"})
			}
		}
		for _, tk := range sortedKeys(p.Types) {
			if strings.Contains(p.Types[tk].Underlying, "["+name+"]") {
				t := p.Types[tk]
				cls := C2
				if t.State {
					cls = C3
				}
				c.add(Finding{Class: cls, Kind: "type-const-dependency", Symbol: ip + "." + tk, Detail: "array length uses changed constant " + name,
					S6Blocker: "type layout depends on the constant"})
			}
		}
	}
}

func contains(xs []string, x string) bool {
	i := sort.SearchStrings(xs, x)
	return i < len(xs) && xs[i] == x
}

// diffType compares two declarations of the same named type.
func (c *classifier) diffType(ip string, ot, nt *TypeDecl) {
	sym := ip + "." + nt.Name
	state := ot.State || nt.State
	stateWhy := firstNonEmpty(nt.StateWhy, ot.StateWhy)
	if ot.Kind == nt.Kind && ot.TypeParams == nt.TypeParams && ot.Underlying == nt.Underlying {
		return
	}
	c.r.Stats.ChangedTypes++
	c.changedT[sym] = true
	users := c.typeUsers(sym)
	nonState := func(kind, detail string) {
		c.add(Finding{Class: C2, Kind: kind, Symbol: sym, Detail: detail,
			S6Blocker: fmt.Sprintf("layout/identity of compiled-in type %s changed (%d functions use it); S6 replaces functions, not types", nt.Name, users)})
	}
	if ot.TypeParams != nt.TypeParams {
		nonState("type-params-changed", ot.TypeParams+" -> "+nt.TypeParams)
		return
	}
	if ot.Kind == "interface" && nt.Kind == "interface" {
		var parts []string
		for _, m := range sortedKeys(unionKeys(ot.Methods, nt.Methods)) {
			om, oOK := ot.Methods[m]
			nm, nOK := nt.Methods[m]
			switch {
			case !oOK:
				parts = append(parts, "+"+m+nm)
			case !nOK:
				parts = append(parts, "-"+m+om)
			case om != nm:
				parts = append(parts, "~"+m+": "+om+" -> "+nm)
			}
		}
		c.add(Finding{Class: C2, Kind: "interface-changed", Symbol: sym, Detail: strings.Join(parts, "; "),
			S6Blocker: "interface method set changed (itabs and type assertions are compiled in)"})
		return
	}
	if ot.Kind == "struct" && nt.Kind == "struct" {
		c.diffStruct(sym, ot, nt, state, stateWhy, users)
		return
	}
	if state {
		c.add(Finding{Class: C3, Kind: "state-type-changed", Symbol: sym, Detail: ot.Underlying + " -> " + nt.Underlying + "; state: " + stateWhy,
			S6Blocker: "representation of in-memory state changed"})
		c.xform = append(c.xform, xformItem{kind: "type", pkg: c.n.Packages[ip], name: nt.Name, oldT: ot, newT: nt})
		return
	}
	nonState("type-changed", ot.Underlying+" -> "+nt.Underlying)
}

func (c *classifier) typeUsers(sym string) int {
	n := 0
	for _, p := range c.n.Packages {
		for _, f := range p.Funcs {
			if contains(f.TypeRefs, sym) {
				n++
			}
		}
	}
	return n
}

func (c *classifier) diffStruct(sym string, ot, nt *TypeDecl, state bool, stateWhy string, users int) {
	of := map[string]Field{}
	for _, f := range ot.Fields {
		of[f.Name] = f
	}
	nf := map[string]Field{}
	for _, f := range nt.Fields {
		nf[f.Name] = f
	}
	var added, removed, retyped, tagged []string
	jsonKeyChanged := false
	for _, f := range nt.Fields {
		o, ok := of[f.Name]
		switch {
		case !ok:
			added = append(added, f.Name+" "+f.Type)
		case o.Type != f.Type || o.Embedded != f.Embedded:
			retyped = append(retyped, f.Name+": "+o.Type+" -> "+f.Type)
		case o.Tag != f.Tag:
			tagged = append(tagged, f.Name+": `"+o.Tag+"` -> `"+f.Tag+"`")
			if reflect.StructTag(o.Tag).Get("json") != reflect.StructTag(f.Tag).Get("json") {
				jsonKeyChanged = true
			}
		}
	}
	for _, f := range ot.Fields {
		if _, ok := nf[f.Name]; !ok {
			removed = append(removed, f.Name+" "+f.Type)
		}
	}
	// reorder among common fields
	var oo, no []string
	for _, f := range ot.Fields {
		if _, ok := nf[f.Name]; ok {
			oo = append(oo, f.Name)
		}
	}
	for _, f := range nt.Fields {
		if _, ok := of[f.Name]; ok {
			no = append(no, f.Name)
		}
	}
	reordered := strings.Join(oo, ",") != strings.Join(no, ",")

	emit := func(kind string, items []string) {
		if len(items) == 0 {
			return
		}
		detail := strings.Join(items, "; ")
		if state {
			c.add(Finding{Class: C3, Kind: kind, Symbol: sym, Detail: detail + " [state: " + stateWhy + "]",
				S6Blocker: "layout of in-memory state type changed"})
		} else {
			c.add(Finding{Class: C2, Kind: kind, Symbol: sym, Detail: detail + " [not state]",
				S6Blocker: fmt.Sprintf("layout of compiled-in type %s changed (%d functions use it); S6 replaces functions, not types", ot.Name, users)})
		}
	}
	emit("struct-field-added", added)
	emit("struct-field-removed", removed)
	emit("struct-field-retyped", retyped)
	if reordered {
		emit("struct-field-reordered", []string{strings.Join(oo, ",") + " -> " + strings.Join(no, ",")})
	}
	if len(tagged) > 0 {
		if state && jsonKeyChanged {
			c.add(Finding{Class: C3, Kind: "struct-tag-changed", Symbol: sym, Detail: strings.Join(tagged, "; ") + " (renames the field in the S5 JSON state envelope)",
				S6Blocker: "tags are part of compiled-in type identity"})
		} else {
			c.add(Finding{Class: C2, Kind: "struct-tag-changed", Symbol: sym, Detail: strings.Join(tagged, "; "),
				S6Blocker: "struct tags are part of compiled-in type identity"})
		}
	}
	if state && (len(added)+len(removed)+len(retyped) > 0 || reordered) {
		c.xform = append(c.xform, xformItem{kind: "type", pkg: c.n.Packages[symPkgOf(sym)], name: nt.Name, oldT: ot, newT: nt})
	}
}

func symPkgOf(sym string) string {
	i := strings.LastIndex(sym, ".")
	return sym[:i]
}

// ---- mechanism selection ----------------------------------------------------------

func (c *classifier) finish() {
	r := c.r
	cls := map[string]bool{}
	var blockers []string
	for _, f := range r.Findings {
		cls[f.Class] = true
		if f.S6Blocker != "" && f.Class != C0 {
			blockers = append(blockers, f.Symbol+": "+f.S6Blocker)
		}
	}
	if len(cls) == 0 {
		cls[C0] = true
	}
	r.Classes = sortedKeys(cls)
	r.Dominant = C0
	for k := range cls {
		if severity[k] > severity[r.Dominant] {
			r.Dominant = k
		}
	}
	for _, k := range sortedKeys(c.patch) {
		r.PatchSet = append(r.PatchSet, *c.patch[k])
	}
	if r.PatchSet == nil {
		r.PatchSet = []PatchFunc{}
	}
	r.PatchSetSize = len(r.PatchSet)
	for _, k := range sortedKeys(c.inline) {
		if _, inSet := c.patch[k]; !inSet {
			r.InlineCallers = append(r.InlineCallers, k)
		}
	}

	// Risk notes.
	var noCtx, inMain []string
	// wasmOnly: every code finding is inside a wasip1 guest package, so the
	// whole guest module can be re-instantiated (S3), re-running its
	// initializers; the host and its state are untouched.
	wasmOnly := false
	for _, f := range r.Findings {
		if f.Class == C0 {
			continue
		}
		owner := c.ownerPkg(f.Symbol)
		if owner == nil || !owner.Wasm {
			wasmOnly = false
			break
		}
		wasmOnly = true
	}
	for _, pf := range r.PatchSet {
		if !pf.CtxParam {
			noCtx = append(noCtx, pf.Symbol)
		}
		owner := c.ownerPkg(pf.Symbol)
		if owner != nil && owner.Name == "main" {
			inMain = append(inMain, pf.Symbol)
		}
	}
	if len(r.InlineCallers) > 0 {
		c.risk("changed functions are small enough to be inlined; with S6 slot trampolines the call still dispatches through the slot, but raw binary patching (internal/livepatch) must also patch the inlining callers: %s", strings.Join(r.InlineCallers, ", "))
	}
	if len(noCtx) > 0 && len(noCtx) < r.PatchSetSize {
		c.risk("%d of %d patched functions have no context.Context parameter: they get per-call, not per-request, version consistency under S6", len(noCtx), r.PatchSetSize)
	} else if len(noCtx) > 0 && r.PatchSetSize > 1 {
		c.risk("no patched function has a context.Context parameter: S6 gives per-call (not per-request) consistency, so one request may mix old and new versions of the %d functions", r.PatchSetSize)
	}
	if len(inMain) > 0 && !wasmOnly {
		c.risk("%d patched functions are in package main; a Go plugin cannot import package main, so hotpatchgen must relocate them into an importable package", len(inMain))
	}
	if c.o.TypeErrors+c.n.TypeErrors > 0 {
		c.risk("type information is partial (%d/%d type errors old/new; third-party packages are opaque): call-graph and state reachability are lower bounds", c.o.TypeErrors, c.n.TypeErrors)
	}

	rec := &r.Recommendation
	has := func(k string) bool { return cls[k] }
	inProcOnly := !has(C3) && !has(C4) && !has(C5)
	switch {
	case !has(C1) && !has(C2) && !has(C3) && !has(C4) && !has(C5):
		rec.Mechanism, rec.InProcess = "none", true
		rec.Summary = "No runtime code change: nothing to patch."
		rec.Justification = append(rec.Justification, "the canonical token streams of all built Go sources are identical; only tests, comments, formatting or non-compiled files changed")
		for _, f := range r.Findings {
			if f.Kind == "file-config" {
				rec.Justification = append(rec.Justification, "configuration changed ("+f.Symbol+"): apply by config reload (an S1-style epoch swap of the config object) or by restarting with the new ConfigMap; no code mechanism needed")
				break
			}
		}
	case wasmOnly && !has(C4):
		rec.Mechanism, rec.InProcess = "S3", true
		rec.Summary = fmt.Sprintf("Swap the Wasm guest module (S3): %d changed guest function(s), host unchanged.", r.PatchSetSize)
		rec.Justification = append(rec.Justification, "every code change is inside a wasip1 guest package; S3 instantiates the new module (re-running its initializers) while the host keeps the state, and drains the old module by reference count")
		c.risk("guest-internal state is not migrated by S3; any change to //go:wasmexport signatures or host imports changes the host ABI")
		rec.Alternatives = append(rec.Alternatives, "S5 handoff if the host ABI (imports/exports) changed", "rolling update")
	case inProcOnly && len(blockers) == 0:
		rec.Mechanism, rec.InProcess = "S6", true
		what := "body-only"
		if has(C2) {
			what = "interface-level (signature/added/removed functions) with call-graph closure"
		}
		rec.Summary = fmt.Sprintf("S6 transactional patch set of %d function(s) (%s change).", r.PatchSetSize, what)
		rec.Justification = append(rec.Justification,
			"no state-schema, dependency, toolchain, goroutine-structure or long-running-code change",
			"every member of the patch set is an ordinary non-generic function or method, so each can be redirected through its slot, and the whole set is published in one atomic PatchSet store")
		if has(C2) {
			rec.Justification = append(rec.Justification, "signature changes and removals were closed over their callers; new functions and types live in the patch plugin")
		}
		rec.Alternatives = append(rec.Alternatives,
			"S2 Go plugin swap of the affected handler (same toolchain; old code leaks)",
			"S1 epoch swap if v2 is compiled in alongside v1 (pre-staged release)",
			"S5 FD-passing handoff (identity state transfer) or rolling update as fallback")
	case !has(C3):
		rec.Mechanism, rec.InProcess = "S5", false
		rec.Summary = "S5 FD-passing process handoff with identity state transfer (new binary)."
		if has(C4) {
			rec.Justification = append(rec.Justification, "dependency/toolchain/runtime/native/embedded change needs a new binary; S5 keeps the listening socket and transfers state unchanged (state schema identical)")
		}
		if has(C5) {
			rec.Justification = append(rec.Justification, "goroutine-structure, long-running or start-up code changed: in-process patching would leave old code running in goroutines that never return; a new process starts the new goroutine structure, and the old one is drained and exits")
			c.risk("quiescence: old activations of main(), init-started or long-running goroutines never return, so the old process is retired by exit, not by draining; set a drain deadline in the S5 upgrader")
		}
		if !has(C4) && !has(C5) {
			rec.Justification = append(rec.Justification, "the change is interface-level but not S6-patchable ("+strings.Join(limit(blockers, 3), "; ")+"); state schema is unchanged, so S5 with identity transfer is the cheapest safe choice")
		}
		rec.Alternatives = append(rec.Alternatives, "Kubernetes rolling update (loses in-memory state, needs surge capacity)")
	default:
		rec.Mechanism, rec.InProcess = "S5+xform", false
		rec.Summary = "S5 FD-passing handoff with a state transformer (in-memory state schema changed)."
		rec.Justification = append(rec.Justification,
			"the layout of in-memory state changed, so no in-process mechanism can keep the old objects; the new process needs the old state mapped to the new schema (Kitsune/MCR-style state transfer)",
			"a transformer skeleton was generated (old -> new, field by field, TODOs for added/retyped fields)")
		c.risk("state transformer must be completed and reviewed; encoding/json does not carry unexported fields, so the S5 state envelope needs an explicit DTO for them")
		rec.Alternatives = append(rec.Alternatives, "rolling update if losing the in-memory state is acceptable (cold caches)")
	}
	if has(C4) {
		for _, f := range r.Findings {
			if f.Kind == "gomod-go" || f.Kind == "gomod-toolchain" {
				c.risk("toolchain changed: S2/S6 plugin delivery is impossible (plugins must match the host toolchain exactly); only a new process works")
				break
			}
		}
	}
	if len(blockers) > 0 && rec.Mechanism != "none" {
		rec.Justification = append(rec.Justification, fmt.Sprintf("S6 blockers (%d): %s", len(blockers), strings.Join(limit(blockers, 5), "; ")))
	}
	rec.Risks = uniqKeepOrder(c.risks)
	if len(c.xform) > 0 {
		r.StateXform = genXform(c.xform)
	}
	sort.SliceStable(r.Findings, func(i, j int) bool {
		if r.Findings[i].Class != r.Findings[j].Class {
			return r.Findings[i].Class < r.Findings[j].Class
		}
		if r.Findings[i].Symbol != r.Findings[j].Symbol {
			return r.Findings[i].Symbol < r.Findings[j].Symbol
		}
		return r.Findings[i].Kind < r.Findings[j].Kind
	})
}

func (c *classifier) ownerPkg(sym string) *Package {
	best := (*Package)(nil)
	for ip, p := range c.n.Packages {
		if sym == ip {
			return p
		}
		if strings.HasPrefix(sym, ip+".") && !strings.Contains(sym[len(ip)+1:], "/") {
			if best == nil || len(ip) > len(best.Path) {
				best = p
			}
		}
	}
	return best
}

func limit(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return append(append([]string(nil), xs[:n]...), fmt.Sprintf("... and %d more", len(xs)-n))
}

func uniqKeepOrder(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func relPath(base, p string) (string, error) {
	r, err := filepath.Rel(base, p)
	return filepath.ToSlash(r), err
}

// linkedClosure returns every package path transitively imported by the
// snapshot (as far as export data and fake packages reveal it).
func linkedClosure(s *Snapshot) map[string]bool {
	seen := map[string]bool{}
	var walk func(p *types.Package)
	walk = func(p *types.Package) {
		if p == nil || seen[p.Path()] {
			return
		}
		seen[p.Path()] = true
		for _, q := range p.Imports() {
			walk(q)
		}
	}
	for _, p := range s.Packages {
		walk(p.tpkg)
	}
	return seen
}
