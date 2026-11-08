# B0 - Naive stop-then-start restart (baseline)

Code: `cmd/b0-naive-restart/` (shares the contract workload in `internal/handoff/service.go`).

## Problem addressed

None - this is the **comparison baseline**. It is what "upgrade by restarting
the binary" looks like without any handoff machinery: the old process stops
listening, then the new binary is started and binds the port. Every
zero-downtime technique (S1-S5) must beat it.

## Design

```mermaid
sequenceDiagram
    participant C as Clients
    participant K as Kernel :8080
    participant O as Old (v1)
    participant N as New (v2)
    O->>O: POST /admin/upgrade or SIGHUP -> upgrade_start
    alt -mode=graceful
        O->>K: close(listen fd)  (http.Server.Shutdown)
        Note over C,K: SYN -> RST: connection refused
        O->>O: wait for in-flight requests
    else -mode=hard
        O->>K: close(listen fd) + close all conns (http.Server.Close)
        Note over C,O: in-flight requests cut (EOF/reset)
    end
    O->>O: optional: write count to -state-file; old_version_drained
    O->>N: fork/exec (-exec-mode=spawn) or execve in place (-exec-mode=exec)
    O->>O: exit
    N->>N: optional -startup-delay, read -state-file
    N->>K: bind+listen (retries while port busy)
    N->>N: new_version_serving, upgrade_done
```

Flags: `-mode=graceful|hard`, `-exec-mode=spawn|exec`, `-state-file=PATH`
(empty = state lost), `-next-binary`, `-next-version`, `-startup-delay`,
`-allow-bin-param`, `-drain-timeout`.

## Correctness argument (why connections ARE lost)

Between `close(listen fd)` in the old process and `listen()` in the new one
there is no socket bound to the port, so the kernel answers SYNs with RST
(client sees `ECONNREFUSED`). The window is
`drain time (graceful) + exec/runtime start + init (-startup-delay) + bind`.
In `hard` mode the drain time is ~0 but in-flight requests are cut. Without
`-state-file` the counter restarts at 0, producing duplicate counts.

**Deliberately left on stdlib `Shutdown` (2026-11-08 handoff fix).**
`-mode=graceful` drains with `http.Server.Shutdown`. Like
`SetKeepAlivesEnabled(false)`, `Shutdown` calls `closeIdleConns` and closes
every idle keep-alive connection at once, so B0 also has the HTTP/1.1
idle-close race that caused the S4/S5 E2 losses. The keep-alive-safe drain
added to `internal/handoff/drain.go` (`Connection: close` on every response,
plus a grace period before idle connections are closed) is a method on the
S4/S5 `handoff.Server`. B0 uses its own `http.Server`, so the drain is not
shared code here. B0 is meant to be the naive baseline, and naive code calls
`Shutdown`, so B0 keeps it. The extra losses this adds are small next to B0's
refused-connection window (see `docs/results.md`).

## State transfer

Only via a file written after the old process stopped serving (`-state-file`).
This is correct only because the old process has fully stopped before the
new one starts (no overlap) - which is exactly what causes the outage.

## Failure / rollback

None. If the new binary fails to start the service is down (`upgrade_failed`
is emitted only when exec/spawn itself fails).

## Kubernetes fit

This is what a plain container image swap / pod restart does at a smaller
scale. With `-exec-mode=spawn` the old process exits and its child is
re-parented; if the old process is PID 1 the container exits with it, so in a
pod use `-exec-mode=exec` (same PID) or the S4/S5 `-supervise` mode as PID 1.
Readiness probes on `/healthz` would mark the pod unready during the window,
but traffic already routed to the pod (or kube-proxy/conntrack entries) still
hits the refused port.

## How to run

```bash
go build -o /tmp/b0-v1 ./cmd/b0-naive-restart
go build -o /tmp/b0-v2 -ldflags "-X main.version=v2" ./cmd/b0-naive-restart
/tmp/b0-v1 -mode=graceful -next-binary=/tmp/b0-v2 &
curl -X POST 'localhost:8080/admin/upgrade?to=v2'   # or kill -HUP <pid>
# end-to-end (Linux container):
docker run --rm -v $PWD:/src -w /src golang:1.25 bash internal/handoff/smoke/smoke.sh b0-graceful b0-hard-state
```

## Measured smoke results

Setup: `golang:1.25` container on Docker Desktop (linuxkit kernel 6.10.14, arm64), `internal/handoff/smoke/smoke.sh`, hammer = 32 closed-loop workers, new TCP connection per request, `work_ms=5`, 6 s run, upgrade triggered at t=2 s. "conn errors" = transport errors seen by the client (refused/reset/EOF); `non-200` was 0 in every run. Timings are from the stderr event log (relative to `upgrade_start`).

| Scenario | ok 200 | conn errors | duplicate counts | last v1 -> first v2 count | upgrade_start -> new_version_serving |
|---|---|---|---|---|---|
| `-mode=graceful`, no state file | 26702 | **131** (130 refused, 1 reset) | **9184** (count restarted at 1) | 9184 -> 1 | 17.5 ms (drain 8.7 ms + start/bind 8.8 ms) |
| `-mode=hard -state-file` | 26947 | **34** (29 refused, 5 EOF = cut in-flight) | 0 | 8879 -> 8880 | 4.9 ms |

An earlier identical run gave 169 refused (graceful) and 33 errors (hard), so the
error count varies with timing but is never zero. With `-state-file` the
counter continues; without it it restarts at 1. Note the state file only works
because nothing serves while the old process is stopped, and that overlap-free
period is exactly when connections are refused.


## Limitations / threats to validity

* Go processes start in a few ms, so the refused window is tiny (tens of ms);
  real services with JIT/warm-up/config loading have far larger windows
  (use `-startup-delay` to model this).
* Clients retry differently: the hammer counts every failed attempt and then
  retries after 1 ms, so the number of errors scales with concurrency x window.
* Single loopback host inside Docker Desktop's VM; no network latency.
