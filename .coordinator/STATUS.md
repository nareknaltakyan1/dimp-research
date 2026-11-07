# Coordinator status (for resuming after usage-limit interruptions)

User goal: strong, novel solutions plus research for "Zero-Downtime Microservice Upgrades via
Dynamic In-Memory Binary Patching in Cloud-Native Environments". Implementation first,
with md docs describing each problem and solution, and paper/thesis ideas. If usage runs out,
wait for the reset and continue. The user is the supervisor; the student repo is Ruzanna20/dimp-poc.

## Done
- [x] Cloned and reviewed the student PoC: docs/dimp-poc-review.md
- [x] PR https://github.com/Ruzanna20/dimp-poc/pull/1 (per-version drain fix)
- [x] Repo https://github.com/nareknaltakyan1/dimp-research, docs/CONTRACT.md, docs/problem-statement.md

## Wave 1 (dispatched, background agents)
- [ ] Research scientist -> docs/literature-survey.md, docs/paper-ideas.md, paper/refs.bib
- [ ] Engineer A -> internal/hotswap, cmd/s1-epoch-swap, cmd/s2-plugin, cmd/s3-wasm, docs/solutions/S1-S3
- [ ] Engineer B -> internal/handoff, cmd/b0-naive-restart, cmd/s4-reuseport, cmd/s5-fdpass, docs/solutions/B0,S4,S5
- [ ] Benchmark engineer -> bench/, scripts/, deploy/, docs/evaluation-methodology.md, kind baseline

## Wave 2 (advanced / novel)
- [ ] S6 true in-memory machine-code patching of a running Go process (function prologue
      trampoline into code loaded at runtime) + stack-inspection safe update points (Linux, Docker).
      Owns internal/livepatch, cmd/s6-*, docs/solutions/S6*
- [ ] S7 Kubernetes-native in-place patch operator: HotPatch CRD, signed patch artifacts (ed25519),
      in-process canary (traffic split v1/v2 in one process), shadow/differential execution,
      SLO-based auto-rollback. Builds on internal/hotswap + s3-wasm (start AFTER Engineer A).
      Owns internal/canary, internal/signing, operator/, cmd/s7-*, docs/solutions/S7*
- [ ] S8 Adaptive mechanism selector: classify a patch (body-only / signature / state-schema change)
      and pick S1/S3/S6 vs S5 vs rolling update. Small; can be done by the lead or S7 agent.

## Wave 3
- [ ] Run the full experiment matrix (E1-E6) for B0, B1(kind), S1-S7 in Docker; results/ plus plots
- [ ] Code-review agent across all code; fix findings
- [ ] Lead: README, results chapter docs/results.md, paper draft outline paper/, update paper-ideas with real numbers
- [ ] Commit and push after each wave; final report to the user
