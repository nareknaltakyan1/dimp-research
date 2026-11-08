# dimpclass update report

* old: `testdata/cases/c5-background-goroutine/old`
* new: `testdata/cases/c5-background-goroutine/new`
* module: `example.com/c5-background-goroutine`

## Classification

**Dominant class: C5** (long-running loop, goroutine-structure, main() or init() change (quiescence risk))

Classes present: C5

## Recommended mechanism: S5

S5 FD-passing process handoff with identity state transfer (new binary).

Justification:

* goroutine-structure, long-running or start-up code changed: in-process patching would leave old code running in goroutines that never return; a new process starts the new goroutine structure, and the old one is drained and exits
* S6 blockers (2): example.com/c5-background-goroutine/server.New: goroutine structure changed (spawned goroutines outlive the patch transaction); example.com/c5-background-goroutine/server.Server.janitor: new long-running code

Alternatives:

* Kubernetes rolling update (loses in-memory state, needs surge capacity)

Risks:

* quiescence: old activations of main(), init-started or long-running goroutines never return, so the old process is retired by exit, not by draining; set a drain deadline in the S5 upgrader

## Patch set (2 functions)

| function | reason | ctx | inlinable |
|---|---|---|---|
| `example.com/c5-background-goroutine/server.New` | body changed | yes | no |
| `example.com/c5-background-goroutine/server.Server.janitor` | added | yes | no |

## Semantic diff

| class | kind | symbol | detail | S6 blocker |
|---|---|---|---|---|
| C5 | goroutine-structure | `example.com/c5-background-goroutine/server.New` | go statements [] -> [s.janitor] | goroutine structure changed (spawned goroutines outlive the patch transaction) |
| C5 | long-running-added | `example.com/c5-background-goroutine/server.Server.janitor` | for {} loop with select | new long-running code |

Stats: packages 1 -> 1, functions 2 -> 3, changed functions 2, changed types 0, changed globals 0, type errors 0/0 (old/new).

