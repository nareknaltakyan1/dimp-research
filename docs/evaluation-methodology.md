# Evaluation Methodology

Owner: benchmark engineer. Implementation: `bench/`, `scripts/`, `deploy/`.
All solutions are measured through the black-box interface in `docs/CONTRACT.md`
(`GET /api`, `POST /admin/upgrade?to=…`, stderr JSON event log), so the same
load generator, driver and analysis apply to every mechanism.

## 1. Research questions

| RQ | Question | Primary metrics (section 4) |
|---|---|---|
| RQ1 Availability | Does the upgrade cause client-visible failures or unavailability? | errors by class, error rate, downtime, failure window, refused window |
| RQ2 Latency | How much does the upgrade inflate tail latency relative to steady state? | p50/p99/p99.9/max in steady vs. upgrade window, latency CDF |
| RQ3 State | Is in-memory service state preserved across the upgrade? | counter resets, regressions, duplicates, lost increments |
| RQ4 Resources | What does the mechanism cost in memory, and does memory leak across repeated upgrades? | RSS steady/peak/end, RSS growth, RSS slope per upgrade, max concurrent processes |
| RQ5 Upgrade time | How long does an upgrade take, and how long do two versions coexist? | upgrade duration, time to new version, version-mix window |
| RQ6 Safety | Does a failed upgrade roll back without client impact? | `upgrade_failed` emitted, no response from failed version, zero errors (`rollback_ok`) |

## 2. Systems under test

`b0-naive-restart` (stop/start baseline), `s1-epoch-swap`, `s2-plugin`,
`s3-wasm` (in-process code swap), `s4-reuseport`, `s5-fdpass` (process
handoff), and the **Kubernetes rolling-update baseline** (`k8s-naive`,
`k8s-prestop`; section 7). `bench/mockserver` is a harness-validation server,
not a solution; with `-mode restart` it is a *negative control* that must show
downtime and state loss, proving the harness can detect them.

## 3. Workload and procedure

* **Client.** `bench/loadgen`, open-loop constant rate. Request *i* is scheduled at
  `intended_i = t0 + i/rate`; latency is `end − intended`, i.e. the time a user
  arriving at the intended instant would wait. This avoids *coordinated
  omission*: a stalled server cannot slow the arrival process. The dispatcher
  busy-waits the last 2 ms before each send (timer wake-up jitter in the VM was
  ~1.2 ms median otherwise; with spinning, median `start − intended` = 0.04 ms).
  A concurrency cap bounds in-flight requests; if hit, the wait is visible as
  `start − intended` and is charged to latency. `client_lag_p99_ms` is reported
  for every run as a validity check.
* **No transparent retries.** Go's `net/http` silently retries idempotent GETs that
  fail on a reused keep-alive connection. That would mask exactly the failures a
  handoff causes, so loadgen makes requests non-replayable (`-no-retry`, default)
  and records connection acquisitions per request (`attempts`).
* **Request mix.** `work_ms` mix, seeded RNG (`seed = SEED + rep`). Short
  requests (`work_ms=0`) are used for latency statistics; long requests stress
  draining.
* **Keep-alive.** On (connection reuse; exposes stale/closed pooled connections at
  handoff) and off (new TCP connection per request; exposes accept-queue/listen
  gaps → `ECONNREFUSED`).
* **Run timeline.** build → start server (own session, pinned CPUs) → wait for
  `/healthz` → start RSS sampler and loadgen at `t0` → trigger upgrade(s) at
  `t0 + UPGRADE_AT_S` (+ `k·UPGRADE_INTERVAL_S`) on an absolute schedule → load
  continues `TAIL_S` after the last trigger → stop. The first `WARMUP_S` seconds
  are excluded from all statistics.
* **Trigger.** `POST /admin/upgrade?to=<target>` (default) or `SIGHUP` to the
  original PID (`TRIGGER=sighup`). The trigger instant is recorded by the driver
  (`triggers.csv`) and is the time origin of plots.

### Experiment matrix (`scripts/experiments.sh`)

| ID | Purpose | Rate (req/s) | work_ms mix | keep-alive | Upgrades | Default reps |
|---|---|---|---|---|---|---|
| E1 | baseline correctness, moderate load | 200 | 100% 0 | on | 1 at 10 s, tail 10 s | 5 |
| E2 | near saturation | 0.8 × calibrated max (`scripts/calibrate.sh`) | 100% 0 | on | 1 | 5 |
| E3 | long-request draining | 200 | 90% 0 / 10% 500 | on | 1 | 5 |
| E4-ka / E4-noka | connection handling | 500 | 95% 0 / 5% 100 | on / off | 1 | 5 |
| E5 | repeated upgrades, leaks | 100 | 100% 0 | on | 50, every 2 s, alternating v2/v1 | 3 |
| E6 | failed upgrade / rollback | 200 | 100% 0 | on | 1 to `FAIL_TARGET` (default `v2-broken`) | 5 |

E2 calibration: rates are swept (8 s each, no upgrade); the highest rate with no
errors and p99 < 50 ms is the saturation point; E2 runs at 80 % of it.

**Open contract items.** (a) E5 assumes a solution can upgrade back to `v1` (or any
alternating pair; set `UPGRADE_TARGETS` in `cmd/<sol>/bench.env`). (b) The
contract does not define how to induce a failing upgrade; E6 uses `to=$FAIL_TARGET`
and each solution must declare a target that fails (e.g. a plugin that fails
its health check).

## 4. Metrics (exact definitions)

Notation: *post* = requests with `intended ≥ t0 + WARMUP_S`; *ok* = no transport
error and HTTP 200; `T` = first trigger time; `E` = last `upgrade_done` or
`upgrade_failed` event time.

* **Windows.** *Steady* = post requests with `intended < T`. *Upgrade window* =
  post requests with `T ≤ intended ≤ E + 1 s` (if no completion event: last
  trigger + 5 s; flagged `upgrade_completed_event=false`).
* **Errors.** Count per class: `refused` (ECONNREFUSED), `reset`
  (ECONNRESET/EPIPE), `timeout` (client timeout, default 5 s), `eof`
  (connection closed before/during response), `other`, `http_<code>` (non-200).
  Error rate = errors / post requests.
* **Downtime (`downtime_ms`).** Longest interval between consecutive *successful
  response completions* (`end_ns`) within `[T − 1 s, E + 1 s]`. Its floor is the
  inter-arrival time (5 ms at 200 req/s); compare with
  `steady_max_success_gap_ms` (same statistic over the steady window).
  Note: in-flight requests admitted before the outage that complete during it
  count as successes, so with long requests (E3/E4) `downtime_ms` can be
  shorter than the failure window; read both.
* **Failure window.** Longest streak of consecutive failed requests in intended
  order; span from the first failed request's intended time to the intended time
  of the next successful request.
* **Refused window.** `max − min` intended time of refused requests + one
  inter-arrival time (0 if none).
* **Latency.** Successful `work_ms=0` requests, `end − intended`, quantiles
  p50/p99/p99.9/max in the steady and upgrade windows (linear-interpolated
  quantiles). For E3, `lat_upg_long_max_ms` = max latency of successful long
  requests in the upgrade window, and `long_requests_upg_failed` = long requests
  that failed (draining failures).
* **Upgrade duration.** `upgrade_done.ts − upgrade_start.ts` (server events),
  averaged over upgrades in the run; `trigger_to_start_ms` = start event − trigger.
* **Time to new version.** First successful response (server `ts_ns`) carrying
  the target version − trigger time.
* **Version-mix window.** Per swap, using server timestamps of successful
  responses between this trigger and the next: last response of the old version
  − first response of the new version, clipped at 0. Mean (and max) over swaps.
* **State continuity.** Successful responses sorted by server `ts_ns`. With
  tolerance `J = concurrency cap` (increment-vs-timestamp reordering under
  concurrency), a drop `count < running_max − J` is a *reset* if
  `count < 0.1·running_max`, else a *regression*; the running max then restarts
  from the new value. *Duplicates* = successful responses − distinct count values
  (exactly 0 for a single atomic counter, regardless of concurrency).
  *Lost increments* = `max − min + 1 − distinct` (increments whose response was
  never received; only when there was no reset).
* **Memory.** `bench/procmon` samples `/proc/<pid>/status` (VmRSS, VmHWM, threads,
  open FDs) every 100 ms for **every process in the server's session** (the
  server is started with `setsid`, so successors forked/exec'd by the server are
  included even after the original exits). Session RSS = sum over processes
  per sample. Reported: steady (median before `T`), peak, end (median of last
  2 s), growth = end − steady, `max_procs`, and for ≥3 upgrades the
  least-squares slope of session RSS sampled just before each trigger vs.
  upgrade index (KiB/upgrade).
* **Rollback (E6).** `rollback_ok` = ≥1 `upgrade_failed` event, no successful
  response from the failing target version, and zero errors.

## 5. Testbed

* Host: Apple Silicon Mac (macOS, arm64), Docker Desktop Linux VM (12 vCPU, 7.65 GiB,
  kernel `6.10.14-linuxkit`). Exact values are captured per run in
  `metadata.json` (`host.*`).
* Container: `deploy/Dockerfile.bench` (`golang:1.25`, Debian trixie, Go 1.25.x,
  Python 3 + pandas/matplotlib/scipy). Started by `scripts/in_docker.sh` with
  `--cpuset-cpus 0-5 --cpus 6 --memory 4g`, `SYS_PTRACE`, unconfined seccomp
  (needed by binary-patching approaches), `ip_local_port_range=1024 65000`,
  `tcp_tw_reuse=1`, `somaxconn=4096`.
* Inside: server pinned to CPUs 0-1 (`SERVER_CPUS`), loadgen + procmon to CPUs
  2-5 (`LOADGEN_CPUS`) with `taskset`, so the client does not steal server CPU.
  Client and server talk over loopback (isolates the mechanism from network noise).
* Kubernetes baseline: kind cluster (single node container, pinned with
  `docker update --cpuset-cpus 0-5`), 3 replicas, client is an in-cluster Job via
  the ClusterIP Service (kube-proxy).
* Each run records git SHA/dirty flag, kernel, Go version, CPU affinity, cgroup
  CPU/memory limits, sysctls and all workload parameters.

## 6. Statistical method

* Each (solution, experiment) is repeated `REPS` times (default 5; E5: 3) with a
  fresh server process and a different workload seed per repetition.
* Per-run metrics are computed first; aggregation is across runs (the run is the
  unit of replication — requests within a run are not independent).
* Reported: mean ± 95 % confidence half-width, `t_{0.975,n−1} · s/√n`, plus
  min/max (`results/summary.csv`). With n = 3–5 the CIs are wide; for claims of
  "zero errors" report the count over all runs (e.g. 0 / 20 000 requests) rather
  than a CI. For heavy-tailed metrics (max latency, downtime) prefer comparing
  medians/min–max and the pooled CDFs.
* Warm-up: `WARMUP_S` (3 s) excluded; the upgrade happens ≥ 7 s after warm-up.
* Order effects: run solutions interleaved or in randomised order when doing the
  final campaign (`run_matrix.sh` runs them sequentially; shuffle the list).

## 7. Kubernetes rolling-update baseline

`scripts/k8s_rolling_baseline.sh` deploys the mock server (`deploy/k8s/mock.yaml`)
and upgrades it with `kubectl set env VERSION=v2` (a standard rolling update,
`maxSurge=1, maxUnavailable=0`, readiness probe every 1 s). Variants: `naive`
(readiness probe only) and `prestop` (`preStop: sleep 5s`, the recommended
pattern so endpoints are removed before SIGTERM). Upgrade start = the
`kubectl set env` instant, upgrade done = `rollout status` returned; both taken on
the kind node clock (same clock as the loadgen pod). The counter is per pod (no
shared state), so state-continuity metrics are expected to show resets/
duplicates: this *is* the property in-process patching aims to improve. Not
measured for k8s: RSS (no procmon in pods), pid (always 1).

## 8. Threats to validity

* **Internal.** Client and server share one VM/kernel; pinning to disjoint CPUs
  reduces but does not remove interference (shared caches, the VM's hypervisor
  scheduling). Docker Desktop on macOS adds a virtualisation layer; absolute
  latencies (sub-ms) are not representative of bare-metal Linux, but
  comparisons between mechanisms under identical conditions are. Occasional
  multi-ms VM stalls appear in all runs (max latency ~7–9 ms in steady state for
  the mock), so max latency below ~10 ms is noise. Timestamps from driver,
  loadgen and server share `CLOCK_REALTIME` inside one container (k8s: one node);
  the driver's trigger timestamp precedes the HTTP call by < 1 ms (k8s: `docker
  exec` + `kubectl` adds ~50–150 ms between recorded trigger and the API call).
  Event timestamps are self-reported by the solutions. The VM wall clock is
  periodically re-synced (observed steps of −7.9 … +5.3 ms within a run);
  loadgen therefore derives all its timestamps from one wall-clock anchor plus
  the monotonic clock (latencies are exact; alignment with driver/server
  timestamps is only as good as the step, reported as `wall_step_ns` in
  `loadgen.log`). An earlier loadgen version used wall readings directly and
  produced negative latencies; those runs were discarded.
* **Shared host.** Other engineers' containers on the same Docker VM compete for
  CPU (the validation runs below overlapped with such a container).
  `scripts/in_docker.sh` warns and records `docker_other_containers_at_start`;
  final campaigns must run on an otherwise idle host.
* **Construct.** "Downtime" from client observations is bounded below by the
  inter-arrival time and depends on rate; we therefore also report the failure
  and refused windows and raw error counts. The counter is a minimal proxy for
  service state; it does not capture e.g. open sessions or caches. Version
  attribution relies on the `version` field the solution returns.
* **External.** One toy service, HTTP/1.1, loopback, single node. Results for
  gRPC/HTTP2 multiplexed connections, large heaps, or multi-node networks may
  differ. The k8s baseline uses kind (no real load balancer, single node).
* **Conclusion.** Small n (3–5) per cell; CIs are reported and claims are limited
  to differences whose CIs do not overlap or to categorical outcomes (errors
  vs. none across all runs).

## 9. How to run

```bash
# one experiment for one solution (Linux container, CPU-pinned):
REPS=5 scripts/in_docker.sh scripts/run_experiment.sh s1-epoch-swap E3
# full matrix for several solutions + analysis:
scripts/in_docker.sh scripts/run_matrix.sh b0-naive-restart s1-epoch-swap s4-reuseport
# calibrate E2's rate first:
scripts/in_docker.sh scripts/calibrate.sh s1-epoch-swap
# analysis only (all runs under results/raw):
scripts/in_docker.sh python3 bench/analysis/analyze.py
# Kubernetes baseline (host; installs kind into ./bin if missing):
VARIANT=naive REPS=3 scripts/k8s_rolling_baseline.sh
```

Solution hooks: `cmd/<sol>/build.sh <out_dir>` (must create `<out_dir>/server`),
optional `cmd/<sol>/bench.env` (e.g. `SERVER_ARGS`, `TRIGGER=sighup`,
`FAIL_TARGET`, `UPGRADE_TARGETS="v2 v1"`). The server runs with cwd `<out_dir>`
and gets `-addr 127.0.0.1:18080`.

## 10. Harness validation (mock server, 2026-11-07)

Purpose: show the harness measures what it claims, using `bench/mockserver`
in-process swap (`mock`, should be perfect) and the negative control
`mock -mode restart -down-ms 300` (`mock-restart`, must show ≈300 ms outage and
a counter reset). Testbed as in section 5; another team's container was running
concurrently, so absolute latencies are noisy. Full tables:
`results/summary.md`, per-run: `results/summary_runs.csv`.

E1 (200 req/s, 3 reps, 3 400 measured requests per run):

| | mock | mock-restart | k8s-naive | k8s-prestop |
|---|---|---|---|---|
| errors per run (classes) | 0, 0, 0 | 60, 60, 61 (refused; 1 eof) | 6, 9, 15 (refused; 1 eof) | 1, 0, 0 (eof) |
| downtime ms (mean ± 95 % CI) | 9.0 ± 5.5 | 306.0 ± 3.7 | 185.3 ± 69.2 | 8.6 ± 8.8 |
| failure window ms | 0 | 301.7 ± 7.2 | 15.0 ± 12.4 | 1.7 ± 7.2 |
| upgrade duration ms | 21.3 ± 1.6 | 322.7 ± 5.2 | 1409.7 ± 54.8 (rollout) | 1440.5 ± 53.6 |
| max latency in upgrade window ms | 3.8 ± 6.3 | 2.7 ± 5.8 | 177.5 ± 68.3 | 3.9 ± 8.7 |
| counter resets / duplicates | 0 / 0 | 1 / ≈1 334 | 2–3 / ≈5 000 (per-pod state) | 2–3 / ≈5 000 |

Checks that passed: refused count ≈ rate × outage (60 ≈ 200/s × 0.3 s); restart
detected as exactly one reset; atomic counter gives 0 duplicates; E6 rollback
detected (`rollback_ok` 2/2, only `v1` served, 0 errors); version-mix window
tracks draining (E3 with 500 ms requests: 447 ms; E4 with 100 ms requests: 80 ms;
E1: 0); E5 with an intentional 1 024 KiB leak per upgrade measured 1 164 KiB/upgrade
(Go heap overhead) vs 30.5 KiB/upgrade without the leak; E2 calibration found
~12 000 req/s saturation for the mock (E2 at 9 600 req/s: 0 errors, 2 reps);
dispatcher lag p99 typically < 1 ms (max 5.7 ms in one E2 run under contention).
The kind baseline shows the expected textbook result: a rolling update without
`preStop` drops connections when old pods get SIGTERM before endpoint removal
propagates; `preStop: sleep 5` removes almost all errors but the rollout takes
~1.4 s and the in-memory state is not preserved.
