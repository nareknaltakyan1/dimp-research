# dimpclass evaluation on real history

Generated 2026-11-08 by `tools/dimpclass/eval` (last 150 first-parent commits per target that touch Go sources, go.mod or go.sum).

## Dominant class per commit pair

| target | pairs | C0 | C1 | C2 | C3 | C4 | C5 | 
|---|---|---|---|---|---|---|---|
| chi | 150 | 50 (33.3%) | 42 (28.0%) | 31 (20.7%) | 5 (3.3%) | 5 (3.3%) | 17 (11.3%) | 
| gotify | 150 | 4 (2.7%) | 18 (12.0%) | 15 (10.0%) | 3 (2.0%) | 50 (33.3%) | 60 (40.0%) | 
| frontend | 150 | 0 (0.0%) | 2 (1.3%) | 2 (1.3%) | 0 (0.0%) | 136 (90.7%) | 10 (6.7%) | 
| ALL | 450 | 54 (12.0%) | 62 (13.8%) | 48 (10.7%) | 8 (1.8%) | 191 (42.4%) | 87 (19.3%) | 
| ALL (no bots) | 301 | 53 (17.6%) | 62 (20.6%) | 48 (15.9%) | 8 (2.7%) | 44 (14.6%) | 86 (28.6%) | 

## Classes present (multi-label; a commit counts once per class it contains)

| target | C0 | C1 | C2 | C3 | C4 | C5 | 
|---|---|---|---|---|---|---|
| chi | 76.0% | 45.3% | 26.0% | 3.3% | 3.3% | 12.7% | 
| gotify | 72.7% | 42.7% | 44.7% | 2.0% | 41.3% | 42.0% | 
| frontend | 94.0% | 6.7% | 7.3% | 0.0% | 94.7% | 6.7% | 
| ALL | 80.9% | 31.6% | 26.0% | 1.8% | 46.4% | 20.4% | 
| ALL (no bots) | 71.4% | 46.8% | 38.5% | 2.7% | 20.3% | 30.2% | 

## Recommended mechanism

| target | none | S6 | S3 | S5 | S5+xform | in-process (none/S6/S3) | median patch set (S6) | max patch set (S6) |
|---|---|---|---|---|---|---|---|---|
| chi | 33.3% | 42.0% | 0.0% | 21.3% | 3.3% | 75.3% | 1 | 10 |
| gotify | 2.7% | 17.3% | 0.0% | 78.0% | 2.0% | 20.0% | 1 | 9 |
| frontend | 0.0% | 2.7% | 0.0% | 97.3% | 0.0% | 2.7% | 5 | 7 |
| ALL | 12.0% | 20.7% | 0.0% | 65.6% | 1.8% | 32.7% | 1 | 10 |
| ALL (no bots) | 17.6% | 30.9% | 0.0% | 48.8% | 2.7% | 48.5% | 1 | 10 |

## Bucket view (thesis question)

* **in-process hot-patchable**: dominant class C0, C1 or C2 *and* mechanism none/S6/S3
* **C0-C2 but S6-blocked**: dominant C1/C2 with an S6 blocker (type layout, var initializer, generic) -> S5 identity handoff
* **state transfer**: dominant C3 -> S5 + transformer
* **new process**: dominant C4 or C5 -> S5 (or rolling update)

| target | pairs | in-process | C0-C2 S6-blocked | state transfer (C3) | new process (C4/C5) |
|---|---|---|---|---|---|
| chi | 150 | 113 (75.3%) | 10 (6.7%) | 5 (3.3%) | 22 (14.7%) |
| gotify | 150 | 30 (20.0%) | 7 (4.7%) | 3 (2.0%) | 110 (73.3%) |
| frontend | 150 | 4 (2.7%) | 0 (0.0%) | 0 (0.0%) | 146 (97.3%) |
| ALL | 450 | 147 (32.7%) | 17 (3.8%) | 8 (1.8%) | 278 (61.8%) |
| ALL (no bots) | 301 | 146 (48.5%) | 17 (5.6%) | 8 (2.7%) | 130 (43.2%) |

## Most frequent finding kinds (share of commits containing the kind)

| kind | commits |
|---|---|
| file-gosum | 199 (44.2%) |
| dep-version | 182 (40.4%) |
| file-test | 145 (32.2%) |
| func-body | 134 (29.8%) |
| func-added | 68 (15.1%) |
| startup-only-changed | 64 (14.2%) |
| dep-added | 34 (7.6%) |
| import-added | 33 (7.3%) |
| dep-removed | 27 (6.0%) |
| func-signature | 25 (5.6%) |
| struct-field-added | 25 (5.6%) |
| func-removed | 23 (5.1%) |
| closure-factory-changed | 22 (4.9%) |
| file-go-unbuilt | 21 (4.7%) |
| gomod-go | 19 (4.2%) |
| var-added | 19 (4.2%) |
| const-added | 18 (4.0%) |
| type-added | 18 (4.0%) |
| gomod-toolchain | 15 (3.3%) |
| main-changed | 13 (2.9%) |
| var-init | 12 (2.7%) |
| long-running-caller | 11 (2.4%) |
| struct-field-retyped | 11 (2.4%) |
| interface-changed | 10 (2.2%) |
| package-added | 7 (1.6%) |
| struct-tag-changed | 6 (1.3%) |
| var-removed | 5 (1.1%) |
| struct-field-reordered | 4 (0.9%) |
| package-removed | 3 (0.7%) |
| struct-field-removed | 3 (0.7%) |
| type-removed | 3 (0.7%) |
| const-removed | 2 (0.4%) |
| const-value | 2 (0.4%) |
| dep-replace | 2 (0.4%) |
| gomod-module | 2 (0.4%) |
| goroutine-structure | 2 (0.4%) |
| init-changed | 2 (0.4%) |
| long-running-changed | 2 (0.4%) |
| state-type-changed | 1 (0.2%) |
| type-changed | 1 (0.2%) |
| var-func-type | 1 (0.2%) |
| var-type | 1 (0.2%) |

Analysis time per pair (load + type-check both versions + classify): median 22 ms, p95 44 ms, max 1290 ms.
