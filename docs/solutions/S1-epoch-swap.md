# S1 - Epoch-Based In-Process Hot-Swap (`cmd/s1-epoch-swap`, `internal/hotswap`)

## 1. Problem addressed

The original PoC (Ruzanna20/dimp-poc) keeps the active handler in an
`atomic.Pointer`, swaps v1 -> v2 after a fixed 5 s sleep, and then waits until
a **single process-wide in-flight counter** reaches zero. That design has
three defects that S1 fixes. S2 and S3 reuse S1's mechanism.

| Defect in PoC | Consequence | S1 fix |
|---|---|---|
| Global in-flight counter counts requests on **both** versions | Under steady overlapping load the counter never reaches 0, so the wait never ends. A value of 0 also says nothing about *which* version is idle | Reference count **per published version** (RCU/epoch style). The old version drains once *its own* count is 0 |
| Sleep-based timing, errors ignored | Not reproducible, and failures pass silently | Event-driven drain (`Drained()` channel), explicit errors, `upgrade_failed` event |
| No state migration, no rollback | Counter resets or diverges | `ExportState -> ImportState` hand-over before publication; `Probe` (before publish), `Verify` with automatic rollback, and explicit `Rollback()` |

S1 still compiles v1 and v2 into the same binary. S1 shows that the
*swap protocol* is correct. S2 and S3 add dynamic code loading.

## 2. Design

```
              Acquire()                          Swap(v2)
 request ──► v := cur.Load()             ┌──────────────────────────────┐
             v.refs++                    │ 1 st := cur.Impl.ExportState()│
             if cur.Load()!=v: undo,retry│ 2 v2.ImportState(st)          │
             ...serve on v.Impl...       │ 3 Probe(v2)        (optional) │
             v.Release(): refs--         │ 4 old := cur.Swap(v2)  ◄── linearisation point
               if refs==0 && retired:    │ 5 old.retired = true          │
                  close(old.drained)     │ 6 if old.refs==0: close(drained)
                                         │ 7 Verify(v2) -> rollback on err│
                                         └──────────────────────────────┘
   cur ──► [Version v2 | refs=37 | live ]
           [Version v1 | refs=3  | retired ] ──(refs→0)──► Drained ──► Unload hook (S3)
```

```mermaid
sequenceDiagram
    participant R as Request (old)
    participant N as Request (new)
    participant S as Swapper
    participant Reg as Registry
    R->>Reg: Acquire() -> v1 (v1.refs=1)
    S->>Reg: ExportState(v1) / ImportState(v2)
    S->>Reg: cur.Swap(v2); v1.retired=true
    N->>Reg: Acquire() -> v2 (v2.refs=1)
    Note over S: waits on v1.Drained(); v2 traffic is irrelevant
    R->>Reg: v1.Release() (refs=0, retired) -> close(v1.drained)
    S-->>S: old_version_drained, upgrade_done
```

Components:

* `internal/hotswap/registry.go` holds the generic `Registry[T]`, `Version[T]`,
  `Swap`, `Rollback`, `Upgrade.Wait(ctx, timeout)` and `InFlight()` (per-version
  in-flight counts for `/admin/status`).
* `internal/hotswap/global.go` holds `GlobalCounterSwapper`, a faithful copy
  of the PoC's design. It is kept only for the contrasting test and the
  benchmarks.
* `internal/hotswap/svc` maps the shared contract (`/api`, `/healthz`,
  `/admin/status`, `POST /admin/upgrade`, SIGHUP, and the JSON event log on
  stderr) onto a registry. A solution supplies only `Load(name)` and an
  optional `Unload` hook.
* `cmd/s1-epoch-swap` holds v1 and v2 (`versions.go`). v2 differs in its
  `version` string and adds an `X-Patched` header.

`/api` pins the version **before** the simulated `work_ms` sleep, so a long
request keeps the old version alive for its whole duration. That is the
condition under which draining matters.

## 3. Correctness argument

The Go memory model (since Go 1.19) makes every `sync/atomic` operation
sequentially consistent: all atomic operations take part in a single total
order that agrees with each goroutine's program order.

**Safety (no request runs on a drained version).** Take an acquirer A that
returns `v`. A executed `refs.Add(1)` and then a validating `cur.Load()` that
returned `v`. The swapper S executed `cur.Swap(new)`, then `retired.Store(true)`,
then `refs.Load()`. If A's validating load returned `v`, it precedes
`cur.Swap` in the total order. Therefore A's increment also precedes S's
`refs.Load()`, so S sees `refs >= 1` and does not signal. From then on, only
a `Release` that brings `refs` to 0 while `retired` is true can signal, and
the last such `Release` happens after every pinned request has finished. If
A's validating load returned a different pointer, A undoes its increment and
never uses `v`. At worst, that undo is the operation that raises the drained
signal, which is still correct because `v` is retired and unused. Signalling
is idempotent (`sync.Once`).

**Liveness (draining terminates under continuous load).** After step 4 no
new `Acquire` can return the old version, because the validation fails.
The old refcount therefore only decreases. It reaches 0 no later than the
end of the longest request that was in flight at publish time. Traffic on the
new version does not affect it. The global-counter design has no such bound.

**State.** `ImportState` runs *before* publication, so the new version never
serves a request without state. The hook semantics are documented in
`StateExporter`. Because old and new versions overlap during draining, the
exported state must be either (a) a **shared handle** (ownership transfer,
used here: `*atomic.Uint64`) or (b) a snapshot that the old version no longer
mutates, which requires a quiescence phase. S1-S3 use (a), so the counter
is strictly continuing (verified: sorted counts equal 1..N with no gaps or
duplicates).

**Verification.** These properties are checked by tests under `-race`:
`TestNoUseAfterDrain` (16 goroutines, 500 swaps, asserts no request
observes a closed `Drained` channel),
`TestPerVersionDrainUnderContinuousLoad` and its counterpart
`TestGlobalCounterNeverDrainsUnderContinuousLoad`. In the load model, each
worker acquires request *k+1* before it releases request *k*, so the global
in-flight count is never 0. Further tests cover the error and rollback paths,
drain timeout, and per-version accounting. `cmd/s1-epoch-swap` has an
end-to-end HTTP test (16 clients with `work_ms=20`, upgrade via HTTP, zero
non-200 responses, contiguous counts, ordered events).

## 4. Rollback and failure handling

* An export, import or probe failure returns an error before publication and
  leaves the registry unchanged. `svc` emits `upgrade_failed` and the old
  version keeps serving.
* A `Verify` failure after publication triggers an automatic re-publication
  of the old implementation (state is handed back through the same hooks),
  and returns `ErrVerify`. The registry is then back where it was before the
  `Swap`, **including the rollback target**. (Fixed after review: the
  rollback target used to be the failed version, so a later `Rollback()`
  republished it.) The returned `Upgrade.New` is the failed version. It may
  still have in-flight requests, so its implementation may be unloaded only
  after `New.Drained()`; `svc` does exactly that.
* On a drain timeout, `Wait` returns `ErrDrainTimeout`. The old version is
  **never cut off**: it keeps draining in the background, and `svc` emits
  `old_version_drained` and runs `Unload` when the drain completes. `svc`
  reports the timeout as the `drain_timeout` event (new version serving,
  old not yet drained), followed by `upgrade_done` with `"drained":false`.
  It used to emit `upgrade_failed`, which the contract reserves for
  upgrades that left or put the old version in service, so the analysis
  could misclassify such runs (e.g. in E3).
* `Rollback()` re-publishes the previous implementation explicitly. Because
  a retired `Version`'s drained signal cannot be revoked, a rollback (and
  the automatic one after `Verify`) publishes the *same implementation* in
  a fresh `Version`; the two share a lineage.
* **Unloading goes through the registry** (`Registry.Unload(v, fn)`). It runs
  `fn` only if `v` has drained and its implementation is neither current
  again nor still draining in another `Version`, and it marks the lineage
  unloaded. `Rollback()` refuses an unloaded implementation with
  `ErrUnloaded`; the caller must load it again and `Swap`. (Fixed after
  review: before, `svc` closed an S3 module directly after the drain while
  the registry still listed it as the rollback target, so `Rollback()`
  could republish a closed module, and an automatic `Verify` rollback could
  make a module current that was about to be closed.) `svc` logs a refused
  unload as `unload_skipped`. Tests: `TestVerifyFailureRestoresPrev`,
  `TestRollbackRefusesUnloadedImpl`, `TestUnloadRefusesLiveImpl`,
  `internal/hotswap/svc/svc_test.go`.
* The S1 to S3 admin endpoint `/admin/upgrade` is unauthenticated (contract
  section 2). Only names of compiled-in versions, or files in the
  configured plugin or module directory, can be loaded. Do not expose the
  port beyond a trusted network.

## 5. How to run

```bash
go build -o /tmp/s1 ./cmd/s1-epoch-swap && /tmp/s1 -addr :8080
curl -s localhost:8080/api?work_ms=100
curl -s -XPOST 'localhost:8080/admin/upgrade?to=v2'   # or: kill -HUP <pid>
curl -s localhost:8080/admin/status
cmd/s1-epoch-swap/smoke.sh /tmp/s1 18081              # end-to-end smoke test (any S1-S3 binary)
go test -race ./internal/hotswap/... ./cmd/s1-epoch-swap/
go test -run '^$' -bench . -benchmem ./internal/hotswap/
```

## 6. Measurements

Hardware: Apple M4 Pro (12 cores). Results are shown for Linux arm64 inside
Docker (`golang:1.25` image, go1.25.14) and for macOS 27 natively (go1.24.3).
Each value is a median of 5 runs, in ns per request, of an indirect call to an
empty non-inlined function plus the bookkeeping. "Parallel" uses
`b.RunParallel` with 12 goroutines.

| Mechanism | Linux serial | Linux parallel | macOS serial | macOS parallel |
|---|---:|---:|---:|---:|
| Plain call | 0.76 | 0.13 | 0.77 | 0.14 |
| `atomic.Pointer` only (PoC without counting) | 0.76 | 0.16 | 0.77 | 0.17 |
| Global counter (PoC) | 3.54 | 114 | 3.64 | 88 |
| **Per-version Acquire/Release (S1)** | **3.60** | **118** | **3.65** | **99** |
| S1 with a concurrent swapper (continuous swaps) | - | 121 | - | 106 |

Interpretation: S1 costs the same as the PoC's global counter. Both are two
atomic RMWs on one cache line, plus one extra plain load for validation in S1.
Uncontended this is about 3.6 ns. With 12 cores hitting the same counter,
cache-line ping-pong pushes it to about 100-120 ns. Against a real HTTP request
(tens of microseconds) this is below 1 %. A version change in progress (the
retry path) adds little. **Correctness, not cost, is what separates S1 from
the PoC.**

End-to-end checks (`go test -race`, Linux/Docker and macOS, plus the
`smoke.sh` run):

* Linux smoke run: 204/204 `work_ms=50` requests returned 200. Versions
  seen: 64 v1, 140 v2. Count duplicates: 0. Highest v1 count 128, lowest v2
  count 133, so the counter continued across the flip. Events: `drain_ns`
  13.0 ms, `upgrade_done` `total_ns` 13.1 ms. A SIGHUP back to v1 also worked.
* In the deterministic check (one 3 s request in flight, then an upgrade),
  `old_version_drained` arrived after 2.68 s, while a request issued after the
  upgrade was served immediately by v2.

## 7. Limitations

* Both versions are compiled in. This is not dynamic code loading; see S2
  and S3.
* A single contended refcount per version. A sharded or per-CPU counter
  (summed only after retirement) would remove the roughly 100 ns contention
  cost at high core counts. This is left as future work.
* State hand-over by shared handle assumes both versions agree on the state
  representation. A schema change needs a converter in `ImportState`, and a
  snapshot migration needs a quiescence phase, which is not implemented.
* Requests that never finish (for example, hijacked or streaming
  connections) block draining indefinitely. The timeout reports this but
  cannot fix it.

## 8. Threats to validity

* The micro-benchmarks isolate bookkeeping. Real handlers dilute the
  overhead, and contention depends on core count and topology. The Docker
  numbers are from a Linux VM on Apple silicon, not bare-metal x86.
* The smoke test uses `curl` processes as clients, so client duty cycle and
  phase alignment affect the observed `drain_ns`. Use the benchmark
  harness, not `smoke.sh`, for drain-time distributions.
* The "never drains" result for the global counter relies on a load model
  with overlapping requests. Under bursty load with idle gaps the PoC
  eventually drains, though by accident rather than by design.
