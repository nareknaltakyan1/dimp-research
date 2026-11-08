# S6: Hotpatch-Ready Compilation

**Idea.** Make functions patchable *at build time*, the way GCC/Clang
`-fpatchable-function-entry` (used by Linux ftrace and livepatch) and MSVC
`/hotpatch` do, so that redirecting a function at run time is an ordinary
atomic pointer store. The process never writes to a code page. New code
arrives as a signed Go plugin. Its replacements are published as a
**transactional patch set**, and each request **pins** one patch-set epoch,
so a request never runs v1 of one function together with v2 of another.

| Component | Path |
|---|---|
| Source-to-source transformer (go/ast + go/types) | `tools/hotpatchgen/` |
| Run-time library: slots, refcounts, patch sets, quiescence | `internal/patchable/` |
| Signed plugin delivery (ed25519 manifest, loader) | `internal/patchable/delivery/` |
| Contract-compliant service, plugins, signing tool, build, smoke | `cmd/s6-hotpatch/` |

Addresses P1 (atomic switch-over), P2 (quiescence), P3 (new code), P6 (safety and rollback) and P8 (overhead). P4 (state) holds by construction because code is replaced and data is not.

---

## 1. Motivation: why not rewrite machine code at run time

An earlier S6 attempt overwrote the first instruction(s) of a compiled Go
function with a jump (the `bouk/monkey` / `gomonkey` approach, made safer).
That code is quarantined behind the `livepatch_raw` build tag in
`internal/livepatch` and is not pursued. The analysis below explains why
entry-point rewriting is brittle in Go specifically:

1. **Inlining makes "the function" ambiguous.** The Go compiler inlines small
   functions (budget 80 nodes) and, since 1.21, does PGO-driven inlining and
   devirtualisation. Patching the out-of-line copy of `F` does not change
   the copies inlined into its callers. Those callers keep running v1
   forever, silently. The only remedy is `//go:noinline` on everything,
   which is a build-time decision anyway.
2. **The runtime trusts metadata about the code bytes.** pclntab maps every
   PC to function, file and line, stack-map index (GC liveness of pointer
   slots), and unsafe-point and async-preemption information. Changing
   instructions without changing that metadata is sound only if no
   goroutine can ever be stopped, scanned or preempted at a modified PC.
   Go's asynchronous preemption (signals at nearly any instruction) makes
   that hard to guarantee. Stack copying (`copystack`) and the
   traceback/unwinder also walk frames by PC.
3. **Cross-modifying code rules.** On arm64 an instruction write needs
   D-cache clean, I-cache invalidate, and an `ISB` on *every* core that may
   execute it. Architecturally, only a few instructions (B, BL, NOP, ...)
   may be modified concurrently with execution. On amd64 a multi-byte
   patch must be a single aligned store, or the other cores must be
   serialised. Neither is expressible from Go without assembly and
   platform-specific code.
4. **No public stop-the-world.** Making a multi-instruction patch atomic
   needs all threads stopped at safe points. Go's `stopTheWorld` is
   internal and reachable only via `go:linkname` (needs
   `-ldflags=-checklinkname=0` since Go 1.23). Also, W^X policies
   (`mprotect` of text, SELinux `execmod`, hardened container runtimes)
   often forbid writable text.
5. **Toolchain coupling.** Prologue shapes (stack-bound checks), register ABI
   (ABIInternal since 1.17) and wrapper functions change between Go
   releases, so a patcher must recognise them byte by byte.

Production systems avoid all of this by **reserving the patch site at
compile time**. GCC/Clang `-fpatchable-function-entry=N` emits N NOPs that
ftrace later turns into a call with a single architecturally allowed
instruction write. MSVC `/hotpatch` guarantees a 2-byte-patchable first
instruction plus padding. S6 takes the idea one step further: the reserved
site is **a data word, not an instruction**, so a patch never modifies
code at all.

## 2. Design

```
 build time                                     run time
 ----------                                     --------
 biz/biz.go (plain Go)                          plugins/v2.so  + v2.manifest.json (ed25519)
   //dimp:patchable                                   |
   func Validate(ctx, req, tr) error {...}            v
        |                                       delivery.Loader
        | hotpatchgen (go/ast + go/types)         1 verify signature (trusted key ids)
        v                                         2 fingerprints vs registry   <- before dlopen
 gen/biz/biz.go                                   3 private copy + sha256 == manifest
   func Validate(ctx, req, tr) error {            4 plugin.Open, Patches map, runtime fingerprints
     hp := _dimpSlot_Validate.Enter(ctx)               |
     defer hp.Exit()                                   v
     return hp.Impl().(func(...) error)(...)      Registry.Apply("v2", {sym: fn, ...})
   }                                                build PatchSet{epoch+1} (copy-on-write)
   func Validate_v0(ctx, req, tr) error {...}      cur.Store(newSet)   <- ONE atomic store
 gen/biz/zz_dimp_registry.go                           |
   _dimpSlot_Validate = Default.MustRegister(          v
     {Symbol:"biz.Validate", Fingerprint:"219e..."})  Txn.Wait: pins on old sets == 0
   init(){ _dimpSlot_Validate.MustBind(Validate_v0) }  && replaced versions' refcounts == 0
                                                       && runtime.Stack scan finds no old frame
```

### 2.1 Data model (`internal/patchable`)

```
Registry ──cur (atomic.Pointer)──▶ PatchSet{Epoch 3, Label "v2", pins}
                                     versions[ slot0 ─▶ Version{fn: Handle_v0,  active} (shared)
                                               slot1 ─▶ Version{fn: validateV2, active} (new)
                                               slot2 ─▶ Version{fn: Price_v0,   active} (shared)
                                               slot3 ─▶ Version{fn: discountV2, active} (new)
                                               slot4 ─▶ Version{fn: formatV2,   active} (new) ]
          live sets: PatchSet{Epoch 2, "v1", pins=3}  (still pinned by 3 long requests)
```

* **Slot**: one per patchable function. It holds the stable symbol, the
  signature fingerprint, the exact `reflect.Type` of the implementation,
  and version 0.
* **Version**: one implementation of one slot plus an in-flight counter
  (**per function, per version**).
* **PatchSet**: an immutable vector `slot -> Version`, with an epoch, a
  label and a pin counter. Unchanged slots share their `*Version` with the
  previous set (copy-on-write).
* The registry's only mutable hot-path state is **one atomic pointer** to
  the current set.

### 2.2 Trampolines and accounting

Functions **with** a `context.Context` parameter use `Slot.Enter(ctx)`:

* If `ctx` carries a pinned set (from `Registry.Pin` at request entry, or
  from an outer trampoline), take `set.versions[slot]` and increment that
  version's counter. No recheck is needed because the pinned set cannot be
  drained under us.
* Otherwise, pin the *current* set with **increment-then-recheck** (`load
  cur; pins++; if cur changed {pins--; retry}`), put it into the context
  that is passed to the callee, and release it on exit. Nested calls then
  inherit the epoch automatically.

Functions **without** a context use `Slot.Acquire()`: increment-then-recheck
directly on the *version's* counter (`v = cur.versions[slot]; v.active++;
if cur.versions[slot] != v {v.active--; retry}`). This gives per-call
consistency, and hotpatchgen warns about these functions.

Increment-then-recheck closes the PoC race (load v1, the drainer sees 0,
then the request increments and runs v1). A caller that increments after
the new set is published sees the recheck fail and backs out, so it never
executes old code. A caller that incremented before is counted. The
stress test checks exactly this: after `Wait` returns, the old
generation's functions record a violation if they ever start. Over 1.9 M
requests and 6,684 transactions under `-race`, there were 0 violations.

**Striped counters (finding).** One atomic counter per version is correct
but is a cache-line hotspot. With 12 goroutines on 12 cores, the naive
trampoline costs **~120 ns/call versus ~4 ns single-threaded**. S6 stripes
each counter over 32 cache-line-padded cells. The stripe is chosen from the
address of a stack variable, because Go has no cheap P or goroutine id and
goroutines have distinct stacks. The stripe index is carried in the
call token, because a stack can move between increment and decrement. Each
stripe stays >= 0, so `sum()==0` after publication still implies
quiescence. Result: **1.1 to 5.5 ns/call** in parallel. This is
bimodal because of stripe collisions; one run of the pinned variant hit
27 ns.

### 2.3 Transactional patch sets (the novel element)

`Registry.Apply(label, {sym: fn, ...})`:

1. validates *all* entries (symbol exists, fingerprint equal, exact
   `reflect.Type` identity or a convertible identical type). On any error
   nothing is published (`TestApplyAllOrNothing`).
2. builds the new set under the writer lock.
3. publishes it with **one `atomic.Pointer.Store`**, which is the
   linearisation point of the whole group.

**Consistency argument.** Each request pins exactly one set (one atomic
load in `pin`). Sets are immutable. Every context-aware trampoline
resolves its implementation from the context's pinned set. So all
context-aware calls of a request use versions from one set: either all
of v1's or all of v2's. A drain completes only when no non-current set
is pinned and no replaced version is executing. Therefore no request
can start on the old set after the drain, and the old code really is
idle. This mirrors the **Linux livepatch consistency model**, where each
*task* switches from old to new universe only at a safe point (its
transition is per task, and a task never runs a mix of patched and
unpatched functions within one patch). In S6 the *request* is the task, and
the "safe point" is request entry.

Revert is a new transaction (a new epoch with the previous versions
restored), so it has the same guarantees. `Revert` requires the
transaction to be the most recent one (stack discipline), otherwise
`ErrNotTop`.

### 2.4 Quiescence: refcount + stack scan (defence in depth)

`Txn.Wait` declares the replaced code idle when:

* **Refcounts are clear.** No non-current set is pinned and every replaced
  version's counter sums to 0. This is sufficient for every activation
  that *entered through a trampoline*, because each such activation holds
  a count for its entire duration (the decrement is deferred, so it also
  runs on panic).
* **Stack scan is clear (optional, on by default).** `runtime.Stack(all)`
  contains no frame of a replaced implementation symbol, including
  closures and go-statement wrappers (`F_v0.func1`, `F_v0.gowrap1`).
  "created by" lines are deliberately ignored.

The refcount cannot see code that runs **outside** a trampoline
activation:

* **goroutines spawned by old code**: `go func(){...}()` inside `F_v0`
  keeps running `F_v0.func1` after `F_v0` returned and its count went to 0.
  (`TestStackScanCatchesWhatRefcountMisses`, `TestStackScanOnGeneratedSpawn`.)
* **captured implementation pointers**: code that called `hp.Impl()` and
  kept the func value, then calls it later without a count.

Both tests show a refcount-only drain wrongly succeeding and the stack scan
holding the drain until the old frames are gone.

Limitations of the scan:

* `runtime.Stack(all)` stops the world while formatting: 75 µs with 10
  goroutines, 3.1 to 3.8 ms with 1,000, and **52 to 55 ms with 10,000**.
  That is why it runs once, only after refcounts are clear.
* The runtime elides the middle of stacks deeper than 100 frames.
* Matching is by symbol name, so two versions that share code (closures
  from one literal) are indistinguishable. Such names are skipped and left
  to the refcounts.

## 3. Transformation rules (`tools/hotpatchgen`)

`hotpatchgen gen -dir biz -out gen/biz [-all]` (the input is never modified,
and `-out` must differ from `-dir`). Selection is by `//dimp:patchable` in
the doc comment, or `-all` for every eligible top-level function and
method.

| Construct | Rule |
|---|---|
| Function `F` | `F` becomes the trampoline (same signature and doc). The body moves verbatim to `F_v0` (textual splice, so comments, closures, `defer`/`recover` and labels are untouched) |
| Method `(*T).M` / `T.M` | `T_M_v0(recv *T, params...)`: the receiver becomes the first parameter. Symbol `pkg.(*T).M` / `pkg.T.M`. Unnamed or blank params become `_` so they can be mixed with the named receiver |
| Variadic | The trampoline forwards `xs...`, and the impl type keeps `...T` (a `[]T` patch is rejected) |
| Multiple / named results | Result names are kept in the trampoline when they don't shadow anything; naked returns stay in `F_v0` |
| Param names that shadow types (`func F(Counter Counter, x p0)`) | Fresh names (`p0_1`, `p1`) avoid both shadowing and collision with the spelled-out func type |
| `context.Context` param | `Enter(ctx)` and the callee receives the pinned `hp.Ctx()`; otherwise `Acquire()` plus a warning |
| Closures inside bodies | Unchanged (they live in `F_v0`) |
| Recursion | Recursive calls go through the trampoline, so a patch also affects recursive calls; `F_v0` can be delegated to |
| Generics (type params, generic receivers) | **Skipped with a warning.** A slot needs one concrete func type, and a generic function has one shape per GC-shape instantiation. Instantiation-specific slots are future work |
| `//go:` directives, no body, `init`, `main`, files with `//go:build` | Skipped (directive semantics would not carry over; the registry must not reference platform-specific code) |

**Registry file.** Each slot is registered in a package-level variable
*without* the implementation and bound in `init()`. Registering
`Impl: F_v0` in the initialiser caused an **initialisation cycle for every
recursive function** (slot -> F_v0 -> F -> slot). The compiled fixture found
this. The trampoline uses a comma-ok assertion and falls back to calling
`F_v0` directly when the slot is still unbound, which only happens during
package-variable initialisation.

**Fingerprints.** The fingerprint is the first 16 hex digits of
SHA-256 over a canonical signature string. That string is produced
independently from `go/types` (build time) and from `reflect` (run time).
`MustBind` re-derives it at init and panics on disagreement, so every
compiled generated package doubles as a cross-check of the two
canonicalisers. Named types render as `pkgname.Name`, because the
generated copy lives at a different import path than its source. The exact
`reflect.Type` check in `Apply` covers name collisions.

`hotpatchgen fingerprints -dir plugins/v2` statically fingerprints a
plugin's `Patches` map for the manifest. `hotpatchgen check` fails CI if
`gen/` is stale, and `build.sh` runs it.

**Tests.**
* The golden fixture `internal/fixture/src -> internal/fixture/gen` is
  checked in, *compiled*, and exercised by `gen/fixture_test.go`.
* The golden edge case `testdata/edge` (marker mode, generics, directives,
  constrained file, warnings) is also type-checked in memory.
* A plugin fingerprint test checks the `fingerprints` command.

## 4. Patch delivery and safety

A patch is `name.so` plus `name.manifest.json`: `{format, label, plugin,
sha256, symbols{sym: {fingerprint, signature}}, key_id, sig}`, signed with
ed25519 over the canonical JSON (`signtool`). The loader **fails closed**
in this order:

| Step | What | Plugin case rejected (smoke) | When |
|---|---|---|---|
| 1 | trusted key id, ed25519 signature | `badsig` (v2 signed by an attacker key) | 0.3 to 0.7 ms |
| 2 | manifest fingerprints vs running binary, **before dlopen** | `badfp` (`int` instead of `int64`) | 0.5 to 0.7 ms |
| 3 | copy into a private 0700 dir while hashing, compare sha256, open the *copy* (closes the check-then-open race) | `tampered` (1 byte appended after signing) | ~11 ms |
| 4 | `plugin.Open`, `Patches` set == manifest set, run-time fingerprints | `badfp-liar` (manifest claims the right fingerprint) | ~9 to 14 ms |
| 5 | `Registry.Apply` (types, all-or-nothing) | n/a | n/a |

Step 2 matters because **`plugin.Open` is irreversible**: it runs the
plugin's `init` and the plugin can never be unloaded. In the liar case the
plugin is mapped but nothing is applied, and v1 keeps serving. Other
findings:

* The Go runtime refuses to open the same plugin package twice ("plugin
  already loaded"). Upgrade -> rollback -> upgrade therefore reuses the
  already-open plugin, cached by sha256 (`cached_plugin: true`, `open_ns: 0`).
* Host and plugin must share byte-identical builds of `gen/biz` and
  `internal/patchable` (same toolchain and flags), so both are built
  together in Docker by `build.sh`.

## 5. Results (Docker `golang:1.25`, linux/arm64, 12 vCPU, Apple M4 Pro host)

### 5.1 Micro-benchmarks (`go test -bench`, 3 runs each, ns/op)

| Call style (trivial non-inlined callee) | ns/call | allocs |
|---|---|---|
| direct call | 0.77 to 0.90 | 0 |
| func value | 0.79 to 0.83 | 0 |
| `atomic.Pointer[func]` load + call | 1.00 to 1.04 | 0 |
| trampoline, no ctx (`Acquire`, striped) | 4.94 | 0 |
| trampoline, naive shared counter | 3.87 to 4.01 | 0 |
| trampoline, ctx already pinned | 13.8 to 13.9 | 0 |
| trampoline, ctx not pinned (pins + `WithValue`) | 31 to 32 | 1 (48 B) |
| request pin + unpin | 21 to 24 | 1 (48 B) |
| **parallel (12 goroutines)**: atomic pointer | 0.16 to 0.17 | 0 |
| parallel: naive shared-counter trampoline | **117 to 123** | 0 |
| parallel: striped trampoline, no ctx | **1.1 to 5.5** | 0 |
| parallel: striped trampoline, pinned ctx | 3.6 (one run 27) | 0 |

Real generated code (`fixture/gen`):
* `Divide` costs 1.15 ns direct versus 5.27 ns through the trampoline.
* Variadic `Sum` costs 0.99 ns direct versus 20.3 ns pinned (16 B, 1
  alloc) and 39 to 41 ns unpinned (64 B, 2 allocs).

| Transaction of N patches (200-slot registry) | apply (validate + build + publish) | publish (the atomic store) |
|---|---|---|
| N=1 | 2.4 to 2.5 µs | 29 to 37 ns |
| N=10 | 10.7 to 11.8 µs | 32 to 49 ns |
| N=100 | 109 to 111 µs | 38 to 95 ns |

Apply cost is linear in N (reflect fingerprint, SHA-256 and a 2 KiB striped
counter per new version). **Visibility is one store, independent of N.**
An idle drain costs 3.8 to 4.3 µs without the stack scan and 94 to 114 µs
with it.

### 5.2 Inlining and escape analysis (`-gcflags=-m=2`)

* Every trampoline reports `cannot inline Validate: unhandled op DEFER`.
  Original bodies that were inlinable (e.g. `Divide_v0`, `Sum_v0`) are no
  longer inlined into callers, because callers now call the trampoline,
  and the trampoline calls the body through an interface-held func value,
  which the compiler can never inline.
* **All pointer-carrying trampoline parameters "leak"**, because the
  callee is unknown. Arguments that used to live on the caller's stack can
  move to the heap. Example: `Sum`'s variadic slice now costs 16 B and 1
  alloc per call. This **escape-analysis tax** is an under-reported cost of
  any function-pointer-based DSU in Go (S1 to S3 pay it too, at a coarser
  granularity).
* The practical guidance is to mark patchable functions at coarse
  granularity (business operations, not tiny helpers). `-all` is for
  experiments only.

### 5.3 End-to-end smoke (`cmd/s6-hotpatch/smoke.sh`)

Setup: 16 clients, 8 s; every 10th request has `work_ms=300` (it
straddles the switch), and the others have 0 to 19 ms. The script runs:
upgrade v2 -> rollback -> badsig -> tampered -> badfp -> badfp-liar ->
upgrade v2 -> rollback -> upgrade v2 (re-upgrade uses the cached plugin),
then SIGHUP (rollback).

| | `-consistency=request` (design) | `-consistency=call` (ablation) |
|---|---|---|
| requests / non-200 / transport errors | 3,200 / **0** / 0 | 3,198 / 0 / 0 |
| count max / unique / duplicates / gaps | 3,200 / 3,200 / **0 / 0** | 3,198 / 3,198 / 0 / 0 |
| version flips in entry order (epochs seen) | 5 (0,1,2,3,4,5) | 5 |
| **mixed patch-set requests** | **0** | **78** (e.g. `label=v1 ... discount@v2,format@v2`) |
| wrong totals (bug fix applied to a v1-labelled request) | 0 | 78 |
| failed upgrades (all 4 bad patches) | `upgrade_failed`, v1 kept serving | same |

* Ordered by completion time, versions interleave (95 to 103 runs). That
  is expected: long requests pinned to the old set finish after the switch.
* v1 totals are 5997 (the bug: no discount). v2 totals are 5398 (the fix).
* The ablation proves the test is sensitive: without pinning, a request
  that validated under v1 and priced after the switch mixes versions.

Upgrade timeline from the event log (request mode):

| Phase | First upgrade | Re-upgrade (cached plugin) | Rollback |
|---|---|---|---|
| `upgrade_start` -> `new_version_serving` | 16.7 ms | 10.3 to 10.8 ms | 0.08 to 0.09 ms |
| of which signature check | 1.3 ms | 0.18 to 0.27 ms | n/a |
| of which copy + sha256 of the 5.3 MB `.so` | 10.6 ms | 9.8 to 10.1 ms | n/a |
| of which `plugin.Open` | 4.0 ms | 0 | n/a |
| `Apply` / atomic publish | 18 to 31 µs / 42 to 125 ns | | 5 to 13 µs / 83 to 166 ns |
| drain (old code quiescent) | 125 to 279 ms (bounded by the 300 ms requests) | | 0.2 to 303 ms |
| stack scan during drain | 0.85 to 1.4 ms, 0 hits | | |

Copy-and-hash dominates time-to-serve. Mapping the verified file
`O_RDONLY` and handing an fd path (`/proc/self/fd/N`) to `dlopen` would
remove the copy.

All S6 packages pass `go vet` and `go test -race` in Docker. That covers
the runtime library, delivery, hotpatchgen goldens, the generated fixture
and the service.

## 6. Comparison

| | S6 | `-fpatchable-function-entry` + ftrace / livepatch | MSVC `/hotpatch` | Erlang/OTP code loading | Raw entry rewriting (quarantined S6 attempt, monkey) |
|---|---|---|---|---|---|
| Patch site reserved at | build (source transform) | build (NOP pad) | build (2-byte first instr. + padding) | VM design | nowhere |
| Runtime patch | data store (atomic pointer) | one instruction write (+ text_poke, IPIs) | instruction write | module table swap | multi-byte code write |
| Unit of consistency | **request** (pinned epoch) | **task** (per-task transition, livepatch) | none (per call) | **process**: old code until the next fully qualified call; 2 versions max | none |
| Group atomicity | yes (patch set) | yes (one livepatch object) | no | per module | no |
| Old code retirement | refcount + stack scan | per-task stack check (reliable stacktrace) | n/a | `soft_purge` / kill processes in old code | unsafe |
| New code delivery | signed Go plugin | signed kernel module | DLL | `.beam` | in-process bytes |
| Inlining | lost at trampolines | callers inlining the patched function are not patched; kpatch-build diffs them in | similar | none (VM) | lost or broken silently |

S6's request-level pinning is the Linux livepatch per-task transition
model, transplanted to the request as the unit of work. Erlang's
"current and old" pair corresponds to S6's live patch sets, but S6 allows
any number of live sets and retires them by measurement rather than by
killing processes.

## 7. Limitations

* **Data layouts cannot change.** A patch can only replace function bodies
  with the same signature. Changing a struct, a method set or a signature
  needs a state transformer and a process-level handoff (S5) or a
  rolling update. S8's selector should route such changes away from S6.
* **Patches can use only the package's exported API.** Unexported fields
  and helpers are invisible to plugin code. Patches can delegate to the
  exported `F_v0` originals.
* **Context-less functions get per-call consistency only.** hotpatchgen warns
  about them.
* **Generics are skipped.** So are directive-carrying, assembly and
  build-tagged functions.
* **Overhead.** About 4 to 14 ns per trampoline call, no inlining, and
  argument escapes. This makes fine-grained `-all` unattractive.
* **Plugins are never unloaded** (P8). Each distinct patch leaks its `.so`
  (5.3 MB here; see S2's memory-growth study). There is also no pure-Go
  linux/arm64 vs. darwin story: plugins need cgo and Linux in practice.
* **Stack scans stop the world.** About 52 ms at 10k goroutines. The scan
  can be disabled with `-stackscan=false`, which leaves refcount-only
  quiescence.
* **Stack-traceable behaviour shifts.** `runtime.Caller` depths and panic
  traces gain one frame (the trampoline), and `F_v0` names appear in traces.

## 8. Future work

* Generic slots: one slot per GC-shape instantiation, emitted from
  `go/types` instance information.
* An `-fpatchable`-style compiler pass (cmd/compile SSA) instead of a source
  transform. It would keep the trampoline inlinable into callers (only the
  indirect call is the patch point) and avoid the escape tax by marking
  the parameters `noescape` when all versions are known not to retain
  them, checked at Apply.
* Lazy migration of goroutine-local state, and state transformers for
  layout changes (combine with S5).
* Per-P counters through a runtime hook, if Go ever exposes one, to
  remove stripe collisions.
* `dlopen` of the verified fd instead of a copy. Kubernetes integration
  (S7): the patch as a CRD carrying a manifest digest, with in-process
  canary between two live patch sets.

## 9. How to run

```bash
# build host + plugins + keys + signed manifests (Linux, matching toolchains)
docker run --rm -v "$PWD":/src -w /src golang:1.25 cmd/s6-hotpatch/build.sh
# end-to-end smoke (design + ablation)
docker run --rm -v "$PWD":/src -w /src golang:1.25 cmd/s6-hotpatch/smoke.sh
# tests / benchmarks
docker run --rm -v "$PWD":/src -w /src golang:1.25 go test -race ./internal/patchable/... ./tools/hotpatchgen/... ./cmd/s6-hotpatch/...
docker run --rm -v "$PWD":/src -w /src golang:1.25 go test -run x -bench . -benchmem ./internal/patchable/ ./tools/hotpatchgen/internal/fixture/gen/
# regenerate the patchable package after editing cmd/s6-hotpatch/biz
go generate ./cmd/s6-hotpatch
```
