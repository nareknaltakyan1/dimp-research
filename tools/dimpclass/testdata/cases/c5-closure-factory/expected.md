# dimpclass update report

* old: `testdata/cases/c5-closure-factory/old`
* new: `testdata/cases/c5-closure-factory/new`
* module: `example.com/c5-closure-factory`

## Classification

**Dominant class: C5** (long-running loop, goroutine-structure, main() or init() change (quiescence risk))

Classes present: C5

## Recommended mechanism: S5

S5 FD-passing process handoff with identity state transfer (new binary).

Justification:

* goroutine-structure, long-running or start-up code changed: in-process patching would leave old code running in goroutines that never return; a new process starts the new goroutine structure, and the old one is drained and exits
* S6 blockers (1): example.com/c5-closure-factory/mw.RequireToken: closures created before the patch (e.g. middleware chains built at start-up) keep the old code; only newly created closures see the fix

Alternatives:

* Kubernetes rolling update (loses in-memory state, needs surge capacity)

Risks:

* quiescence: old activations of main(), init-started or long-running goroutines never return, so the old process is retired by exit, not by draining; set a drain deadline in the S5 upgrader

## Patch set (1 functions)

| function | reason | ctx | inlinable |
|---|---|---|---|
| `example.com/c5-closure-factory/mw.RequireToken` | body changed | no | no |

## Semantic diff

| class | kind | symbol | detail | S6 blocker |
|---|---|---|---|---|
| C5 | closure-factory-changed | `example.com/c5-closure-factory/mw.RequireToken` | change is confined to the function literal(s) this factory returns | closures created before the patch (e.g. middleware chains built at start-up) keep the old code; only newly created closures see the fix |

Stats: packages 1 -> 1, functions 1 -> 1, changed functions 1, changed types 0, changed globals 0, type errors 0/0 (old/new).

