package dimpclass

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// JSON renders the report as indented JSON.
func (r *Report) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Markdown renders a human-readable report.
func (r *Report) Markdown() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# dimpclass update report\n\n")
	fmt.Fprintf(&b, "* old: `%s`\n* new: `%s`\n* module: `%s`\n\n", r.Old, r.New, r.Module)
	fmt.Fprintf(&b, "## Classification\n\n")
	fmt.Fprintf(&b, "**Dominant class: %s** (%s)\n\n", r.Dominant, ClassDescriptions[r.Dominant])
	fmt.Fprintf(&b, "Classes present: %s\n\n", strings.Join(r.Classes, ", "))
	rec := r.Recommendation
	fmt.Fprintf(&b, "## Recommended mechanism: %s\n\n%s\n\n", rec.Mechanism, rec.Summary)
	if len(rec.Justification) > 0 {
		b.WriteString("Justification:\n\n")
		for _, j := range rec.Justification {
			fmt.Fprintf(&b, "* %s\n", j)
		}
		b.WriteString("\n")
	}
	if len(rec.Alternatives) > 0 {
		b.WriteString("Alternatives:\n\n")
		for _, a := range rec.Alternatives {
			fmt.Fprintf(&b, "* %s\n", a)
		}
		b.WriteString("\n")
	}
	if len(rec.Risks) > 0 {
		b.WriteString("Risks:\n\n")
		for _, a := range rec.Risks {
			fmt.Fprintf(&b, "* %s\n", a)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "## Patch set (%d functions)\n\n", r.PatchSetSize)
	if r.PatchSetSize > 0 {
		b.WriteString("| function | reason | ctx | inlinable |\n|---|---|---|---|\n")
		for _, p := range r.PatchSet {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", p.Symbol, p.Reason, yn(p.CtxParam), yn(p.Inlinable))
		}
		b.WriteString("\n")
	}
	if len(r.InlineCallers) > 0 {
		fmt.Fprintf(&b, "Inline-sensitive callers (needed only for raw binary patching): %s\n\n", "`"+strings.Join(r.InlineCallers, "`, `")+"`")
	}
	b.WriteString("## Semantic diff\n\n")
	if len(r.Findings) == 0 {
		b.WriteString("No differences.\n\n")
	} else {
		b.WriteString("| class | kind | symbol | detail | S6 blocker |\n|---|---|---|---|---|\n")
		for _, f := range r.Findings {
			fmt.Fprintf(&b, "| %s | %s | `%s` | %s | %s |\n", f.Class, f.Kind, f.Symbol, mdEsc(f.Detail), mdEsc(f.S6Blocker))
		}
		b.WriteString("\n")
	}
	s := r.Stats
	fmt.Fprintf(&b, "Stats: packages %d -> %d, functions %d -> %d, changed functions %d, changed types %d, changed globals %d, type errors %d/%d (old/new).\n\n",
		s.OldPackages, s.NewPackages, s.OldFuncs, s.NewFuncs, s.ChangedFuncs, s.ChangedTypes, s.ChangedGlobals, s.OldTypeErrors, s.NewTypeErrors)
	if r.StateXform != "" {
		b.WriteString("## State transformer skeleton\n\n```go\n")
		b.WriteString(r.StateXform)
		b.WriteString("```\n")
	}
	return b.String()
}

func yn(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func mdEsc(s string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ").Replace(s)
}
