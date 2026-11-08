# dimpclass update report

* old: `testdata/cases/c2-signature-change/old`
* new: `testdata/cases/c2-signature-change/new`
* module: `example.com/c2-signature-change`

## Classification

**Dominant class: C2** (signature/added/removed functions or non-state type/global changes (call-graph closure))

Classes present: C1, C2

## Recommended mechanism: S6

S6 transactional patch set of 4 function(s) (interface-level (signature/added/removed functions) with call-graph closure change).

Justification:

* no state-schema, dependency, toolchain, goroutine-structure or long-running-code change
* every member of the patch set is an ordinary non-generic function or method, so each can be redirected through its slot, and the whole set is published in one atomic PatchSet store
* signature changes and removals were closed over their callers; new functions and types live in the patch plugin

Alternatives:

* S2 Go plugin swap of the affected handler (same toolchain; old code leaks)
* S1 epoch swap if v2 is compiled in alongside v1 (pre-staged release)
* S5 FD-passing handoff (identity state transfer) or rolling update as fallback

Risks:

* 1 of 4 patched functions have no context.Context parameter: they get per-call, not per-request, version consistency under S6

## Patch set (4 functions)

| function | reason | ctx | inlinable |
|---|---|---|---|
| `example.com/c2-signature-change/pricing.Quote` | body changed; calls lookupPrice whose signature changed | yes | yes |
| `example.com/c2-signature-change/pricing.Single` | body changed; calls lookupPrice whose signature changed | yes | yes |
| `example.com/c2-signature-change/pricing.convert` | added | no | yes |
| `example.com/c2-signature-change/pricing.lookupPrice` | signature changed | yes | yes |

## Semantic diff

| class | kind | symbol | detail | S6 blocker |
|---|---|---|---|---|
| C1 | func-body | `example.com/c2-signature-change/pricing.Quote` | body changed (43 -> 47 tokens) |  |
| C1 | func-body | `example.com/c2-signature-change/pricing.Single` | body changed (7 -> 11 tokens) |  |
| C2 | func-added | `example.com/c2-signature-change/pricing.convert` | func(int, string) int |  |
| C2 | func-signature | `example.com/c2-signature-change/pricing.lookupPrice` | func(string) (int, error) -> func(context.Context, string, string) (int, error) |  |

Stats: packages 1 -> 1, functions 3 -> 4, changed functions 4, changed types 0, changed globals 0, type errors 0/0 (old/new).

