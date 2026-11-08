# dimpclass update report

* old: `testdata/cases/c5-startup-wiring/old`
* new: `testdata/cases/c5-startup-wiring/new`
* module: `example.com/c5-startup-wiring`

## Classification

**Dominant class: C5** (long-running loop, goroutine-structure, main() or init() change (quiescence risk))

Classes present: C5

## Recommended mechanism: S5

S5 FD-passing process handoff with identity state transfer (new binary).

Justification:

* goroutine-structure, long-running or start-up code changed: in-process patching would leave old code running in goroutines that never return; a new process starts the new goroutine structure, and the old one is drained and exits
* S6 blockers (1): example.com/c5-startup-wiring.routes: start-up-only code already ran; its effect is baked into the live state, so patching it changes nothing

Alternatives:

* Kubernetes rolling update (loses in-memory state, needs surge capacity)

Risks:

* changed functions are small enough to be inlined; with S6 slot trampolines the call still dispatches through the slot, but raw binary patching (internal/livepatch) must also patch the inlining callers: example.com/c5-startup-wiring.main
* 1 patched functions are in package main; a Go plugin cannot import package main, so hotpatchgen must relocate them into an importable package
* quiescence: old activations of main(), init-started or long-running goroutines never return, so the old process is retired by exit, not by draining; set a drain deadline in the S5 upgrader

## Patch set (1 functions)

| function | reason | ctx | inlinable |
|---|---|---|---|
| `example.com/c5-startup-wiring.routes` | body changed | no | yes |

Inline-sensitive callers (needed only for raw binary patching): `example.com/c5-startup-wiring.main`

## Semantic diff

| class | kind | symbol | detail | S6 blocker |
|---|---|---|---|---|
| C5 | startup-only-changed | `example.com/c5-startup-wiring.routes` | only called (directly) from start-up code main()/init(); body changed (10 -> 19 tokens) | start-up-only code already ran; its effect is baked into the live state, so patching it changes nothing |

Stats: packages 1 -> 1, functions 3 -> 3, changed functions 1, changed types 0, changed globals 0, type errors 0/0 (old/new).

