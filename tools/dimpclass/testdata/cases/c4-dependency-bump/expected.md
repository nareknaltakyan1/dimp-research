# dimpclass update report

* old: `testdata/cases/c4-dependency-bump/old`
* new: `testdata/cases/c4-dependency-bump/new`
* module: `example.com/c4-dependency-bump`

## Classification

**Dominant class: C4** (dependency, toolchain, native code or embedded-asset change (new binary))

Classes present: C0, C4

## Recommended mechanism: S5

S5 FD-passing process handoff with identity state transfer (new binary).

Justification:

* dependency/toolchain/runtime/native/embedded change needs a new binary; S5 keeps the listening socket and transfers state unchanged (state schema identical)
* S6 blockers (1): github.com/google/uuid: dependency version changed

Alternatives:

* Kubernetes rolling update (loses in-memory state, needs surge capacity)

Risks:

* type information is partial (1/1 type errors old/new; third-party packages are opaque): call-graph and state reachability are lower bounds

## Patch set (0 functions)

## Semantic diff

| class | kind | symbol | detail | S6 blocker |
|---|---|---|---|---|
| C0 | file-gosum | `go.sum` | modified (checksums only; the build list is determined by go.mod) |  |
| C4 | dep-version | `github.com/google/uuid` | v1.3.0 -> v1.6.0 | dependency version changed |

Stats: packages 1 -> 1, functions 1 -> 1, changed functions 0, changed types 0, changed globals 0, type errors 1/1 (old/new).

