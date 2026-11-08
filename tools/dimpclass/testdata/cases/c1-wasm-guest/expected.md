# dimpclass update report

* old: `testdata/cases/c1-wasm-guest/old`
* new: `testdata/cases/c1-wasm-guest/new`
* module: `example.com/c1-wasm-guest`

## Classification

**Dominant class: C1** (body-only change in patchable functions (constant changes patch their users))

Classes present: C1

## Recommended mechanism: S3

Swap the Wasm guest module (S3): 1 changed guest function(s), host unchanged.

Justification:

* every code change is inside a wasip1 guest package; S3 instantiates the new module (re-running its initializers) while the host keeps the state, and drains the old module by reference count

Alternatives:

* S5 handoff if the host ABI (imports/exports) changed
* rolling update

Risks:

* guest-internal state is not migrated by S3; any change to //go:wasmexport signatures or host imports changes the host ABI

## Patch set (1 functions)

| function | reason | ctx | inlinable |
|---|---|---|---|
| `example.com/c1-wasm-guest/guest.discount` | body changed | no | yes |

## Semantic diff

| class | kind | symbol | detail | S6 blocker |
|---|---|---|---|---|
| C1 | func-body | `example.com/c1-wasm-guest/guest.discount` | body changed (4 -> 17 tokens) |  |

Stats: packages 1 -> 1, functions 2 -> 2, changed functions 1, changed types 0, changed globals 0, type errors 0/0 (old/new).

