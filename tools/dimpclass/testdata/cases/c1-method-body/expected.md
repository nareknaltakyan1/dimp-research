# dimpclass update report

* old: `testdata/cases/c1-method-body/old`
* new: `testdata/cases/c1-method-body/new`
* module: `example.com/c1-method-body`

## Classification

**Dominant class: C1** (body-only change in patchable functions (constant changes patch their users))

Classes present: C1

## Recommended mechanism: S6

S6 transactional patch set of 1 function(s) (body-only change).

Justification:

* no state-schema, dependency, toolchain, goroutine-structure or long-running-code change
* every member of the patch set is an ordinary non-generic function or method, so each can be redirected through its slot, and the whole set is published in one atomic PatchSet store

Alternatives:

* S2 Go plugin swap of the affected handler (same toolchain; old code leaks)
* S1 epoch swap if v2 is compiled in alongside v1 (pre-staged release)
* S5 FD-passing handoff (identity state transfer) or rolling update as fallback

## Patch set (1 functions)

| function | reason | ctx | inlinable |
|---|---|---|---|
| `example.com/c1-method-body/cache.Cache.Get` | body changed | yes | no |

## Semantic diff

| class | kind | symbol | detail | S6 blocker |
|---|---|---|---|---|
| C1 | func-body | `example.com/c1-method-body/cache.Cache.Get` | body changed (46 -> 68 tokens) |  |

Stats: packages 1 -> 1, functions 3 -> 3, changed functions 1, changed types 0, changed globals 0, type errors 0/0 (old/new).

