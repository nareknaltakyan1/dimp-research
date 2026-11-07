# Review of the Original PoC (`Ruzanna20/dimp-poc`)

Commit reviewed: `0313133` ("DIMP PoC implementation with atomic pointer and in-flight counter"), a single `main.go` of 63 lines.

## What it does

* Two handlers `v1` (sleeps 500 ms) and `v2` are compiled into the binary.
* The active handler is stored in `atomic.Pointer[HandleFunc]`, and each
  request does `Load()` and calls it.
* After a fixed 5 s it calls `Store(&v2)`, then polls a **global**
  `inFlightCount` until it reaches 0, and prints that v1 can be collected.

## What is good

* The atomic pointer is the right primitive for an RCU-style swap. Each
  request sees exactly one version, and switch-over is a single atomic store.
* It names the right second problem, knowing when the old version is idle
  (quiescence), which many hot-swap demos skip.

## Issues

| # | Severity | Issue | Consequence |
|---|---|---|---|
| 1 | High (research framing) | v2 is compiled in. Nothing is loaded dynamically, and no binary is patched. | This is feature-flag switching, not "dynamic in-memory binary patching". It cannot deliver code that was unknown at build time. |
| 2 | High (correctness) | `inFlightCount` is global and also counts v2 requests. | Under steady load it may never reach 0, so the drain never completes. When it does, it does not prove v1 is idle. |
| 3 | Medium (race) | A request can `Load()` v1, then the drain loop observes 0, then the request increments and runs v1. | v1 can run after it was declared drained, which is unsafe if v1's resources are then freed. |
| 4 | Medium | No state, no state transfer. | The hardest part of DSU (P4) is not exercised. |
| 5 | Low | `ListenAndServe` error ignored, sleep-based timing, no trigger API, no rollback, no measurement. | Not usable for experiments. |

## Fixes delivered

* **PR [Ruzanna20/dimp-poc#1](https://github.com/Ruzanna20/dimp-poc/pull/1)**
  is a minimal, same-style fix for #2, #3 and #5. Each version has its own
  counter, `acquire()` uses increment-then-recheck, and the drain has a timeout.
* This repository goes further. S1 generalises the fix into the
  `internal/hotswap` library (state hand-over, rollback, benchmarks). S2 and S3
  address #1 with genuinely dynamic code loading. S4 and S5 explore the
  process-level alternative.
