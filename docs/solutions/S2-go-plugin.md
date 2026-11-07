# S2 - Dynamic Code Loading with Go Plugins (`cmd/s2-plugin`)

## 1. Problem addressed

S1 fixes the swap protocol, but both versions are still compiled into the
binary. S2 loads the new business logic **at runtime** from a separately
built shared object (`-buildmode=plugin`). The upgraded code therefore did not
exist when the server started. The plugin is published through the same
`internal/hotswap` registry, so S1's draining and state guarantees carry over
unchanged.

## 2. Design

```
 build time                               run time (single process, pid unchanged)
 ──────────                               ───────────────────────────────────────
 plugins/v1/main.go ─go build -buildmode=plugin─► v1.so ─┐  plugin.Open at startup
 plugins/v2/main.go ─go build -buildmode=plugin─► v2.so ─┼─► POST /admin/upgrade?to=v2
 cmd/s2-plugin (host) ─go build─► s2-plugin              │      plugin.Open(v2.so)  (~3-30 ms Linux)
                                                          │      Lookup("New"), Lookup("Version")
                                                          │      impl := New()  (fresh instance)
                                                          │      hotswap.Swap: v1.ExportState() ─► v2.ImportState()
                                                          │      publish; wait v1 drained
                                                          └── v1.so stays mapped forever (no dlclose)
```

```mermaid
flowchart LR
  A[POST /admin/upgrade?to=v2] --> B[plugin.Open v2.so]
  B --> C[Lookup New / Version]
  C --> D[New() -> handler]
  D --> E[ExportState v1 -> ImportState v2]
  E --> F[cur.Swap -> v2 serving]
  F --> G[wait v1 refcount = 0]
  G --> H[old_version_drained, upgrade_done]
  H -.-> I[(v1.so code + data remain mapped)]
```

**Host/plugin ABI.** The plugin depends **only on the standard library**.
It exports `func New() any` and `var Version string`. The host does not import
any plugin type. It uses Go's dynamic interface checks on the returned value:
`http.Handler` (`ServeHTTP`), plus the optional
`ExportState() (any, error)` / `ImportState(any) error`. Every type in these
signatures belongs to `net/http` or `sync/atomic`, which the host and the
plugin share, so runtime type identity holds across the boundary. This choice
is deliberate. Any non-std package shared between the host and a plugin, or
between two plugins, must be the *identical* build. Otherwise `plugin.Open`
fails with "plugin was built with a different version of package X".

**State.** The host creates a `*atomic.Uint64` and hands it to the initial
plugin through `ImportState`. Each upgrade moves the handle with
`ExportState -> ImportState` before publication, so the counter continues
across plugins even though the plugins have no shared package.

Files: `internal/hotswap/pluginimpl` (loader, with a `!cgo` stub),
`cmd/s2-plugin/main.go` (host), `cmd/s2-plugin/plugins/{v1,v2}` (plugins),
`cmd/s2-plugin/build.sh`, and `cmd/s2-plugin/memgrowth` (experiment).

## 3. Correctness argument

Draining and safety are inherited from `internal/hotswap` (see S1 section 3).
Plugins add the following:

* `plugin.Open` and the `New()` call happen **before** `Swap`, on the
  upgrade goroutine, so a slow or failing load never affects requests (v1
  keeps serving). A failed load emits `upgrade_failed` and leaves the
  registry unchanged.
* `plugin.Open` runs the plugin's package `init`s exactly once per
  process. Re-opening an already loaded path returns the cached plugin, which
  makes a rollback to v1 cheap (`load_ns` of about 27 us in the smoke test).
  `New()` still yields a fresh instance that then imports state.
* Code is never unmapped, so a late or stray call into the old version can
  never jump into freed memory. The price is the memory leak quantified
  below.

## 4. How to run

```bash
cmd/s2-plugin/build.sh                         # bin/s2-plugin, bin/plugins/{v1,v2}.so  (needs cgo)
cmd/s2-plugin/bin/s2-plugin -addr :8080        # -plugin-dir defaults to <exe dir>/plugins
curl -XPOST 'localhost:8080/admin/upgrade?to=v2'   # or kill -HUP <pid> (toggles v1<->v2)
cmd/s1-epoch-swap/smoke.sh cmd/s2-plugin/bin/s2-plugin 18082
GENS=20 cmd/s2-plugin/build.sh && cmd/s2-plugin/bin/memgrowth -n 20   # memory growth CSV
go test -race ./cmd/s2-plugin/     # builds plugins with the test binary's flags (-race)
# Linux:
docker run --rm -v $PWD:/src -w /src golang:1.25 bash -c 'OUT=/tmp/s2 cmd/s2-plugin/build.sh && go test -race ./cmd/s2-plugin/'
```

`/admin/upgrade?to=<name>` loads `<plugin-dir>/<name>.so`. Names are
restricted to `[A-Za-z0-9_.-]{1,64}`, so no path traversal is possible.

## 5. Measurements (Apple M4 Pro; Linux = Docker `golang:1.25`, arm64)

**End-to-end smoke test** (Linux): 200/200 `work_ms=50` requests returned 200.
Versions seen: 64 v1, 136 v2. Count duplicates: 0. Highest v1 count 128, lowest
v2 count 133. `new_version_serving` `load_ns` was 2.9 ms, `drain_ns` 28.9 ms,
`upgrade_done` `total_ns` 31.9 ms. A SIGHUP back to v1 (already loaded)
took 52 us. On macOS the first `plugin.Open` of v2.so takes about 280-300 ms
(dyld), but serving is unaffected because loading happens before the swap.

**Memory growth over repeated upgrades** (`memgrowth`): each generation is a
distinct v2 build (`v2_genN.so`) swapped in under 4 concurrent request
loops. RSS was sampled after `runtime.GC()`. Go heap in use stayed flat at
about 4-19 MiB, so the growth is mapped plugin code and data, not Go heap.

| Platform | RSS gen 1 -> gen 20 | Linear fit | Median load time |
|---|---|---:|---:|
| Linux arm64 | 27 MiB -> 140 MiB | **+5.3 MiB / upgrade** | 12.4 ms |
| macOS arm64 | 26 MiB -> 108 MiB | **+4.5 MiB / upgrade** | 283 ms |

Memory grows linearly and is never reclaimed. 1,000 upgrades would cost
roughly 5 GiB. Compare S3, where unloading keeps RSS flat.

## 6. Limitations (all observed, not hypothetical)

1. **No unload.** Go has no `plugin.Close`, and the runtime cannot
   `dlclose` because types, itabs, GC metadata and goroutines may still
   reference the module. See the growth measured above.
2. **Identical toolchain, flags and dependencies.** The host and plugins must
   be built by the same Go version with the same `-race`, `-trimpath`,
   `GOFLAGS` and dependency versions. Our test (`buildPlugin`) mirrors the
   test binary's `-race` setting for exactly this reason. A modified shared
   package makes loading fail.
3. **Same package path cannot be loaded twice.** A rebuilt, *modified* v2
   from the same import path fails with `plugin already loaded`
   (`TestSamePluginPathRejected`). `-ldflags=-pluginpath=X` alone does **not**
   help: symbols keep their compile-time package path, and `Open` then fails
   with `could not find symbol New`. Verified on macOS. The working
   "symbol versioning" trick is to give each generation a unique *import
   path* by copying its source into its own package directory
   (`bin/_gen/v2genN`, as done by `build.sh GENS=N`).
4. **cgo required, Linux/macOS/FreeBSD only.** No Windows support and no
   static `CGO_ENABLED=0` binaries. This excludes `scratch` or distroless
   static images.
5. **macOS dyld incompatibility.** With go1.24.3 on macOS 27, plugins linked
   by default are rejected by dyld ("chained fixups, seg_count does not match
   number of segments"). `build.sh` links them with
   `-extldflags=-Wl,-no_fixup_chains`. Linux needs no workaround.
6. **No isolation.** A plugin runs in the host's address space with full
   privileges. A panic or memory corruption in the plugin can take down the
   process. Loading untrusted code is out of scope.
7. Package-level state in plugins (globals, `init` side effects,
   background goroutines) is not transferred and is never stopped.

## 7. Threats to validity

* Only small plugins were measured (about 7-9 MB `.so`, mostly `net/http`
  already present in the host). Larger plugins grow memory faster.
* RSS includes Go heap noise: the Linux series fluctuates by about 20 MiB,
  so the linear fit over 20 points is more reliable than any single delta.
* `plugin.Open` latency on macOS is dominated by dyld. Linux, the target
  platform, is the relevant figure.
* The memory experiment calls `ServeHTTP` in-process (through
  `httptest.ResponseRecorder`) instead of over sockets. That isolates code
  loading cost but does not represent network-level latency.
