# S5 - Listening-socket FD passing (SCM_RIGHTS) with state handoff

Code: `cmd/s5-fdpass/` (thin `main`), `internal/handoff/` (library:
`fdpass.go` SCM_RIGHTS send/recv, `proto.go` framed control protocol + state
envelope, `upgrader.go` orchestration/readiness/rollback/drain,
`service.go` contract workload with seal-and-reserve counter,
`supervisor.go` PID-1 subreaper mode). Same family as Cloudflare
`tableflip`, Envoy hot restart and Meta's socket takeover (Zero Downtime
Release, SIGCOMM'20).

## Problem addressed

Replace the *process* (a new binary: new code, new runtime, new memory
layout) without a single refused/reset connection and without losing the
in-memory state (`count`). B0 shows the cost of not doing this.

## Design

The old process owns a unix control socket (`-ctl-sock`, default
`/tmp/dimp-fdpass.sock`). On `POST /admin/upgrade?to=v2[&bin=]` or `SIGHUP`
it spawns the new binary with `DIMP_HANDOFF_CHILD=1` (or an externally
started process runs with `-takeover`), and the two speak a strictly
alternating, length-prefixed JSON protocol over that socket. Descriptors ride
as SCM_RIGHTS ancillary data on exactly one frame (`offer`), and the frame
announces `num_fds`, so a descriptor can never be attributed to the wrong
message.

```mermaid
sequenceDiagram
    participant C as Clients
    participant K as Kernel socket :8080 (one accept queue)
    participant O as Old v1
    participant N as New v2
    O->>O: upgrade_start; spawn N (env DIMP_HANDOFF_CHILD=1)
    N->>O: hello {pid, version, mode=fdpass}
    O->>N: offer + SCM_RIGHTS(listen fd)
    N->>N: net.FileListener(fd) -- does NOT accept yet
    N->>O: prepared
    Note over O: old still accepting; any failure so far = trivial rollback
    O->>K: stop accept(): close OUR fd only (socket stays open in N)
    O->>O: keep-alives off; Seal(): next = count + inflight
    O->>N: state {count: next, generation, ...}
    Note over C,K: new connections queue in the SAME accept queue
    N->>K: Serve(): accept()
    N->>N: GET /healthz via loopback (self-check)
    N->>O: ready
    N->>N: new_version_serving
    O->>O: drain: serve own in-flight + proxy late requests to N
    O->>O: old_version_drained, upgrade_done, exit 0
```

## Correctness argument (why no connection is lost)

1. **One socket, one accept queue.** A listening socket is a kernel object;
   SCM_RIGHTS installs another descriptor for the *same* object in the new
   process. Closing the old process' descriptor only drops a reference. So
   at every instant the port has a listening socket, and any connection the
   kernel queued is eventually `accept()`ed by someone (old before the seal,
   new after). Contrast S4, where each process has its own queue.
2. **No accept gap is visible as an error**: between the old closing its fd
   and the new calling `accept()` connections wait in the queue (latency, not
   loss). `-stall-accept=300ms` makes this window artificially long: still 0
   errors, max latency ~310 ms.
3. **Connections accepted by the old but not yet read.** These are served by
   the old process: requests admitted before the seal run locally; requests
   read after the seal are reverse-proxied to the new process over loopback
   (the old can no longer own counter values). The old only exits once its
   ConnState-tracked connection count is 0.
   *Pitfall found during this work:* calling `http.Server.Shutdown` to drain
   loses such connections - Go's `conn.serve` returns without a response if
   the request is read after shutdown started (`if c.server.shuttingDown()
   { return }` in `net/http/server.go`). Our first version did that and showed
   1-8 `EOF` errors per upgrade; see "Measured results". The drain therefore
   waits for tracked connections before calling `Shutdown`.
4. **Count continuity ("seal and reserve").** Increment and `inflight--`
   happen atomically under one mutex; `Seal` returns `count + inflight` and
   stops admitting. Every admitted request consumes exactly one number in
   `(count, count+inflight]`, the successor starts after that range, late
   requests are proxied: no duplicates, no gaps. Unit test
   `TestSealReservesInflightNumbers`; the in-process handoff test checks
   `final count == successful requests + 1`.

## State transfer

`state` frame = JSON `State{count, from_version, from_pid, sealed_at_ns,
generation, extra}` sent *after* the old stops admitting and *before* the new
starts accepting. Arbitrary further state can go in `extra` or additional
descriptors (e.g. a memfd / shared memory segment) on the same channel.

## Failure and rollback

| Failure point | Detection | Old process action | Loss |
|---|---|---|---|
| Child fails to start / never connects | `exec` error, child exit, `-ready-timeout` | kill child, `upgrade_failed`, keep serving | none |
| Child hangs before `prepared` (`simulateFail=hang`) | read deadline (`-ready-timeout`) | same; old never stopped accepting | none (measured) |
| Child dies after `state` (`simulateFail=crash-after-state`) | EOF on control socket | re-create listener from its saved dup of the **same socket** (`net.FileListener`), unseal, resume | none (measured): the queue survives because the socket never closed |
| Child serves some requests then dies before `ready` | EOF | as above | counts the child issued may be re-issued (documented limitation) |

After any failure `upgrading` resets, so the upgrade can be retried.

## Kubernetes fit

* **One container, one port**: no Service/Endpoints change, no extra pod,
  no surge capacity; the pod's IP and conntrack entries stay valid.
* **PID 1**: the new process is a child of the old; when the old exits the
  child is re-parented. If the old process were PID 1 the container would
  stop. Run with `-supervise`: a tiny supervisor stays PID 1, sets
  `PR_SET_CHILD_SUBREAPER`, reaps everything, forwards SIGHUP/SIGTERM to the
  pid in `-pidfile` (the active server), and exits when no descendants remain.
  Verified: after an upgrade the v2 worker's PPID is the supervisor.
* **Readiness probes**: `/healthz` returns 503 while the old is sealed; the
  new process' self-check gates `ready`. A kubelet probe during the handoff
  sees either the old (ready) or new (ready) - the window is milliseconds.
* **Triggering**: a sidecar/operator can `POST /admin/upgrade?to=v2`, send
  SIGHUP to PID 1 (`kubectl exec pod -- kill -HUP 1`), or run the new binary
  itself with `-takeover` (`kubectl exec ... /bin/s5-v2 -takeover`) - the
  Meta-style "new process asks old for the sockets" path. The new binary must
  already be in the container's filesystem (shared volume populated by an
  init/sidecar container, or an image that ships v1 and v2); this is the main
  friction with immutable-image practice.
* SIGTERM (pod deletion) closes the listener and drains tracked connections.

## How to run

```bash
go build -o /tmp/s5-v1 ./cmd/s5-fdpass
go build -o /tmp/s5-v2 -ldflags "-X main.version=v2" ./cmd/s5-fdpass
/tmp/s5-v1 -next-binary=/tmp/s5-v2 &         # add -supervise when PID 1
curl -X POST 'localhost:8080/admin/upgrade?to=v2'   # or kill -HUP <pid>
# without -next-binary the same binary is re-exec'd with -version=<to>
# end-to-end in Linux:
docker run --rm -v $PWD:/src -w /src golang:1.25 bash internal/handoff/smoke/smoke.sh
```

Rollback test binaries: `-ldflags "-X main.version=v2 -X main.simulateFail=hang"`
or `=crash-after-state`.

## Measured smoke results

Setup: `golang:1.25` container on Docker Desktop (linuxkit kernel 6.10.14, arm64), `internal/handoff/smoke/smoke.sh`, hammer = 32 closed-loop workers, new TCP connection per request, `work_ms=5`, 6 s run, upgrade triggered at t=2 s. "conn errors" = transport errors seen by the client (refused/reset/EOF); `non-200` was 0 in every run. Timings are from the stderr event log (relative to `upgrade_start`).

Final run (one each):

| Scenario | ok 200 | conn errors | duplicates | last v1 -> first v2 | phases (from upgrade_start) |
|---|---|---|---|---|---|
| `s5` HTTP trigger | 29348 | **0** | 0 | 9934 -> 9935 | new serving 2.3 ms, old drained 13.8 ms |
| `s5-stall` (old stops accepting 300 ms before handing off) | 27702 | **0** | 0 | 9664 -> 9665 | new serving 307 ms; max latency 310 ms |
| `s5-sighup` (SIGHUP trigger) | 28825 | **0** | 0 | 9942 -> 9943 | 1.8 ms / 9.9 ms |
| `s5-rollback-hang` (successor never prepared, `-ready-timeout=2s`) | 28724 | **0** | 0 | stays v1 | `upgrade_failed` at 2002.8 ms ("i/o timeout") |
| `s5-rollback-crash` (successor exits after state, post-seal) | 21733 | **0** | 0 | stays v1 (resumed on same socket) | `upgrade_failed` at 3.2 ms (EOF); max latency 135 ms |
| `s5-twice` v1 -> v2 -> v3 (second hop re-execs self with `-version=v3`) | 12785 + 15484 | **0 / 0** | 0 | 4647 -> 4648, then v2 -> v3 at 12786 | generation 1, then 2 |
| `s5-supervised` (`-supervise`, SIGHUP to supervisor) | 29276 | **0** | 0 | 10054 -> 10055 | after the upgrade, v2 worker PPID = supervisor PID |

Repeated (4 s runs, after the drain fix): **12 / 12 runs with 0 errors.**

Before the drain fix (old process drained with `http.Server.Shutdown`), 15
repetitions showed 1-8 `EOF` errors in about half the runs, all at the
upgrade instant. That is the net/http behaviour described in correctness point
3. This is worth reporting in the paper: "graceful" drain with Go's Shutdown is
not lossless for connections that were accepted but not yet read.


## Limitations

* Linux/Unix only (SCM_RIGHTS, `SO_REUSEPORT`, subreaper are Linux; unit
  tests also pass on macOS).
* Only the *listening* socket moves; established keep-alive connections
  stay with the old process until their current request finishes (then
  closed with `Connection: close`) - clients reconnect to the same socket.
  Long-lived streams (WebSocket, gRPC streams) would need connection-level
  handoff or Meta's Partial Post Replay / downstream drain signalling.
* Late requests are proxied through the old process (extra hop, briefly).
* The new binary must be present on the container filesystem.
* `?bin=` is arbitrary exec; disabled unless `-allow-bin-param`.

## Threats to validity

* Loopback inside Docker Desktop's linuxkit VM (kernel 6.10), one host, one
  closed-loop client (32 workers, new TCP connection per request, `work_ms=5`);
  no packet loss, no real network RTT.
* Go processes start in ~3 ms; heavier runtimes lengthen the queueing window
  (which raises latency, not errors, for S5).
* The hammer retries 1 ms after an error, so error counts depend on
  concurrency and are only comparable within this harness.
* Repetition counts are small (stated per row); the bench harness should
  produce the statistically meaningful numbers.
