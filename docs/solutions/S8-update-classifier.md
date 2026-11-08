# S8 - Update classifier and mechanism selector (`tools/dimpclass`)

Code: `tools/dimpclass/` (library: `load.go` loader and type-checker,
`extract.go` per-version model, `classify.go` semantic diff, classes and
mechanism selection, `xform.go` state-transformer skeleton generator,
`report.go` JSON/Markdown), `tools/dimpclass/cmd/dimpclass/` (CLI),
`tools/dimpclass/eval/` (real-history evaluation driver),
`tools/dimpclass/testdata/cases/` (13 golden old/new pairs). Results:
`results/dimpclass/`. Standard library only (`go/ast`, `go/parser`,
`go/types`, `go/importer`, `go/scanner`, `go/build`). No `golang.org/x/tools`.

```
go run ./tools/dimpclass/cmd/dimpclass [-format md|json|both] [-o out] [-xform xf.go] OLD_DIR NEW_DIR
go test ./tools/dimpclass/...            # golden corpus (-update to regenerate)
```

## 1. Motivation

S1-S6 are *mechanisms*. Each is cheapest for one kind of change and unsafe
or impossible for others:

* S6 (hotpatch-ready compilation) can swap function bodies through slots.
  It cannot change a compiled-in type layout, re-run an initializer, or move
  a goroutine that never returns.
* S3 can replace a Wasm guest module wholesale, but only code that lives in
  the guest.
* S5 (FD passing) can deploy any new binary, but it pays a process start. A
  changed state schema also needs a state transformer.
* A Kubernetes rolling update always works, but it loses in-memory state.

An operator, or a future controller (Gap G5), needs a decision procedure: *given v1 and v2, what kind
of update is this, and what is the cheapest mechanism that is still safe?*
The same procedure answers an empirical question the thesis needs: **what fraction
of real commits could be hot-patched in-process at all?** This mirrors the
classic DSU observation that most updates change function bodies, while the
hard minority changes types or state. Ksplice reports that 56 of 64 kernel
security patches needed no programmer-written code [`arnold2009ksplice`].
Java HotSwap is limited to method-body changes
[`java-instrumentation`], so DCEVM, JVolve and Rubah exist to handle class-schema
changes [`wurthinger2010dcevm`; `subramanian2009jvolve`; `pina2014rubah`].
No such measurement exists for Go microservices.

## 2. Update class taxonomy

| class | meaning | literature analogue | cheapest safe mechanism |
|---|---|---|---|
| C0 | no runtime code change: config, docs, tests, comments, formatting | | none (config reload) |
| C1 | body-only change of patchable functions; a changed constant patches all its users | HotSwap-supported change [`java-instrumentation`]; Ksplice "no new code" patches [`arnold2009ksplice`] | S6 patch set (S3 if the code is in a Wasm guest) |
| C2 | signatures, added or removed functions, interface or non-state type changes, package-level var add/remove/initializer | Ginseng-style function and type indirection [`neamtiu2006ginseng`]; con-freeness [`stoyle2005mutatis`] | S6 patch set over the call-graph closure, else S5 with identity transfer |
| C3 | schema change of **in-memory state** (fields of reachable struct types, global var types) | state transformers: Kitsune xfgen [`hayden2012kitsune`], MCR [`giuffrida2014mcr`], JVolve/TOS [`subramanian2009jvolve`; `magill2012tos`], "state transfer is the hard part" [`giuffrida2012statetransfer`] | S5 plus a state transformer |
| C4 | dependency, toolchain (`go`/`toolchain` directive), cgo/native, Dockerfile, `//go:embed` asset | none for Go; plugins need identical toolchains [`go-plugin`] | S5 (new binary) |
| C5 | code that will never be re-executed or never returns: `main`, `init`, start-up-only functions, closure factories, long-running loops, goroutine-structure changes | quiescence [`kramer1990evolving`; `hayden2012quiescence`], active functions [`strombaeck2024activedsu`], induced update points [`neamtiu2009timely`] | S5 (old process retired by exit) or rolling update |

An update usually spans several classes. The report lists all of them (the
multi-label view) and a **dominant class** by cost order
C0 < C1 < C2 < C4 < C5 < C3. C3 ranks highest because it needs a new process
*and* a reviewed transformer.

Two C5 sub-rules came out of the evaluation and are, in our view, the most
interesting Go-specific findings:

* **Start-up-only code.** A function reached *only by direct calls* from
  `main`/`init`, or from other start-up-only functions, has already run. Its
  effect is baked into live state: routes registered, middleware chains
  built, configuration parsed, goroutines spawned. Patching its body changes
  nothing. This holds unless the function is also used as a value (handler,
  callback, method value, var initializer). Calls from inside a function
  literal count as deferred, not start-up, calls.
* **Closure factories.** If a function returns a closure (`func(...)` or
  `http.Handler`) and the change is confined to the returned function
  literal, closures created before the patch keep the old code. Go
  middleware is built exactly this way. S6 slot trampolines redirect calls to
  *named* functions, but they do not redirect closures already captured in
  state. This is the Go version of the "function pointers stored in the heap"
  problem that Ginseng solves by type wrapping [`neamtiu2006ginseng`].

## 3. Algorithm

1. **Load.** If the directory has a `go.mod`, the whole module is loaded:
   every package directory except vendor, testdata, hidden and nested
   modules. Otherwise one package is loaded, and the old package's import
   path is reused for the new copy. Files are selected with
   `go/build.Context.MatchFile` for linux/amd64, falling back to
   wasip1/wasm, which marks S3 guest packages. Standard-library imports are
   type-checked from export data. Module-local imports are type-checked
   recursively from source. Third-party imports become empty "fake"
   packages, so their identifiers are opaque. Type errors are counted, not
   fatal.
2. **Extract a per-version model.** It contains:
   * functions and methods (key `pkg.F` / `pkg.T.M`), with a canonical
     signature using import paths and no parameter names
   * a body fingerprint over the `go/scanner` token stream, so comments and
     layout are ignored
   * resolved references, split into direct calls and value uses, plus
     constants and types used
   * `go` statement targets, long-running heuristics, an inlinability
     heuristic, generic-ness, a `context.Context` parameter flag, a
     start-up-only flag and a closure-factory fingerprint
   * named types (struct fields with type and tag, interface method sets),
     package vars (type, initializer, `//go:embed`), constant values, imports
   * `go.mod` (own line parser) and a hash of every other file.
3. **State types.** The roots are package-level var types (excluding func,
   error and embed vars) and struct types with a field of type
   `sync.Mutex`/`RWMutex`/`Map`/`Pool`, `sync/atomic.*`, map or chan. Those
   fields mark a long-lived shared object such as a cache, session table,
   server or throttler. Everything reachable from a root through fields,
   pointers, slices, arrays, maps and channels counts as state.
4. **Diff and classify** (`classify.go`). Every difference becomes a
   `Finding{class, kind, symbol, detail, s6_blocker}`. These are the main
   rules:

   | Change | Class | Notes |
   |---|---|---|
   | Body change | C1 | C5 if it is `main`/`init`, start-up-only, a closure factory, long-running, or changes `go` statements |
   | Signature change | C2 | Old-version callers are added to the patch set (call-graph closure). A caller that never returns turns it into C5 |
   | Constant value change | C1 | Every user function joins the patch set, because constants are compiled in |
   | Struct change on a state type | C3 | Field added, removed, retyped or reordered, or `json` key changed |
   | Struct change on a non-state type | C2 | S6-blocked (type layout is compiled in) |
   | Interface method-set change | C2 | S6-blocked (itabs) |
   | Var initializer change | C2 | S6-blocked (initializers run once) |
   | Var type change | C3 | C2 for func-typed hook vars |
   | `go.mod`, `toolchain`, cgo, native, Dockerfile or embedded-asset change | C4 | |
   | Newly linked import | C2 | C1 if the package was already linked transitively |
5. **Select the mechanism.**

   | condition | mechanism |
   |---|---|
   | only C0 | `none` (config reload, an S1-style epoch swap of the config object) |
   | all code findings inside wasip1 guest packages, no C4 | `S3` |
   | max C1/C2 and no S6 blocker | `S6` transactional patch set; size = number of functions |
   | C4 or C5 (no C3), or C1/C2 with S6 blockers | `S5` with identity state transfer |
   | C3 present | `S5+xform`, plus a generated transformer skeleton |

   Rolling update is always listed as the fallback. Risks are attached as
   notes, for example:
   * patched functions without a `context.Context` parameter get per-call,
     not per-request, consistency in S6
   * functions in package `main` cannot be imported by a plugin
   * inlinable changed functions list the "inline-sensitive callers" that a
     raw binary patcher (`internal/livepatch`) would also have to patch
   * exported functions with changed signatures have callers outside the
     module
   * partial type information.
6. **State-transformer skeleton** (`xform.go`; research hook for Idea 7 /
   Gap G9). For each C3 struct it emits mirror types `TV1` and `TV2`, because
   both versions share one import path. It also emits
   `TransformT(in TV1) (TV2, error)`:
   * unchanged fields are copied
   * numeric retypes are converted, with a TODO
   * other retypes, added fields and dropped fields get TODOs
   * nested maps, slices and pointers of changed types get element-wise
     loops.

   The golden test type-checks every skeleton. In the corpus case
   `c3-cache-entry-field` the generator writes
   `out.TTLSecs = time.Duration(in.TTLSecs) // TODO(retyped): int64 -> time.Duration; check units`.
   This conversion is type-correct but semantically wrong (seconds vs
   nanoseconds), which shows why the TODOs are needed. This is the gap an
   LLM- or synthesis-based completer (cf. TOS [`magill2012tos`]) would fill.

## 4. Golden corpus

There are 13 realistic old/new pairs (`testdata/cases`). Each has
`expected.json`, `expected.md` and, for C3, `expected_xform.go.txt`. An
expectation table in `dimpclass_test.go` pins dominant class, mechanism,
patch-set size and class set, so a golden file cannot drift silently.

| case | result |
|---|---|
| c0-config-comments (yaml, comment/format-only Go, test, README) | C0, none |
| c1-handler-bugfix (missing 404) | C1, S6, 1 fn |
| c1-const-change (`maxRetries` 3 -> 5) | C1, S6, 2 fns (all users) |
| c1-method-body (`Cache.Get` expiry) | C1, S6, 1 fn |
| c1-wasm-guest (`//go:wasmexport` rule) | C1, S3 |
| c2-signature-change (`lookupPrice` gains ctx and currency, new helper) | C2, S6, 4 fns (closure) |
| c2-interface-change (`Store` gains `Delete`) | C2, S5 (S6-blocked) |
| c3-cache-entry-field (Entry: +Hits, -ETag, TTL int64 -> Duration) | C3, S5+xform |
| c4-dependency-bump (uuid v1.3.0 -> v1.6.0) | C4, S5 |
| c5-background-goroutine (new janitor `for{select}`) | C5, S5 |
| c5-main-new-route | C5, S5 |
| c5-startup-wiring (route added in a start-up-only `routes()`) | C5, S5 |
| c5-closure-factory (fix inside returned middleware closure) | C5, S5 |

`go vet ./tools/dimpclass/...` is clean. `go test ./tools/dimpclass/...`
passes on macOS/arm64 with Go 1.24.3. The repository's S3 guests compare
as expected: `dimpclass cmd/s3-wasm/guest/v1 cmd/s3-wasm/guest/v2` gives C2
(var initializer `Version`) -> S3.

## 5. Evaluation on real history

**Setup** (`tools/dimpclass/eval`, raw data in `results/dimpclass/`).
Three targets were cloned into `~/research/corpus/`:

| target | what it is | scope |
|---|---|---|
| go-chi/chi | HTTP router library | whole module, including `middleware` |
| gotify/server | self-hosted push-notification service | whole Go module |
| GoogleCloudPlatform/microservices-demo `src/frontend` | Online Boutique frontend microservice | its own module |

* Commits: per target, the most recent **150** first-parent commits whose
  diff against the first parent touches a `.go` file, `go.mod` or `go.sum`
  below the scope. That is 450 pairs. A merge commit is compared with its
  first parent, so one pair is one deployable update.
* Extraction: both trees are extracted with `git archive`, Go sources plus
  `go.mod`/`go.sum` only.
* Output: per-commit JSON reports in `results/dimpclass/reports/`.
* Speed: median 22 ms per pair, p95 44 ms.

**Dominant class per commit pair** (`results/dimpclass/summary.md`):

| target | pairs | C0 | C1 | C2 | C3 | C4 | C5 |
|---|---|---|---|---|---|---|---|
| chi | 150 | 33.3% | 28.0% | 20.7% | 3.3% | 3.3% | 11.3% |
| gotify | 150 | 2.7% | 12.0% | 10.0% | 2.0% | 33.3% | 40.0% |
| frontend | 150 | 0.0% | 1.3% | 1.3% | 0.0% | 90.7% | 6.7% |
| **all** | 450 | 12.0% | 13.8% | 10.7% | **1.8%** | 42.4% | 19.3% |
| all without bot commits (301) | 301 | 17.6% | 20.6% | 15.9% | 2.7% | 14.6% | 28.6% |

**Thesis buckets:**

| target | in-process (none/S6/S3) | C0-C2 but S6-blocked -> S5 | state transfer (C3) | new process (C4/C5) |
|---|---|---|---|---|
| chi | 75.3% | 6.7% | 3.3% | 14.7% |
| gotify | 20.0% | 4.7% | 2.0% | 73.3% |
| frontend | 2.7% | 0.0% | 0.0% | 97.3% |
| **all** | **32.7%** | 3.8% | **1.8%** | **61.8%** |
| all without bots | **48.5%** | 5.6% | 2.7% | 43.2% |

S6 patch sets are small: median 1 function, maximum 10.

**Findings.**

1. **State-schema changes are rare: 1.8% of all pairs (2.7% excluding bots).**
   This is the class that motivates Kitsune/MCR-style transformers, and the
   transformer burden is real but infrequent. Three of the 8 C3 pairs
   change a field or global type; the rest add, remove or reorder fields.
2. **Process replacement dominates real services, but mostly not because
   of code.** Of the 450 pairs, 42.4% are dominated by C4. Nearly all are
   Renovate/Dependabot dependency or toolchain bumps (frontend: 90.7%).
   S6/S2 plugin delivery is impossible after a toolchain change. For these,
   S5 handoff (new binary, identity state transfer) is the right in-cluster
   alternative to a rolling update.
3. **For human-written commits, about half are hot-patchable in-process.**
   In the library, which has no start-up code, it is three quarters.
4. **Start-up-only code is the dominant C5 cause.** Among the 87 C5 pairs:
   * start-up-only changes: 62 (`router.Create`, `config.Get`, middleware
     factories)
   * closure factories: 20 (mostly chi middleware)
   * `main` changes: 13
   * genuinely long-running loops and goroutine-structure changes: 3

   In Go services much of a change's effect lives in the *object graph built
   at start-up* (routers, chains, closures) rather than in code called per
   request. A code-only patcher silently does nothing for these changes.
   Detecting this is necessary for *safe* in-process patching, and it argues
   for building patchable services as "re-runnable wiring", for example an
   S1-style swap of a whole router.
5. S6 blockers inside C1/C2 are mostly:
   * var-initializer changes: 8
   * non-state struct layout or tag changes: 9
   * interface method-set changes: 3

## 6. Manual validation

I read the actual diffs of two samples (`results/dimpclass/manual-validation.csv`):

* **Random sample**, 20 pairs, seed 8 (`validation-sample.csv`).
* **Hard sample**: 10 more pairs drawn at random only from non-bot, non-C0
  commits (`validation-sample-hard.csv`). The random sample is dominated by
  easy cases: 6 bot bumps and 6 test/comment commits.

| sample | agreement with my manual class and mechanism |
|---|---|
| random (20) | **20/20** in the final version. The first run had 19/20: chi 05f1ef7 put a fix inside a returned middleware closure, which was classified C1/S6 but needs a restart. That disagreement led to the closure-factory rule. |
| hard (10) | **9/10**. Disagreement: frontend 03e209e. The tool says C2/S6. In truth the new value feeds HTML templates parsed once at start-up, and `templates/header.html` (outside the Go-only extraction) and a K8s manifest env var also changed, so a restart is needed. |
| total | **29/30 (97%)** for the final tool, but see the caveats below |

Caveats:

* The random sample was used to find and fix two problems: the
  closure-factory rule and an eval extraction bug when `go.sum` is absent.
  So the 20/20 is not an independent estimate. The 9/10 on the hard sample,
  drawn after those fixes, is the more honest number.
* All verdicts are from one rater.

**Audit of all 8 C3 pairs** (not sampled; every one was read):

* 6 are genuine in-memory state changes:
  * `throttler` field added and reordered (2 pairs)
  * global `reqid` uint64 -> `atomic.Uint64`
  * the `methodTyp` key type of the routing tree
  * live websocket `client` gains a channel
  * the live `config.Server` gains a field
* 1 is debatable: `MessageExternal.Priority int -> *int`. It is reachable
  through channels of live clients, but it is mainly a DTO.
* 1 is a false positive: the per-request `compressResponseWriter`, marked
  state because it has a map field.

The audit also found two rule bugs, which are now fixed:
* `interface{}` -> `any` was reported as a retype
* any tag change on a field with a json tag was treated as a json-key rename.

## 7. Threats to validity

* **Heuristics.** "State" is a reachability heuristic. Request-scoped
  structs with a map, mutex or chan field are over-approximated as state
  (compressResponseWriter). Long-lived objects held only in locals of
  `main` without such fields are missed. Long-running detection is
  syntactic (`for {}` that blocks, range over a channel, `select{}`,
  `ListenAndServe`/`Serve`/`Accept`). Inlinability uses a node-count
  threshold (<= 60 AST nodes, no closures, defer, go or select), not the
  gc inliner.
* **Start-up-only and closure rules are conservative for libraries.**
  chi's middleware factories are classified C5 because applications almost
  always call them while building the router. A library cannot show its
  callers, so this inflates C5 for chi. Without the closure rule, chi's
  in-process share would be 86.7%.
* **Opaque third-party code.** Fake packages make call resolution through
  third-party APIs incomplete. Interface dispatch is not followed. Reflection
  and `go:linkname` are ignored. A changed function reached only through
  reflection (e.g. gin binding) is still treated as patchable.
* **Go-only extraction in the evaluation.** Templates, static assets,
  manifests and Dockerfiles are not compared in the history run. Commits
  that touch only those files are not sampled. A template parsed once at
  start-up is effectively start-up state (the 03e209e disagreement). The
  CLI itself does compare non-Go files, but it classifies run-time-read
  assets as C0.
* **Assumptions about S6.** I assume S6 can redirect any non-generic
  top-level function or method, carry new functions, types and globals in
  the patch plugin, and patch package `main` after relocation. hotpatchgen
  is still being built. If it cannot handle package `main` or unexported
  access, the S6 share drops. The report lists these as risks per update.
* **Corpus.** Three projects, 150 commits each. One is a library and one
  service is dominated by bots, so the overall mix depends on that
  selection. I report per-project and bot-excluded numbers for that reason.
* **One rater**, and part of the validation set was used for tuning (see §6).

## 8. Future work

* Track start-up-built objects: data flow from `main` into router and
  handler registration, so the tool can say *which* routes or closures a
  start-up change affects. Use that to propose S1-style "re-run the wiring
  and swap the router" plans instead of S5.
* Compare non-Go assets, and model "read once at start-up" files (templates,
  config) as start-up state.
* Escape and lifetime analysis for a precise state set (types that escape
  into globals or long-lived objects) instead of field-kind roots.
* Complete transformer TODOs with an LLM, checked by round-trip property
  tests on the S5 JSON envelope (Gap G9 / Idea 7). Evaluate on the 8 real
  C3 pairs.
* Feed the classifier into the S6 patch builder (patch-set list -> slots)
  and into a Kubernetes controller that picks S6, S5 or a rolling update per
  release (Gap G5).
* A larger corpus (10+ services, all commits, a second rater with Cohen's
  kappa), and a comparison with what Java DSU systems support
  [`mlinaric2025javadsu`].
