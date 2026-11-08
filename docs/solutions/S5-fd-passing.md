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
    O->>O: beginDrain (responses get Connection: close; idle conns stay open); Seal(): next = count + inflight
    O->>N: state {count: next, generation, ...}
    Note over C,K: new connections queue in the SAME accept queue
    N->>N: self-check without serving (SO_ACCEPTCONN + in-process /healthz)
    N->>O: ready (holds listener + state, has accepted nothing)
    O->>N: commit (point of no return: O never rolls back after this)
    N->>K: Serve(): accept()
    N->>N: new_version_serving
    O->>O: drain: serve/proxy requests on its conns, each with Connection: close
    O->>O: after -idle-grace (1 s): close conns idle for the whole grace
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
   *Second pitfall: keep-alive connections. The E2 campaign losses were
   caused by this one, which was fixed on 2026-11-08.* See "Keep-alive
   close race" below.
4. **Count continuity ("seal and reserve").** Increment and `inflight--`
   happen atomically under one mutex; `Seal` returns `count + inflight` and
   stops admitting. Every admitted request consumes exactly one number in
   `(count, count+inflight]`, the successor starts after that range, late
   requests are proxied: no duplicates, no gaps. Unit test
   `TestSealReservesInflightNumbers`; the in-process handoff test checks
   `final count == successful requests + 1`.

## Keep-alive close race (root cause of the E2 campaign losses, fixed 2026-11-08)

**Symptom.** In the final campaign, S5 lost 7-11 requests in every E2 run
(21 000 req/s, keep-alive), 42 in total. S4 lost 37 and S4+migrate 58. All of
them were on **reused** keep-alive connections (`conn_reused=1`, `attempts=1`),
classified `reset` or `eof`, starting 8.6-12.3 ms after the trigger. That is the
seal instant (spawn + `prepared` takes about 9 ms). E1, E4-ka and the earlier smoke
tests (new connection per request) showed none or almost none.

**Cause.** At the seal the old process called
`http.Server.SetKeepAlivesEnabled(false)`, and its drain loop called it every 2 ms.
In Go (checked in 1.25.14 `net/http/server.go`) this calls `closeIdleConns()`,
which immediately `close(2)`s every connection in `StateIdle`. At 21k req/s some of
those "idle" connections already carry a request the client has just written. The
three failure paths are:
- bytes unread in the server's buffer (Go marks `StateActive` only after reading),
  so the close sends RST and the client sees `reset`;
- the client writes just after the server's FIN and reads EOF (`eof`);
- a response written without `Connection: close` just before the flag flipped,
  followed by a close in `conn.serve` (the "such is life with HTTP/1.1" path). The
  next request on that connection is then lost.

This is the idle-close race of RFC 9112 section 9.5 ("a client might have started to
send a new request at the same time that the server has decided to close the 'idle'
connection"). `tcp_migrate_req` cannot help: it concerns not-yet-accepted requests
on a closing listener. `Shutdown` has the same behaviour.

**Why only the client can resolve it.** Any server-side close of an idle connection
can cross a request in flight. The only race-free signal is in-band: a response
carrying `Connection: close` (RFC 9112 section 9.6), after which a non-pipelining
client never writes on that connection again. Otherwise the client has to retry,
which RFC 9110 section 9.2.2 (referenced from RFC 9112 section 9.3.1) permits
automatically only for idempotent requests.

**Fix (`internal/handoff/drain.go`, shared by S4 and S5).**
1. At the seal, `beginDrain()` only sets a flag. Idle connections are **not**
   closed, and neither `SetKeepAlivesEnabled(false)` nor `Shutdown` is called while
   connections remain.
2. Every response whose header is written while draining gets
   `Connection: close`. A `ResponseWriter` wrapper decides at header-write time, so
   requests admitted before the seal are covered too, as are proxied late requests.
   net/http then closes after the response.
3. Idle connections stay open for `-idle-grace` (default 1 s). A reuse is served
   (or proxied to the successor) with `Connection: close`. Only connections idle
   for the whole grace are closed by the server, using per-connection
   `ConnState` tracking.
4. The drain ends when the tracked open-connection count reaches 0, and only then
   is `Shutdown` called.

**Result (same parameters as the campaign, 5 reps per cell).**

| cell | errors/run before | after |
|---|---|---|
| s5-fdpass E2 | 11/7/7/10/7 (42) | 0/0/0/0/0 |
| s4-reuseport E2 | 9/6/9/5/8 (37) | 0/0/0/0/0 |
| s4-reuseport+migrate E2 | 27/8/9/8/6 (58) | 0/0/0/0/0 |
| s4-reuseport+migrate E4-ka | 0/0/0/1/1 | 0/0/0/0/0 |
| s5-fdpass+crash E6 | 0/0/0/0/1 | 0/0/0/0/0 |
| all other S4/S5 cells of E1, E4-ka, E6 | 0 | 0 |

There were 0 failures in 70 runs (6.38 M requests). The cost, as medians: the
upgrade ends 4-9 ms later (S5 E2 6.1 -> 11.1 ms, E1 11.8 -> 16.0 ms), because the
old process waits for clients to come back and close. The E2 upgrade-window max is
about 2 ms higher (1.7 -> 3.6 ms) because of the proxy hop, and the upgrade-window
p99 is unchanged (0.75 -> 0.89 ms). The server had to close an idle connection
(rule 3) **0 times in 70 runs**: at these rates every pooled connection was reused
within the grace. Data: `results/handoff-fix/` (`summary.csv`, `medians.txt`,
`analysis/`, raw runs, drivers).

**Residual race.** Rule 3 is still a server-side close. It can lose a request only if
a client reuses a connection it has not used for at least IdleGrace, at the same
instant (tens of microseconds on loopback). Go's transport reuses the most recently
used idle connection first, so this needs a burst that exhausts all fresher
connections. It was never triggered here, so its rate is unmeasured. Raising
IdleGrace to the client's idle timeout removes rule 3 entirely, at the price of a
longer-lived old process.

**Measurement note: the loadgen is a strict no-retry client.**
`bench/loadgen -no-retry=true` (the default) sends GETs with a non-replayable
empty body. That disables Go's transparent retry on reused connections
(`persistConn.shouldRetryRequest`: `nothingWrittenError` needs a zero
`outgoingLength()` or `GetBody`, and `errServerClosedIdle` or a first-byte read
error needs `isReplayable()`). A default Go client would have hidden all of the
losses as latency. The no-retry setting is the right worst case: it is what every
client sees for non-idempotent requests. Behaviour of non-Go clients was not
verified here.

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
| Child dies after `state` (`simulateFail=crash-after-state`) | EOF on control socket | re-create listener from its saved dup of the **same socket** (`net.FileListener`), unseal, resume. Late requests whose forwarding to the dead child failed are served locally once the rollback is known | none (measured): the queue survives because the socket never closed |
| Child fails self-check or dies after `ready`, before `commit` | abort / EOF / ready timeout | same as above; spawned child is killed, an external (`-takeover`) child exits by itself on abort/EOF | none: the child has accepted nothing (two-phase) |
| Child dies after `commit` | - | **no rollback** (old keeps draining, then exits) | an ordinary crash of the new version. Rolling back here could hand out overlapping counts |

**Two-phase ready/commit (review M6, fixed 2026-11-08).** Before the fix
the child started `Serve()` *before* it sent `ready`, because it ran a
loopback self-check through the public listener. So a failure between its
first accepted request and `ready` made the old process roll back
(`resumeServing`) after the child had already served and incremented the
counter, and two servers then issued overlapping counts. An external
`-takeover` child (`cmd == nil`) was not even killed. Now the child self-checks
without serving: `SO_ACCEPTCONN` on the listener, plus an in-process
`GET /healthz` on its handler. It sends `ready` and calls `Serve()` only after
it reads a complete `commit` frame. The old process sends `commit` and never
rolls back after that write has succeeded. If the write fails, the frame was
not delivered: a unix stream socket reports partial writes as short counts,
which `SendFiles` completes or turns into an error. In that case the child
will never serve, and the rollback is safe for spawned and external
successors alike. The rule is: *a successor that might have served is never
rolled back over*. Tests: `TestSuccessorServesOnlyAfterCommit`,
`TestNoRollbackAfterCommit`, `TestRollbackAfterSeal`.

**Waiter race (review M5, fixed).** The channel on which the control loop
hands the spawned child's connection to the spawning goroutine was
unbuffered, and the send was non-blocking. If the child connected before the
goroutine reached its `select` (easy under CPU load), its connection was
treated as a second, external successor and rejected with "upgrade already in
progress". The channel now has a buffer of 1, and delivery and unregistration
happen under one mutex, so a connection can be neither rejected nor stranded
(`TestWaiterAcceptsConnectionBeforeReceiverWaits`).

**Descriptor hygiene (review Low, fixed).** `recvChunk` now wraps every
descriptor the kernel installed before it checks `MSG_CTRUNC` or parse
errors, and closes them all on error. Before the fix, a truncated control
message leaked the descriptors that fit (`TestRecvClosesFDsOnTruncation`,
Linux). `SendFiles` calls `runtime.KeepAlive(files)` after `sendmsg`, so a
finalizer cannot close a descriptor obtained with `Fd()` while it is still in
use.

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
  stay with the old process until their next response (sent with
  `Connection: close`), or until they have been idle for `-idle-grace`.
  Clients reconnect to the same socket. See "Keep-alive close race" for the
  one remaining server-side close.
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
