# dimpclass update report

* old: `testdata/cases/c2-interface-change/old`
* new: `testdata/cases/c2-interface-change/new`
* module: `example.com/c2-interface-change`

## Classification

**Dominant class: C2** (signature/added/removed functions or non-state type/global changes (call-graph closure))

Classes present: C1, C2

## Recommended mechanism: S5

S5 FD-passing process handoff with identity state transfer (new binary).

Justification:

* the change is interface-level but not S6-patchable (example.com/c2-interface-change/store.Store: interface method set changed (itabs and type assertions are compiled in)); state schema is unchanged, so S5 with identity transfer is the cheapest safe choice
* S6 blockers (1): example.com/c2-interface-change/store.Store: interface method set changed (itabs and type assertions are compiled in)

Alternatives:

* Kubernetes rolling update (loses in-memory state, needs surge capacity)

## Patch set (2 functions)

| function | reason | ctx | inlinable |
|---|---|---|---|
| `example.com/c2-interface-change/store.Logout` | body changed | yes | yes |
| `example.com/c2-interface-change/store.memStore.Delete` | added | yes | yes |

## Semantic diff

| class | kind | symbol | detail | S6 blocker |
|---|---|---|---|---|
| C1 | func-body | `example.com/c2-interface-change/store.Logout` | body changed (12 -> 10 tokens) |  |
| C2 | interface-changed | `example.com/c2-interface-change/store.Store` | +Delete(context.Context, string) | interface method set changed (itabs and type assertions are compiled in) |
| C2 | func-added | `example.com/c2-interface-change/store.memStore.Delete` | (*memStore)func(context.Context, string) |  |

Stats: packages 1 -> 1, functions 4 -> 5, changed functions 2, changed types 1, changed globals 0, type errors 0/0 (old/new).

