# Shared Contract for All Solutions (S*) and the Benchmark Harness

Every solution in `cmd/` implements the SAME observable behaviour so one load
generator and one analysis pipeline can compare them fairly.

## 1. Service behaviour ("the workload")

A toy *stateful* microservice:

| Endpoint | Behaviour |
|---|---|
| `GET /api?work_ms=N` | Sleeps `N` ms (default 0, simulates long requests), increments a service-wide request counter, returns `200` + JSON (below). |
| `GET /healthz` | `200 ok` when ready to serve. |
| `GET /admin/status` | JSON: `{"version":"v1","pid":123,"inflight":{"v1":3,"v2":0},"count":N}` |

`/api` response body (one line of JSON) and header `X-Version: <version>`:

```json
{"version":"v1","pid":4242,"count":1234,"ts_ns":1731000000000000000}
```

* `version` - logical code version that served the request (`v1`, `v2`, ...).
  v1 and v2 must be observably different (`version` string at minimum).
* `count` - service-wide counter. **It is the "state" that must survive an
  upgrade.** A correct state-preserving upgrade gives a strictly continuing
  sequence (no reset to 0, no duplicates beyond concurrency jitter).
* `pid` - lets the analysis see process-level handoffs.

## 2. Triggering an upgrade

Each solution must support **both**:

* `POST /admin/upgrade?to=v2` (HTTP, preferred by the harness; for
  process-level solutions this may exec/start the new process), and
* `SIGHUP` to the (original) server PID.

The upgrade is asynchronous; `/admin/upgrade` returns `202` immediately.

## 3. Upgrade event log (for measurement)

Each server writes JSON lines to **stderr** with these events:

```json
{"ev":"upgrade_start","ts_ns":...,"from":"v1","to":"v2","pid":...}
{"ev":"new_version_serving","ts_ns":...,"version":"v2","pid":...}
{"ev":"old_version_drained","ts_ns":...,"version":"v1","pid":...}
{"ev":"upgrade_done","ts_ns":...,"pid":...}
{"ev":"upgrade_failed","ts_ns":...,"err":"...","pid":...}   // rollback path
{"ev":"drain_timeout","ts_ns":...,"version":"v1","pid":...}  // new version serving, old not drained in time
```

`upgrade_failed` means the upgrade did not take effect: the old version
kept serving or was put back. `drain_timeout` means the new version *is*
serving (nothing was rolled back), but the old version still had work in
flight when the drain deadline passed. It keeps draining and
`old_version_drained` follows when it finishes. A `drain_timeout` is
followed by `upgrade_done` (the upgrade completed, the drain did not), and
it is never reported as `upgrade_failed`.

`ts_ns` = `time.Now().UnixNano()`.

## 4. Flags

All servers accept `-addr` (default `:8080`). Extra flags are fine but must
have defaults so `./server` alone works.

## 5. Ownership (avoid edit conflicts between agents)

| Path | Owner |
|---|---|
| `internal/hotswap/`, `cmd/s1-*`, `cmd/s2-*`, `cmd/s3-*`, `docs/solutions/S1-S3*` | Go-level hot-swap engineer |
| `internal/handoff/`, `cmd/b0-*`, `cmd/s4-*`, `cmd/s5-*`, `docs/solutions/B0*,S4*,S5*` | Process-handoff engineer |
| `bench/`, `scripts/`, `deploy/`, `docs/evaluation-methodology.md`, `results/` | Benchmark engineer |
| `docs/literature-survey.md`, `docs/paper-ideas.md`, `paper/` | Research scientist |
| `README.md`, `docs/CONTRACT.md`, `docs/problem-statement.md`, `go.mod` | Lead (coordinator) |

Only the Go-level engineer may add third-party Go dependencies (e.g.
`github.com/tetratelabs/wazero`). Everyone else: Go stdlib only (Python is
fine for analysis/plots). Shared helpers go in your own `internal/` package.

## 6. Platforms

Development host: macOS arm64. **Target / measurement platform: Linux**
(run inside Docker, `golang:1.25` image). Linux-only features
(`SO_REUSEPORT` load-balancing, FD passing semantics) must be tested in Docker.
