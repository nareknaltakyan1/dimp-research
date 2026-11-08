# Handoff Keep-Alive Fix: Before/After (S4, S5)

Re-run of the S4/S5 cells of the main campaign after fixing the keep-alive close race
(2026-11-08). Same parameters as the campaign. "Before" is the campaign at 464cd1f;
"after" is the fixed `internal/handoff` drain. Full analysis: the "Keep-alive close
race" section of [docs/solutions/S5-fd-passing.md](../../docs/solutions/S5-fd-passing.md).
Per-cell numbers: [summary.csv](summary.csv), medians: [medians.txt](medians.txt).

## Errors (total over 5 runs per cell)

| Cell | Before | After |
|---|---|---|
| S4 E2 (21k req/s) | 37 | **0** |
| S4+migrate E2 | 58 | **0** |
| S5 E2 | 42 | **0** |
| S4+migrate E4-ka | 2 | **0** |
| S5+crash E6 | 1 | **0** |
| All other E1, E4-ka, E6 cells | 0 | 0 |

Totals after the fix: 70 runs and 6.38 M requests with **0 failed requests**, 0 duplicate
counts and 0 counter resets. The idle grace never had to close a connection
(`idle_closed` = 0 in every run).

## Root cause

At the seal the old process called `http.Server.SetKeepAlivesEnabled(false)`, repeated
every 2 ms during the drain. In Go 1.25 that call runs `closeIdleConns()`, which closes
every idle keep-alive connection at once. At 21k req/s some of them already carry a
request the client has just sent, so the client gets a reset or EOF with no response:
the HTTP/1.1 idle-close race (RFC 9112 §9.5). `http.Server.Shutdown` behaves the same way.
All 137 campaign failures were on reused connections, 9-12 ms after the trigger. Only a
`Connection: close` response (RFC 9112 §9.6) can tell the client to stop reusing a
connection, and `tcp_migrate_req` cannot help because these connections were already accepted.

## Fix

`internal/handoff/drain.go`, shared by S4 and S5:
- Nothing is closed at the seal.
- Every response written while draining carries `Connection: close`.
- Idle connections get a 1 s grace (`-idle-grace`) to be closed by their clients.
- `Shutdown` runs only once no connections remain.

B0 deliberately keeps stdlib `Shutdown`, because it is the naive baseline.

## Cost

The upgrade finishes 4-9 ms later (median) while the old process waits for clients to
close. The maximum latency in the E2 upgrade window rises by about 2-3 ms because late
requests are proxied to the successor, and p99 is essentially unchanged. S5 E2 "after"
contains outlier runs (upgrade up to 188 ms) taken while another container shared the
Docker VM. Read the medians, not the means (`medians.txt`, `docker_activity.log`).

## Validity notes

- The load generator makes every GET non-retryable on purpose, so Go's automatic retry on
  reused connections never hides a loss (every failure had `attempts=1`). This is the
  worst case, and it matches non-idempotent requests (RFC 9110 §9.2.2). A default Go
  client would have turned all 137 losses into extra latency.
- 35 runs taken while the Mac slept were discarded (their wall time was far above 20 s),
  and those cells were re-run under `caffeinate`. All 70 kept runs are within 0.3 s of the
  nominal duration. Raw data (`raw/`, `raw_invalid/`) is not committed because of its size.
- A residual race remains: a client reusing a connection that has been idle for at least
  1 s, at the exact moment the grace expires. It was never observed, so its rate is unmeasured.
