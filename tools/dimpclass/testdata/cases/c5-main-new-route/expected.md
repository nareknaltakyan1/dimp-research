# dimpclass update report

* old: `testdata/cases/c5-main-new-route/old`
* new: `testdata/cases/c5-main-new-route/new`
* module: `example.com/c5-main-new-route`

## Classification

**Dominant class: C5** (long-running loop, goroutine-structure, main() or init() change (quiescence risk))

Classes present: C2, C5

## Recommended mechanism: S5

S5 FD-passing process handoff with identity state transfer (new binary).

Justification:

* goroutine-structure, long-running or start-up code changed: in-process patching would leave old code running in goroutines that never return; a new process starts the new goroutine structure, and the old one is drained and exits
* S6 blockers (1): example.com/c5-main-new-route.main: main() runs once at start-up (never re-executed / never returns)

Alternatives:

* Kubernetes rolling update (loses in-memory state, needs surge capacity)

Risks:

* 1 patched functions are in package main; a Go plugin cannot import package main, so hotpatchgen must relocate them into an importable package
* quiescence: old activations of main(), init-started or long-running goroutines never return, so the old process is retired by exit, not by draining; set a drain deadline in the S5 upgrader

## Patch set (1 functions)

| function | reason | ctx | inlinable |
|---|---|---|---|
| `example.com/c5-main-new-route.version` | added | no | yes |

## Semantic diff

| class | kind | symbol | detail | S6 blocker |
|---|---|---|---|---|
| C2 | func-added | `example.com/c5-main-new-route.version` | func(net/http.ResponseWriter, *net/http.Request) |  |
| C5 | main-changed | `example.com/c5-main-new-route.main` | body changed (32 -> 41 tokens) | main() runs once at start-up (never re-executed / never returns) |

Stats: packages 1 -> 1, functions 2 -> 3, changed functions 2, changed types 0, changed globals 0, type errors 0/0 (old/new).

