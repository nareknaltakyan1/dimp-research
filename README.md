# DIMP Research: Zero-Downtime Microservice Upgrades via Dynamic In-Memory Patching

Research codebase for the topic **"Zero-Downtime Microservice Upgrades via Dynamic
In-Memory Binary Patching in Cloud-Native Environments"**. It contains eight upgrade
mechanisms for Go microservices, a common benchmark harness, a Kubernetes operator,
a program-analysis tool, a literature survey of 100 verified references, and paper and
thesis ideas backed by measured data.

It grew out of the student PoC [Ruzanna20/dimp-poc](https://github.com/Ruzanna20/dimp-poc)
([review](docs/dimp-poc-review.md), [fix PR #1](https://github.com/Ruzanna20/dimp-poc/pull/1)).

## Start here

| Document | What it is |
|---|---|
| [docs/problem-statement.md](docs/problem-statement.md) | Research problem, sub-problems P1-P8, scope |
| [docs/literature-survey.md](docs/literature-survey.md) | DSU taxonomy, comparison, research gaps G1-G10 ([refs.bib](paper/refs.bib)) |
| [docs/paper-ideas.md](docs/paper-ideas.md) | 12 paper ideas (9-12 backed by our results), recommended thesis structure |
| [docs/CONTRACT.md](docs/CONTRACT.md) | Common service and upgrade contract all solutions implement |
| [docs/evaluation-methodology.md](docs/evaluation-methodology.md) | RQ1-RQ6, metrics, statistics, threats to validity |
| [docs/results.md](docs/results.md) | Results chapter (full experiment matrix) |

## Solutions

| ID | Mechanism | Family | Doc | Headline result |
|---|---|---|---|---|
| B0 | Naive stop-and-restart | baseline | [B0](docs/solutions/B0-naive-restart.md) | ~130 refused connections per upgrade, state lost |
| B1 | Kubernetes rolling update (kind) | baseline | [methodology](docs/evaluation-methodology.md) | 0-1 errors with preStop, but in-memory state reset |
| S1 | Atomic swap + per-version epoch draining | in-process | [S1](docs/solutions/S1-epoch-swap.md) | old version drains in ~0.7 ms under load (global counter never drains) |
| S2 | Go `plugin` (.so) loading | in-process | [S2](docs/solutions/S2-go-plugin.md) | real dynamic code, but +5.3 MiB never reclaimed per upgrade |
| S3 | WebAssembly modules (wazero) | in-process | [S3](docs/solutions/S3-wasm.md) | dynamic code with memory reclaimed (flat over 20 upgrades) |
| S4 | New process + `SO_REUSEPORT` | process | [S4](docs/solutions/S4-so-reuseport.md) | 0 failed requests after the keep-alive drain fix (was 37 at 21k req/s) |
| S5 | Listener FD passing (SCM_RIGHTS) + state transfer | process | [S5](docs/solutions/S5-fd-passing.md) | 0 failed requests in all re-runs (6.38M requests); found and fixed the Go keep-alive close race |
| S6 | Hotpatch-ready compilation + transactional patch sets | in-process | [S6](docs/solutions/S6-hotpatch-ready-compilation.md) | 0 mixed-version requests; patch 100 functions in 110 µs |
| S7 | Kubernetes `HotPatch` operator, signed patches, in-process canary | cloud-native | [S7](docs/solutions/S7-k8s-inplace-patch-operator.md) | in-place fleet patch with 0 errors and 0 restarts; bad patch caught in shadow; replay and unauthenticated admin rejected |
| S8 | Update classifier and mechanism selector | analysis | [S8](docs/solutions/S8-update-classifier.md) | 48.5% of real non-bot Go commits hot-patchable in-process |

## Repository layout

```
cmd/            one contract-compliant server per solution (+ s7-patchctl, s7-load)
internal/       hotswap (S1-S3), handoff (S4-S5), patchable (S6), canary + signing (S7)
                livepatch (quarantined raw machine-code attempt, build tag livepatch_raw)
tools/          hotpatchgen (S6 source transformer), dimpclass (S8 classifier)
operator/       Kubernetes operator (separate Go module)
bench/          open-loop load generator, mock server, RSS monitor, Python analysis
scripts/        experiment runner, matrix, calibration, kind baselines, S7 demo
deploy/         Dockerfiles, kind/k8s manifests
results/        summaries and plots (raw data is gitignored)
paper/          bibliography
```

## Quick start

Requirements: Go 1.24+, Docker. Linux behaviour (SO_REUSEPORT, FD passing) is tested in Docker.

```bash
go test -race ./internal/...                     # unit and race tests (macOS or Linux)

# smoke test one solution
go run ./cmd/s1-epoch-swap &                     # serves :8080
curl -s localhost:8080/api; curl -s -XPOST 'localhost:8080/admin/upgrade?to=v2'; curl -s localhost:8080/api

# rigorous experiment in a Linux container (pinned CPUs)
REPS=5 scripts/in_docker.sh scripts/run_experiment.sh s5-fdpass E1
scripts/in_docker.sh python3 bench/analysis/analyze.py

# Kubernetes operator demo on kind
scripts/s7_kind_demo.sh
```

## Status

Stage 1 (implementation, a 311-run campaign, code review and fixes) is complete. Headline: S1-S3, S6 and S7 had zero failed requests in every experiment; S4/S5 reached zero in all re-run cells after the keep-alive fix (S4's repeated-upgrade E5 not yet re-run); and only in-process and handoff mechanisms preserved in-memory state (Kubernetes rolling updates reset it). See [docs/results.md](docs/results.md) section 8 for the post-review re-runs. See [.coordinator/STATUS.md](.coordinator/STATUS.md)
for the work log and remaining items. The testbed is a single Apple M4 Pro laptop with Docker Desktop; see the
threats to validity in each document.
