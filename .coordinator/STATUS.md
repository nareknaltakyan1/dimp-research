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
- [x] Research scientist (100 verified refs, 10 gaps G1-G10, 8 ideas; TODO lead: add Idea 9 = transactional patch sets from S6 once results exist) -> docs/literature-survey.md, docs/paper-ideas.md, paper/refs.bib
- [x] Engineer A (committed; drain 0.7ms vs global-counter timeout; plugin +5.3MiB/upgrade leak, WASM flat) -> internal/hotswap, cmd/s1-epoch-swap, cmd/s2-plugin, cmd/s3-wasm, docs/solutions/S1-S3
- [x] Engineer B (committed; S5 0 errors; finding: http.Server.Shutdown drops accepted-unread conns) -> internal/handoff, cmd/b0-naive-restart, cmd/s4-reuseport, cmd/s5-fdpass, docs/solutions/B0,S4,S5
- [x] Benchmark engineer (committed; validated on mock + kind)  -> bench/, scripts/, deploy/, docs/evaluation-methodology.md, kind baseline

## Wave 2 (advanced / novel)
NOTE 2026-11-08 05:37: S6, S7 and S8 were cut off by the 5h usage limit (reset 04:40) and were resumed via SendMessage with their context intact. If they die again, resume the same agents rather than re-dispatching.
- [~] S6 RE-SCOPED: the raw machine-code patching attempt stopped (part of its output was safety-filtered);
      internal/livepatch is quarantined behind build tag `livepatch_raw` (do NOT resume raw code-page rewriting).
      New S6 = hotpatch-ready compilation: tools/hotpatchgen (AST transformer that makes functions patchable),
      internal/patchable (slots, transactional patch sets, quiescence), cmd/s6-hotpatch, signed plugin delivery.
      Agent dispatched. Owns tools/hotpatchgen, internal/patchable, cmd/s6-*, docs/solutions/S6*
- [~] S7 DISPATCHED (agent D; kind cluster 'dimp-s7'; operator/ is a separate go module) Kubernetes-native in-place patch operator: HotPatch CRD, signed patch artifacts (ed25519),
      in-process canary (traffic split v1/v2 in one process), shadow/differential execution,
      SLO-based auto-rollback. Builds on internal/hotswap + s3-wasm (start AFTER Engineer A).
      Owns internal/canary, internal/signing, operator/, cmd/s7-*, docs/solutions/S7*
- [~] S8 DISPATCHED (agent E; tools/dimpclass + real-history eval of commit classes) Adaptive mechanism selector: classify a patch (body-only / signature / state-schema change)
      and pick S1/S3/S6 vs S5 vs rolling update. Small; can be done by the lead or S7 agent.

## Wave 3
- [ ] Run the full experiment matrix ONLY when no other agent uses Docker (noisy-neighbour warning).
      Needs per-solution cmd/<sol>/bench.env (SERVER_ARGS, TRIGGER, FAIL_TARGET) + build.sh; contract gaps:
      E5 needs upgrade back to v1 (alternate v1/v2), E6 needs a per-solution failure target (default to=v2-broken).
      Run (E1-E6) for B0, B1(kind), S1-S7 in Docker; results/ plus plots
- [ ] Code-review agent across all code; fix findings
- [ ] Lead: README, results chapter docs/results.md, paper draft outline paper/, update paper-ideas with real numbers
- [ ] Commit and push after each wave; final report to the user
