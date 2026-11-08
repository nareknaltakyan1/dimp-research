// Command eval runs dimpclass over the recent git history of real Go
// services and reports the distribution of update classes (S8 evaluation).
//
//	go run ./tools/dimpclass/eval \
//	    -target chi=/path/to/corpus/chi: \
//	    -target gotify=/path/to/corpus/gotify-server: \
//	    -target frontend=/path/to/corpus/microservices-demo:src/frontend \
//	    -n 150 -out results/dimpclass
//
// For every target (name=repo:subdir) it walks `git log --first-parent`
// over subdir, keeps the most recent N commits whose diff against their
// first parent touches a .go file, go.mod or go.sum below subdir, extracts
// both trees (Go sources, go.mod, go.sum only) with git archive, and runs
// dimpclass.Analyze on the pair. Each pair is one deployable update (merge
// commits are compared with their first parent, i.e. the whole merged PR).
package main

import (
	"archive/tar"
	"bytes"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nareknaltakyan1/dimp-research/tools/dimpclass"
)

type target struct{ name, repo, sub string }

type targets []target

func (t *targets) String() string { return fmt.Sprint(*t) }
func (t *targets) Set(v string) error {
	name, rest, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("want name=repo:subdir")
	}
	repo, sub, _ := strings.Cut(rest, ":")
	*t = append(*t, target{name, repo, strings.Trim(sub, "/")})
	return nil
}

type row struct {
	Target, Commit, Parent, Date, Author, Subject string
	Bot                                           bool
	GoFiles                                       int
	Classes, Dominant, Mechanism                  string
	InProcess                                     bool
	PatchSize, Findings, Blockers                 int
	Kinds                                         string
	TypeErrOld, TypeErrNew                        int
	Millis                                        int64
}

func main() {
	var ts targets
	flag.Var(&ts, "target", "name=repo:subdir (repeatable)")
	n := flag.Int("n", 150, "commits per target")
	out := flag.String("out", "results/dimpclass", "output directory")
	sample := flag.Int("sample", 20, "random sample size for manual validation")
	seed := flag.Int64("seed", 8, "sampling seed")
	flag.Parse()
	if len(ts) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	must(os.MkdirAll(*out, 0o755))
	var all []row
	for _, t := range ts {
		rows := runTarget(t, *n, *out)
		writeCSV(filepath.Join(*out, t.name+".csv"), rows)
		all = append(all, rows...)
	}
	writeCSV(filepath.Join(*out, "all.csv"), all)
	writeSample(filepath.Join(*out, "validation-sample.csv"), all, *sample, *seed)
	must(os.WriteFile(filepath.Join(*out, "summary.md"), []byte(summary(ts, all, *n)), 0o644))
	fmt.Println("wrote", *out)
}

func git(repo string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "git %v: %v %s\n", args, err, stderr.String())
		os.Exit(1)
	}
	return string(b)
}

func runTarget(t target, n int, out string) []row {
	pathArg := t.sub
	if pathArg == "" {
		pathArg = "."
	}
	log := git(t.repo, "log", "--first-parent", "--format=%H%x09%P%x09%cI%x09%an%x09%s", "--", pathArg)
	var rows []row
	jsonDir := filepath.Join(out, "reports", t.name)
	must(os.MkdirAll(jsonDir, 0o755))
	tmp, err := os.MkdirTemp("", "dimpclass-eval-")
	must(err)
	defer os.RemoveAll(tmp)
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if len(rows) >= n {
			break
		}
		f := strings.SplitN(line, "\t", 5)
		if len(f) < 5 {
			continue
		}
		commit, parents, date, author, subj := f[0], strings.Fields(f[1]), f[2], f[3], f[4]
		if len(parents) == 0 {
			continue
		}
		parent := parents[0]
		changed := strings.Fields(git(t.repo, "diff", "--name-only", parent, commit, "--", pathArg))
		goFiles, relevant := 0, false
		for _, c := range changed {
			if strings.HasSuffix(c, ".go") {
				goFiles++
				relevant = true
			}
			if strings.HasSuffix(c, "/go.mod") || c == "go.mod" || strings.HasSuffix(c, "go.sum") {
				relevant = true
			}
		}
		if !relevant {
			continue
		}
		oldDir := filepath.Join(tmp, commit[:12], "old")
		newDir := filepath.Join(tmp, commit[:12], "new")
		extract(t, parent, oldDir)
		extract(t, commit, newDir)
		start := time.Now()
		r, err := dimpclass.Analyze(filepath.Join(oldDir, t.sub), filepath.Join(newDir, t.sub))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s %s: %v\n", t.name, commit[:12], err)
			os.RemoveAll(filepath.Join(tmp, commit[:12]))
			continue
		}
		ms := time.Since(start).Milliseconds()
		r.Old, r.New = t.name+"@"+parent[:12], t.name+"@"+commit[:12]
		js, _ := r.JSON()
		must(os.WriteFile(filepath.Join(jsonDir, commit[:12]+".json"), js, 0o644))
		kinds := map[string]bool{}
		blockers := 0
		for _, fd := range r.Findings {
			kinds[fd.Kind] = true
			if fd.S6Blocker != "" && fd.Class != dimpclass.C0 {
				blockers++
			}
		}
		la := strings.ToLower(author)
		rows = append(rows, row{
			Target: t.name, Commit: commit[:12], Parent: parent[:12], Date: date[:10], Author: author,
			Subject: subj, Bot: strings.Contains(la, "bot") || strings.Contains(la, "renovate") || strings.Contains(strings.ToLower(subj), "chore(deps)"),
			GoFiles: goFiles, Classes: strings.Join(r.Classes, " "), Dominant: r.Dominant,
			Mechanism: r.Recommendation.Mechanism, InProcess: r.Recommendation.InProcess,
			PatchSize: r.PatchSetSize, Findings: len(r.Findings), Blockers: blockers,
			Kinds: strings.Join(sortedKeys(kinds), " "), TypeErrOld: r.Stats.OldTypeErrors, TypeErrNew: r.Stats.NewTypeErrors,
			Millis: ms,
		})
		os.RemoveAll(filepath.Join(tmp, commit[:12]))
		fmt.Fprintf(os.Stderr, "%s %d/%d %s %s %s %dms\n", t.name, len(rows), n, commit[:12], r.Dominant, r.Recommendation.Mechanism, ms)
	}
	return rows
}

// extract writes the Go sources, go.mod and go.sum of rev below t.sub to dir.
func extract(t target, rev, dir string) {
	must(os.MkdirAll(dir, 0o755))
	prefix := ""
	if t.sub != "" {
		prefix = t.sub + "/"
	}
	// One archive per pathspec: git archive fails as a whole if any
	// pathspec matches nothing (e.g. a revision without go.sum).
	for _, spec := range []string{"**/*.go", "**/go.mod", "**/go.sum"} {
		b, err := exec.Command("git", "-C", t.repo, "archive", rev, "--", ":(glob)"+prefix+spec).Output()
		if err != nil {
			continue
		}
		untar(b, dir)
	}
}

func untar(b []byte, dir string) {
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		must(err)
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(p, filepath.Clean(dir)+string(os.PathSeparator)) {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			must(os.MkdirAll(p, 0o755))
		case tar.TypeReg:
			must(os.MkdirAll(filepath.Dir(p), 0o755))
			data, err := io.ReadAll(tr)
			must(err)
			must(os.WriteFile(p, data, 0o644))
		}
	}
}

func writeCSV(p string, rows []row) {
	f, err := os.Create(p)
	must(err)
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"target", "commit", "parent", "date", "author", "bot", "subject", "go_files", "classes", "dominant",
		"mechanism", "in_process", "patch_size", "findings", "s6_blockers", "kinds", "type_err_old", "type_err_new", "ms"})
	for _, r := range rows {
		w.Write([]string{r.Target, r.Commit, r.Parent, r.Date, r.Author, strconv.FormatBool(r.Bot), r.Subject,
			strconv.Itoa(r.GoFiles), r.Classes, r.Dominant, r.Mechanism, strconv.FormatBool(r.InProcess),
			strconv.Itoa(r.PatchSize), strconv.Itoa(r.Findings), strconv.Itoa(r.Blockers), r.Kinds,
			strconv.Itoa(r.TypeErrOld), strconv.Itoa(r.TypeErrNew), strconv.FormatInt(r.Millis, 10)})
	}
	w.Flush()
	must(w.Error())
}

// writeSample draws a stratified-by-target random sample for manual review.
func writeSample(p string, rows []row, k int, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	idx := rng.Perm(len(rows))
	if k > len(rows) {
		k = len(rows)
	}
	pick := append([]int(nil), idx[:k]...)
	sort.Ints(pick)
	var s []row
	for _, i := range pick {
		s = append(s, rows[i])
	}
	writeCSV(p, s)
}

var order = []string{"C0", "C1", "C2", "C3", "C4", "C5"}

func pct(a, b int) string {
	if b == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(a)/float64(b))
}

func summary(ts targets, all []row, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# dimpclass evaluation on real history\n\n")
	fmt.Fprintf(&b, "Generated %s by `tools/dimpclass/eval` (last %d first-parent commits per target that touch Go sources, go.mod or go.sum).\n\n", time.Now().UTC().Format("2006-01-02"), n)
	groups := map[string][]row{}
	for _, r := range all {
		groups[r.Target] = append(groups[r.Target], r)
	}
	var names []string
	for _, t := range ts {
		names = append(names, t.name)
	}
	names = append(names, "ALL")
	groups["ALL"] = all
	nonBot := []row{}
	for _, r := range all {
		if !r.Bot {
			nonBot = append(nonBot, r)
		}
	}
	names = append(names, "ALL (no bots)")
	groups["ALL (no bots)"] = nonBot

	b.WriteString("## Dominant class per commit pair\n\n| target | pairs | ")
	for _, c := range order {
		b.WriteString(c + " | ")
	}
	b.WriteString("\n|---|---|---|---|---|---|---|---|\n")
	for _, nm := range names {
		rs := groups[nm]
		cnt := map[string]int{}
		for _, r := range rs {
			cnt[r.Dominant]++
		}
		fmt.Fprintf(&b, "| %s | %d | ", nm, len(rs))
		for _, c := range order {
			fmt.Fprintf(&b, "%d (%s) | ", cnt[c], pct(cnt[c], len(rs)))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n## Classes present (multi-label; a commit counts once per class it contains)\n\n| target | ")
	for _, c := range order {
		b.WriteString(c + " | ")
	}
	b.WriteString("\n|---|---|---|---|---|---|---|\n")
	for _, nm := range names {
		rs := groups[nm]
		cnt := map[string]int{}
		for _, r := range rs {
			for _, c := range strings.Fields(r.Classes) {
				cnt[c]++
			}
		}
		fmt.Fprintf(&b, "| %s | ", nm)
		for _, c := range order {
			fmt.Fprintf(&b, "%s | ", pct(cnt[c], len(rs)))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n## Recommended mechanism\n\n| target | none | S6 | S3 | S5 | S5+xform | in-process (none/S6/S3) | median patch set (S6) | max patch set (S6) |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, nm := range names {
		rs := groups[nm]
		cnt := map[string]int{}
		in := 0
		var sizes []int
		for _, r := range rs {
			cnt[r.Mechanism]++
			if r.InProcess {
				in++
			}
			if r.Mechanism == "S6" {
				sizes = append(sizes, r.PatchSize)
			}
		}
		sort.Ints(sizes)
		med, mx := "-", "-"
		if len(sizes) > 0 {
			med = strconv.Itoa(sizes[len(sizes)/2])
			mx = strconv.Itoa(sizes[len(sizes)-1])
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", nm,
			pct(cnt["none"], len(rs)), pct(cnt["S6"], len(rs)), pct(cnt["S3"], len(rs)), pct(cnt["S5"], len(rs)),
			pct(cnt["S5+xform"], len(rs)), pct(in, len(rs)), med, mx)
	}
	b.WriteString("\n## Bucket view (thesis question)\n\n")
	b.WriteString("* **in-process hot-patchable**: dominant class C0, C1 or C2 *and* mechanism none/S6/S3\n")
	b.WriteString("* **C0-C2 but S6-blocked**: dominant C1/C2 with an S6 blocker (type layout, var initializer, generic) -> S5 identity handoff\n")
	b.WriteString("* **state transfer**: dominant C3 -> S5 + transformer\n")
	b.WriteString("* **new process**: dominant C4 or C5 -> S5 (or rolling update)\n\n")
	b.WriteString("| target | pairs | in-process | C0-C2 S6-blocked | state transfer (C3) | new process (C4/C5) |\n|---|---|---|---|---|---|\n")
	for _, nm := range names {
		rs := groups[nm]
		var a, bl, s3, np int
		for _, r := range rs {
			switch {
			case r.Dominant == "C3":
				s3++
			case r.Dominant == "C4" || r.Dominant == "C5":
				np++
			case r.InProcess:
				a++
			default:
				bl++
			}
		}
		fmt.Fprintf(&b, "| %s | %d | %d (%s) | %d (%s) | %d (%s) | %d (%s) |\n", nm, len(rs), a, pct(a, len(rs)), bl, pct(bl, len(rs)), s3, pct(s3, len(rs)), np, pct(np, len(rs)))
	}
	// most frequent finding kinds
	b.WriteString("\n## Most frequent finding kinds (share of commits containing the kind)\n\n| kind | commits |\n|---|---|\n")
	kc := map[string]int{}
	for _, r := range all {
		for _, k := range strings.Fields(r.Kinds) {
			kc[k]++
		}
	}
	var ks []string
	for k := range kc {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if kc[ks[i]] != kc[ks[j]] {
			return kc[ks[i]] > kc[ks[j]]
		}
		return ks[i] < ks[j]
	})
	for _, k := range ks {
		fmt.Fprintf(&b, "| %s | %d (%s) |\n", k, kc[k], pct(kc[k], len(all)))
	}
	var ms []int64
	for _, r := range all {
		ms = append(ms, r.Millis)
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i] < ms[j] })
	if len(ms) > 0 {
		fmt.Fprintf(&b, "\nAnalysis time per pair (load + type-check both versions + classify): median %d ms, p95 %d ms, max %d ms.\n",
			ms[len(ms)/2], ms[len(ms)*95/100], ms[len(ms)-1])
	}
	return b.String()
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		os.Exit(1)
	}
}
