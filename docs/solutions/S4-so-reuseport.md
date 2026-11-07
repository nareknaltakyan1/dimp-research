# S4 - Overlapping processes with SO_REUSEPORT

Code: `cmd/s4-reuseport/` (thin `main`) and `internal/handoff/` (same
orchestration as S5, `Mode = reuseport`; `reuseport.go` sets `SO_REUSEPORT`
via `net.ListenConfig.Control` + `syscall.SetsockoptInt`). Same protocol,
state transfer, drain and rollback code as S5, so the **only** variable
between S4 and S5 is how the new process gets a listening socket.

## Problem addressed

Start the new binary while the old one still listens, so there is never a
moment without a listener on the port, then retire the old process. This is
the "simplest thing that almost works" (used by many Go/Rust servers and by
Nginx-less setups).

## Design

```mermaid
sequenceDiagram
    participant C as Clients
    participant K as Kernel reuseport group :8080
    participant O as Old v1 (socket A, queue A)
    participant N as New v2 (socket B, queue B)
    O->>O: upgrade_start; spawn N
    N->>K: socket B, SO_REUSEPORT, bind+listen (joins group)
    Note over C,K: kernel now hashes new connections to A or B
    N->>O: hello (ctl unix socket)
    O->>N: (no fd)  N->>O: prepared
    O->>K: close(A)  <-- connections still in queue A get RST
    O->>O: keep-alives off; Seal(): next = count + inflight
    O->>N: state {count: next}
    N->>K: accept() on B (queue B kept connections hashed to it)
    N->>O: ready (after /healthz self-check)
    O->>O: drain own + proxy late requests to N; exit
```

The new process binds only after it has initialised and does not `accept()`
until it holds the state, so connections hashed to queue B simply wait.

## Correctness argument - and where it breaks

* There is always at least one listening socket in the group, so no
  `ECONNREFUSED` (unlike B0).
* Count continuity: same seal-and-reserve argument as S5 (requests are only
  admitted by the old before the seal and by the new after it).
* **Known Linux pitfall (demonstrated):** every reuseport socket has its own
  accept queue. When the old process closes socket A, the kernel resets every
  connection that completed the handshake into queue A but was not yet
  `accept()`ed (`inet_csk_listen_stop`). Those clients see
  `connection reset by peer` even though a perfectly good listener (B)
  exists. Under normal operation Go accepts quickly so queue A is short and
  only a few connections are lost; if the old process is slow to accept
  (GC pause, CPU saturation - modelled by `-stall-accept=300ms`, which stops
  calling `accept()` for 300 ms before closing) the loss grows with the queue.
* **Mitigation:** Linux >= 5.14 `net.ipv4.tcp_migrate_req=1` makes the kernel
  migrate queued (and SYN_RECV) requests from a closing listener to another
  socket in the same reuseport group. It is per network namespace, so in K8s
  it can be set as a pod-level "safe"-listed sysctl only if allowed by the
  cluster (`securityContext.sysctls`, or `--allowed-unsafe-sysctls`). Other
  mitigations: eBPF `SO_ATTACH_REUSEPORT_EBPF` steering away from the old
  socket before closing, or simply use S5 (shared socket, no second queue).

## State transfer

Identical to S5: JSON `State` envelope over the unix control socket
(`-ctl-sock`, default `/tmp/dimp-reuseport.sock`), sent after the seal and
before the new process accepts.

## Failure and rollback

Old process keeps serving if the child fails before `prepared`. If the child
dies after `state`, the old re-binds a fresh reuseport socket, unseals and
resumes (measured: `upgrade_failed` emitted, service stays v1, count
continues). But unlike S5 the connections queued in the dead child's queue
B **are reset** - rollback is not lossless with reuseport.

## Kubernetes fit

Same as S5 (one container, `-supervise` for PID 1, readiness via `/healthz`,
triggers via HTTP / `kill -HUP 1` / `-takeover`). Additional concern: the
mitigation needs a namespaced sysctl that many clusters do not allow.
Also note any process in the pod's network namespace with the same UID could
join the reuseport group (Linux restricts reuseport groups to the same
effective UID).

## How to run

```bash
go build -o /tmp/s4-v1 ./cmd/s4-reuseport
go build -o /tmp/s4-v2 -ldflags "-X main.version=v2" ./cmd/s4-reuseport
/tmp/s4-v1 -next-binary=/tmp/s4-v2 [-stall-accept=300ms] &
curl -X POST 'localhost:8080/admin/upgrade?to=v2'
# pitfall demo with/without kernel mitigation:
docker run --rm -v $PWD:/src -w /src golang:1.25 bash internal/handoff/smoke/smoke.sh s4 s4-stall
docker run --rm --sysctl net.ipv4.tcp_migrate_req=1 -v $PWD:/src -w /src golang:1.25 \
    bash internal/handoff/smoke/smoke.sh s4 s4-stall
```

## Measured smoke results

Setup: `golang:1.25` container on Docker Desktop (linuxkit kernel 6.10.14, arm64), `internal/handoff/smoke/smoke.sh`, hammer = 32 closed-loop workers, new TCP connection per request, `work_ms=5`, 6 s run, upgrade triggered at t=2 s. "conn errors" = transport errors seen by the client (refused/reset/EOF); `non-200` was 0 in every run. Timings are from the stderr event log (relative to `upgrade_start`).

| Scenario | runs | conn errors per run | duplicates | count continuity | upgrade_start -> new serving |
|---|---|---|---|---|---|
| `s4` (no stall), final run | 1 | 0 | 0 | 8822 -> 8823 | 5.7 ms |
| `s4` (no stall), repeated (4 s runs) | 12 | 0 in 10 runs; **16** and **3** resets in 2 runs | 0 | continuous | - |
| `s4-stall` (`-stall-accept=300ms`), `tcp_migrate_req=0` | 1 + 5 | **18**; then 18, 17, 11, 12, 10 (all `connection reset by peer`) | 0 | 8839 -> 8840 | 306 ms |
| `s4-stall`, `--sysctl net.ipv4.tcp_migrate_req=1` | 5 | **0, 0, 0, 0, 0** | 0 | continuous | ~306 ms (max latency 308-317 ms) |
| `s4-rollback` (successor binary exits after receiving state) | 1 | 1 reset | 0 | stays v1, count continues (27514 total) | `upgrade_failed` after 7.1 ms |

The resets cluster at the upgrade instant (+2007-2009 ms in the client's
error samples), both as `read: connection reset` (connection already queued in
the old socket's accept queue) and `connect: connection reset` (handshake in
progress when the socket closed). The kernel mitigation removes all of them;
the stall only changes latency then. The single reset in the rollback run is
the reuseport cost of a crashing successor: its own queue B is discarded. S5
avoids this because both processes share one queue.


## Limitations / threats to validity

* Loss depends on accept-queue occupancy at close time, hence on load,
  scheduling and Go's accept loop; small-sample numbers below are indicative.
* Docker Desktop linuxkit kernel 6.10 on arm64; behaviour of
  `tcp_migrate_req` requires >= 5.14.
* Same harness caveats as S5 (loopback, closed-loop client, retry policy).
