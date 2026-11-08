# Paper and Thesis Ideas

*Grounded in the research gaps G1-G10 of `docs/literature-survey.md`. BibTeX keys refer to `paper/refs.bib`.*
*Every experiment is designed to run on a laptop with Docker, `kind` (Kubernetes-in-Docker) and Go. Linux measurements run in the `golang:1.25` container, following `docs/CONTRACT.md`.*

**Venue notes.** The venues below are real and recurring. Deadlines and acceptance rates change every year, so check the current call for papers before planning around one. "Student / regional" options are for a first publication or a thesis spin-off.

---

## Idea 1 (required a). In-Process or Out-of-Process? An Empirical Comparison of Zero-Downtime Upgrade Mechanisms for Go Microservices

**Gaps addressed:** G2 (primary), G10, G1.

**Research questions**
* RQ1: How do in-process mechanisms (S1 atomic pointer + epochs, S2 Go plugins, S3 wazero modules) compare with process-level mechanisms (S4 SO_REUSEPORT, S5 FD passing / re-exec with state transfer) and with an orchestrator baseline (B0 Kubernetes rolling update) on failed requests, tail latency during the upgrade, time-to-new-version, time-to-drain, memory overhead and state continuity?
* RQ2: How do these metrics change with request duration (`work_ms`), offered load, concurrency and state size?
* RQ3: What are the operational costs of each mechanism: build constraints, platform limits, how many upgrades are possible before a restart is forced (plugin leakage), and lines of code?

**Hypotheses**
* H1: In-process swap (S1) gives the lowest p99 latency disturbance and zero failed requests, and the smallest memory overhead.
* H2: Plain SO_REUSEPORT re-binding (S4 without `tcp_migrate_req`) shows a non-zero failure rate under load because of accept-queue loss [`tarreau2017haproxy`; `kerrisk2013reuseport`; `iwashima2021migrate`]. FD passing (S5) removes it.
* H3: Memory under S2 (Go plugins) grows monotonically with the number of upgrades because plugins cannot be unloaded [`go-issue-20461`].
* H4: B0 (rolling update) keeps service-level availability, but shows the largest p99.9 spike and loses all in-process state.

**Method and experiments**
* Use the shared workload from `docs/CONTRACT.md` (stateful counter, `X-Version`, upgrade event log).
* Use an open-loop load generator (constant arrival rate, to avoid coordinated omission), at load levels of 30%, 60% and 90% of measured capacity, and `work_ms` in {0, 10, 100, 1000}.
* Run 30 or more repetitions per configuration, report medians with bootstrap confidence intervals, and follow the reporting principles of Papadopoulos et al. [`papadopoulos2021methodological`]. Report p50, p99 and p99.9 in sliding windows around `upgrade_start` [`dean2013tail`].
* Run on two platforms: Docker on a single host (mechanism-level), and a three-node `kind` cluster (orchestration-level, with Service/EndpointSlice routing).
* Measure state continuity by checking that the `count` sequence is monotonic with no gaps.
* Run repeated-upgrade experiments (1, 10, 100 upgrades) to expose leaks.

**Expected contribution.** The first controlled, reproducible comparison across the in-process / process-level / orchestrator spectrum for Go, with an open benchmark harness and dataset, and practical guidance on which mechanism to use when.

**Venues.** ACM/SPEC ICPE (research or experience track); IEEE CLOUD; ACM Middleware (industry or experience track); ACM SoCC (poster or short paper); IEEE IC2E; JSS or the Empirical Software Engineering journal (extended version). Student / regional: ICPE companion workshops, the Middleware Doctoral Symposium, arXiv preprint.

**Risk.** Low to medium. The main risk is measurement noise on a laptop (mitigate with CPU pinning, repetition and reporting variance) and "obvious result" criticism (mitigate with the repeated-upgrade, state-size and kernel-feature (`tcp_migrate_req`) dimensions).

**Fit.** An excellent master's thesis core. Also one solid conference paper.

---

## Idea 2 (required b). Draining Never Ends: Version-Aware Epoch-Based Quiescence for Live Upgrades of Request-Driven Services

**Gaps addressed:** G3 (primary), G1.

**Research questions**
* RQ1: Under what arrival processes does global in-flight-counter quiescence (the student's proof of concept) fail to terminate, and how long does it take when it does terminate?
* RQ2: Does per-version epoch draining (requests pinned to the epoch they were admitted under, at most two live epochs) guarantee termination with a drain time bounded by the longest in-flight request, independent of load?
* RQ3: What is the steady-state cost of epoch tracking (atomic counters, sharded counters, or an RCU-like scheme) at high request rates and core counts?

**Hypothesis.** The global counter's drain time grows without bound as utilisation approaches 1 (it never drains when requests overlap continuously). Per-epoch draining finishes in at most max(in-flight duration) plus a small ε, regardless of utilisation, at under 1% throughput overhead.

**Method and experiments**
* **Model.** Formalise both protocols in TLA+ (or PlusCal). Model-check safety ("no request observes a mixed version", which is version consistency at request granularity [`ma2011versionconsistent`]) and liveness ("the old epoch eventually drains" under weak fairness). Show a counterexample trace for the global counter.
* **Analysis.** Relate the protocol to Kramer and Magee's quiescence [`kramer1990evolving`; `kramer2025quiescence`], to tranquillity [`vandewoude2007tranquility`], to RCU grace periods [`guniguntala2008rcu`] and to epoch-based reclamation [`hart2007reclamation`]. Derive the drain-time bound.
* **Implementation.** In S1 (`internal/hotswap`), compare four variants: a single global counter, per-version atomic counters, sharded per-CPU counters, and `sync.WaitGroup` per epoch.
* **Experiments.** Measure drain time against utilisation (Poisson and bursty arrivals, heavy-tailed `work_ms`). Run micro-benchmarks of the admit/release cost at 1 to 16 cores. Handle long-lived requests (streaming, WebSockets) with a deadline and forced-cancel policy, and measure what it does to errors.

**Expected contribution.** A small, rigorous result: a verified draining protocol for Go (and any request-driven service), with a liveness bound and a practical implementation. It is the request-level counterpart of Erlang's two-version rule [`erlang-codeloading`] and of the kernel livepatch per-task model [`linux-livepatch`].

**Venues.** ICSE NIER or ICSME (short or new-ideas track); ACM Middleware; DSN (short); IEEE/IFIP DSN workshops; ICSA (short). Formal-methods angle: an iFM or FormaliSE short paper. Student / regional: the ICSE SRC (Student Research Competition), the HotSWUp-style workshop if it is revived, arXiv.

**Risk.** Low. It is small and self-contained. The novelty risk is that it is "just RCU", so the contribution must be framed as *applying and verifying* RCU/epochs for live upgrades with a load-independent drain bound and an empirical demonstration of the failure mode.

**Fit.** An ideal first paper for the student, and a strong thesis chapter.

---

## Idea 3 (required c). Did We Lose a Request? Correctness Criteria and Systematic Testing of State Transfer in Live Process Handoff

**Gaps addressed:** G4 (primary), G7, G10.

**Research questions**
* RQ1: What correctness criteria should a live handoff with application-state transfer satisfy? Candidates: no lost updates, no duplicated updates, monotonic observable state, exactly-once effects for in-flight requests, and atomic visibility of the switch.
* RQ2: Which handoff designs violate which criteria, and under what interleavings? Designs to compare: snapshot-then-switch, switch-then-snapshot, freeze-and-transfer, and dual-write / forwarding.
* RQ3: Can systematic interleaving or fault injection at handoff points find these violations automatically?

**Hypothesis.** Naive "serialize state, then pass FDs" designs lose or duplicate updates made between the snapshot and the moment the old process stops accepting work. A two-phase protocol (stop admitting → drain → snapshot → transfer → resume), or a forwarding window, removes these anomalies, at a measurable latency cost.

**Method and experiments**
* Define the criteria as trace properties over the contract's `count` sequence and the upgrade event log, adapting Hayden et al.'s trace-relation view of update correctness [`hayden2012specifying`].
* Build a test harness in the spirit of Tedsuto [`pina2016tedsuto`] and systematic DSU testing [`hayden2012testing`]. Inject delays, crashes and SIGKILLs at each handoff step of S5 (in Docker), and check the properties.
* Compare three state-transfer channels: a UNIX socket alongside the FDs, a shared memory-mapped file, and an external store (Redis) as a baseline.
* Optionally, compare with CRIU same-version restore [`criu`] as a "perfect state" reference.

**Expected contribution.** A precise correctness definition for state-preserving hot restart, a catalogue of anomalies found in realistic designs, and an open testing tool. It complements the transfer of sockets only (Envoy, tableflip [`envoy-hotrestart`; `tableflip`]) and partial request replay (ZDR [`naseer2020zdr`]).

**Venues.** ICST; ISSTA; ICSME; ACM Middleware; DSN; EuroSys (if a strong systems angle develops). Student / regional: the ICST doctoral symposium, the ICSE SRC, arXiv.

**Risk.** Medium. The scope can grow without limit, so restrict it to one workload with counter or key-value state, and one or two transfer channels.

**Fit.** Good thesis chapter. A paper if the anomaly catalogue is convincing.

---

## Idea 4 (required d). Upgrade Without Replacement: A Kubernetes Operator for Signed In-Process Hot Patching of Microservices

**Gaps addressed:** G5 and G6 (primary), G7.

**Research questions**
* RQ1: Can a Kubernetes-native controller (a CRD such as `HotPatch` plus an operator or sidecar) trigger in-process or in-pod upgrades (S1/S3/S5) instead of pod replacement, while keeping readiness, observability, rollout history and rollback semantics?
* RQ2: How does it compare with a rolling update [`k8s-deployment`] and with OpenKruise in-place image update [`openkruise-inplace`] in upgrade duration, resource surge, failed requests, p99 latency and state preservation?
* RQ3: What is the threat model of runtime patch endpoints, and can provenance (signatures plus in-toto-style attestations) and admission policies close it at acceptable cost?

**Hypotheses**
* H1: Operator-driven in-process upgrades finish faster than a rolling update (no scheduling, image pull or warm-up), need no surge capacity, and preserve state.
* H2: Signature and attestation verification before loading adds latency in the tens of milliseconds, negligible compared with pod replacement.
* H3: Without such controls, a compromised workload or service account can load arbitrary code through the upgrade endpoint, so in-process patching enlarges the attack surface unless patch loading is bound to verified provenance.

**Method and experiments**
* **Design.**
  * A CRD declares the target version, the patch artifact (Wasm module or plugin) and its signature. Patches are artifacts in an OCI registry signed with Sigstore-style keys [`newman2022sigstore`], with an optional TUF-style trusted-metadata model [`samuel2010tuf`] and in-toto layout [`torresarias2019intoto`].
  * The operator orders pods, calls `/admin/upgrade`, watches the `upgrade_done` / `upgrade_failed` events, and uses readiness gates or a custom pod condition to expose "running version".
  * Rollout follows PodDisruptionBudget-like batching. Rollback works by re-patching to the previous version.
  * Mixed-version routing across replicas is discussed using Chait-Roth et al. [`chaitroth2026consistent`].
* **Security analysis.** STRIDE threat model of the patch path, informed by KShot's protected patch path [`zhou2020kshot`]. Implement verification inside the pod (for Wasm modules) and measure the cost. Run attack experiments: unsigned patch, replayed old patch (rollback attack), patch for the wrong target.
* **Experiments.** In `kind` with 3 to 50 replicas: upgrade duration, CPU and memory surge, failed requests, p99, and state continuity, compared with a rolling update and OpenKruise.
* **Observability.** Report the "image digest ≠ running code" problem and propose exporting a running-code digest as a pod annotation or metric.

**Expected contribution.** The first Kubernetes-native design and evaluation of in-process hot patching with supply-chain-grade provenance. It would extend in-place pod resize [`kep1287`] from resources to code.

**Venues.** ACM SoCC; IEEE CLOUD; ACM Middleware; IEEE IC2E; USENIX ATC (if the evaluation is strong); security angle: ACM CCSW (Cloud Computing Security Workshop), ACSAC, or the SCORED workshop (software supply chain). Practitioner reach: a KubeCon talk or an arXiv tech report. Student / regional: the Middleware Doctoral Symposium, IEEE CLOUD workshops.

**Risk.** Medium to high: engineering effort, plus reviewers asking "why not just roll". Mitigate by quantifying when replacement is costly: large in-memory state, warm caches, long-lived connections, and resource-tight clusters with no surge.

**Fit.** A strong master's thesis capstone. A paper if the security part is rigorous.

---

## Idea 5. WebAssembly as a Dynamic Software Updating Substrate for Go Services

**Gaps addressed:** G8 (primary), G1.

**Research questions**
* RQ1: What are the swap latency, steady-state overhead and memory-reclamation behaviour of hot-swapping business logic compiled to Wasm and hosted in a Go service through wazero [`wazero`], compared with native Go (S1) and Go plugins (S2)?
* RQ2: How should state cross the module boundary? Options: host-held state, explicit export/import functions, or Component-Model-typed interfaces [`wasm-component-model`]. What is the cost of each?
* RQ3: Does sandboxing make rollback safer, for example by containing a faulty new version?

**Hypothesis.** Wasm swap reclaims old versions (unlike plugins), has sub-millisecond instantiation for small modules, and costs a measurable but acceptable steady-state slowdown compared with native Go for CPU-light request handlers.

**Method.** Micro-benchmarks and the shared contract workload. Repeated-upgrade memory curves for S2 against S3. Fault-injection comparison (panic / trap in v2). Ground the work in the Wasm design properties [`haas2017wasm`] and in DSU state-transfer theory [`giuffrida2012statetransfer`; `magill2012tos`].

**Expected contribution.** The first peer-reviewed characterisation of Wasm-based DSU in server software.

**Venues.** ACM/SPEC ICPE; ACM Middleware; MPLR (Managed Programming Languages and Runtimes); VEE; the WebAssembly research workshop co-located with PLDI or ASPLOS, when it is held. Student / regional: arXiv, Middleware workshops.

**Risk.** Low to medium. The tooling exists, and the risk is mainly that the results are "incremental".

**Fit.** A good thesis chapter that pairs naturally with Idea 1.

---

## Idea 6. Shadow-Then-Commit: Lightweight Multi-Version Validation for In-Process Microservice Upgrades

**Gaps addressed:** G7 (primary), G5.

**Research questions**
* RQ1: Can an in-process upgrade mechanism mirror a sample of live requests to v2 (side-effect-free or sandboxed), compare responses with declared expected divergences, and commit or roll back automatically, as Mvedsua does [`pina2019mvedsua`] but at the HTTP/handler level in Go?
* RQ2: What is the overhead as a function of the sampling rate, and what is the detection rate for injected regressions (mutation testing)?

**Hypothesis.** Sampling 1-5% of requests detects most injected behavioural regressions within seconds, at under 5% overhead. That would make in-process upgrades as safe as canary releases, without pod churn.

**Method.** Implement on top of S1/S3. Use mutation operators on v2. Compare with Istio-based canary traffic shifting [`istio-trafficshifting`] in `kind`. Ground the work in MVE [`hosek2013mx`; `hosek2015varan`] and DSU testing [`pina2016tedsuto`].

**Venues.** ICSE (SEIP track), FSE, ICST, ISSRE, ACM Middleware. Student / regional: the ICSE SRC, arXiv.

**Risk.** Medium. Side effects (writes) make mirroring hard, so restrict it to read-only endpoints or sandboxed state (Wasm) for the main results.

**Fit.** A paper or a thesis extension.

---

## Idea 7. Towards LLM-Assisted Synthesis and Checking of State Transformers for Microservice Upgrades (exploratory)

**Gaps addressed:** G9.

**Research questions.** Given two versions of a Go service and its state types, can an LLM propose a state-transfer function, and can property-based testing against the criteria of Idea 3 reliably reject wrong ones? How does this compare with rule-based synthesis in the style of Magill et al. [`magill2012tos`]?

**Method.** Build a small corpus of version pairs with state-schema changes (field added, renamed, type changed, split). Generate transformers, check them with property-based tests and the Idea 3 harness, and measure precision and recall of the checker.

**Venues.** ICSE NIER, the FSE Ideas/Visions track, the LLM4Code workshop (ICSE), arXiv.

**Risk.** High. The results may be negative, and the area moves fast. Keep it exploratory.

**Fit.** An optional "future work" chapter or a workshop paper. Not a thesis core.

---

## Idea 8. ZDU-Bench: An Open Benchmark and Metric Suite for Zero-Downtime Upgrades

**Gaps addressed:** G10, G2.

A reusable artifact: the shared workload contract, an open-loop load generator, an upgrade-event schema, analysis scripts, and metric definitions. The metrics are *failed-request count*, *refused-connection count*, *upgrade disturbance window* (time during which p99 > k × baseline p99), *time-to-new-version*, *time-to-drain*, *state-continuity violations* and *memory overhead*. They follow Papadopoulos et al. [`papadopoulos2021methodological`].

**Venues.** The ICPE Artifact or Data Challenge track; ACM REP; the JOSS (Journal of Open Source Software) paper format; an artifact badge alongside Idea 1.

**Risk.** Low. **Fit.** Comes naturally out of Idea 1. Publish it as an artifact companion.

---

## Ideas added after implementation (grounded in measured results)

These came out of building S1-S8 and are backed by data already in this
repository. Each is a strong candidate for a first publication because the
core evidence exists.

### Idea 9. Transactional Patch Sets: Request-Scoped Consistency for Live Updates of Go Services

**Evidence:** S6 ([docs/solutions/S6-hotpatch-ready-compilation.md](solutions/S6-hotpatch-ready-compilation.md)).
Across 3,200 requests spanning upgrades and rollbacks, **0** requests saw a mixed
patch set with per-request epoch pinning, and **78** did with pinning disabled.
Apply time for 100 functions is 110 µs, and the visibility switch is a single atomic store (<100 ns).

**RQ.** Can a compiler-assisted (source-to-source) transformation give Go the
"patchable function entry" capability of `-fpatchable-function-entry` / MSVC
`/hotpatch` with a *request-level* consistency model, analogous to Linux
livepatch's per-task model, at negligible cost?

**Contributions:** (1) the `hotpatchgen` transformation and its rules; (2) the
transactional, epoch-pinned patch-set protocol; (3) striped reference counts that cut
multi-core trampoline cost from ~120 ns to 1-5 ns; (4) evidence that refcount-only
quiescence is unsound in Go (goroutines spawned by old code and captured function
pointers), so a stack scan is needed; (5) hidden costs: trampolines are never
inlined (defer) and cause argument heap escapes.

**Venues:** ICSME, Middleware, EuroSys/ASPLOS workshops (e.g. PLOS, VMIL); a short version at ICSE NIER.
**Fit:** a paper, and a thesis chapter in place of or next to Chapter 5.

### Idea 10. How Hot-Patchable Is Real Go Code? An Empirical Study of Update Classes in Open-Source Go Projects

**Evidence:** S8 ([docs/solutions/S8-update-classifier.md](solutions/S8-update-classifier.md),
[results/dimpclass/](../results/dimpclass/)). On 450 consecutive real commit pairs
(chi, gotify/server, microservices-demo frontend), **48.5%** of non-bot updates are
hot-patchable in-process, only **2.7%** change in-memory state schemas, and most
restarts are forced by **dependency bumps** (bots make up 91% of the frontend's commits) and by
changes to **initialisation-time code** (router/middleware/config built once at start-up),
not by long-running loops (3 of 450).

**RQ.** What fraction of real-world updates to Go services can be applied by each DSU
mechanism, and what actually blocks in-process patching?

**Why it is publishable:** classic DSU studies (Ginseng, Kitsune, Rubah) report
update-class distributions for C/Java; none exist for Go or for cloud-native
services. The "initialisation-time code" finding goes against the usual focus on
long-running loops.

**To strengthen it:** extend to 10-20 repositories and 2,000+ commits, add a second rater
(Cohen's kappa), and analyse templates and config files as well as Go code.
**Venues:** MSR (Mining Software Repositories), ICSME, EMSE journal.
**Fit:** an excellent, low-risk first paper for the student.

### Idea 11. Graceful Is Not Lossless: Request Loss in Go's http.Server.Shutdown and Process Handoffs

**Evidence:** S4/S5 ([docs/solutions/S5-fd-passing.md](solutions/S5-fd-passing.md), section
"Keep-alive close race"; [results/handoff-fix/summary.md](../results/handoff-fix/summary.md)).
Two independent loss mechanisms were found and fixed:

1. *Accepted-but-unread connections.* `http.Server.Shutdown` closes connections that
   were accepted but whose request has not yet been read. This gave 1-8 EOFs in about half of 15
   early S5 runs, and a connection-tracking drain removed them.
2. *Keep-alive idle-close race (the main result).* At 21k req/s the full campaign lost
   137 requests across S4/S5 runs, all on **reused keep-alive connections**, 9-12 ms after
   the trigger. Root cause: `SetKeepAlivesEnabled(false)` (and `Shutdown`) call
   `closeIdleConns()`, which closes connections a client has just sent a request on
   (RFC 9112 §9.5). `tcp_migrate_req` does not help, since it covers only unaccepted
   connections. The fix: never close at the seal, send `Connection: close` (RFC 9112
   §9.6) on every response during the drain, and give idle connections a 1 s grace. After
   the fix: **0 failed requests in 70 runs (6.38 M requests)**, against 137 before, at a cost of
   +4-9 ms in median upgrade time.

A default Go client would have hidden every one of these losses: it retries idempotent
requests on reused connections automatically. Our load generator disables retry, which matches
non-idempotent traffic (RFC 9110 §9.2.2), so "zero downtime" claims measured with
retrying clients can overstate safety.

**RQ.** Do the "graceful" shutdown primitives of mainstream server stacks guarantee zero
request loss during upgrades? Under which load and connection-reuse conditions do they fail,
and does client retry mask the losses?

**Contributions:** characterisation and root cause in the Go stdlib; a drain protocol
that eliminates the loss on our testbed; quantified masking by client retry; the effect
of `tcp_migrate_req`; extension to Envoy, Node, Java/Netty and nginx as future work; an
upstream Go issue or patch.
**Venues:** short paper or experience track (Middleware Industry, SoCC, ICPE, USENIX ;login:).
**Fit:** fast, high impact, and fully reproducible with this repository.

### Idea 12. In-Process Progressive Delivery: Shadow-Then-Commit Canaries Inside a Single Pod (extends Idea 4 + 6 with results)

**Evidence:** S7 ([docs/solutions/S7-k8s-inplace-patch-operator.md](solutions/S7-k8s-inplace-patch-operator.md)).
On kind with 3 replicas: a good patch was applied in place to all pods with 0 failed requests,
0 restarts and unchanged pod UIDs; a buggy patch was caught in shadow in 0.6 s (3 wrong
answers in 23 mirrored requests) and rolled back fleet-wide in 1.15 s with 0 client-visible errors;
a tampered bundle was rejected in 49 ms.

**Findings worth a paper:** keep-alive connections starve per-pod analysis and make the
fleet reject correct patches (a new failure mode of fleet-wide canary analysis);
cold-start bias makes new code look slow, so the candidate must be warmed up before judging;
exact differential testing is possible because both versions see identical state.

**Venues:** SoCC, Middleware, IEEE CLOUD, ICSE-SEIP.

---

## Recommended master's thesis structure

**Working title.** *Zero-Downtime Upgrades of Go Microservices in Kubernetes: In-Process Hot Swapping versus Process-Level Handoff, with Verified Draining and Secure Orchestration.*

This combines **Idea 1** (empirical core) with **Idea 2** (theoretical and algorithmic contribution) and **Idea 4** (system and security contribution). Idea 3 is an optional extension of Chapter 6, and Idea 8 is the artifact.

1. **Introduction.** Motivation (the cost of pod replacement, stateful services, upgrade failures [`zhang2021upgradefailures`]); research questions; contributions; publications.
2. **Background and related work.** Condensed from `docs/literature-survey.md`: the DSU taxonomy, quiescence theory, process handoff, Kubernetes rollout semantics, and gaps G1-G10.
3. **Problem statement and threat model.** Workload contract, metric definitions (ZDU-Bench), and the attacker model for patch endpoints.
4. **Design space of upgrade mechanisms for Go.** S1-S5 and B0. Go-specific constraints (no plugin unload, toolchain matching, goroutines, the `Serve()` loop). Implementation notes.
5. **Version-aware epoch draining (Idea 2).** The global-counter liveness failure, the epoch protocol, the TLA+ model and model checking, the drain-time bound, and micro-benchmarks.
6. **Empirical comparison (Idea 1).** Methodology, results across load, request duration, state size and repeated upgrades; state-continuity analysis; discussion. *(Optional 6.x: state-transfer correctness testing for S5, from Idea 3.)*
7. **Kubernetes-integrated, signed in-process upgrades (Idea 4).** The operator and CRD design, provenance verification, evaluation against rolling update and OpenKruise, and security analysis.
8. **Discussion.** When to use which mechanism (a decision guide); threats to validity (laptop testbed, a single synthetic workload, Go version); generalisation to other languages.
9. **Conclusion and future work.** Wasm DSU (Idea 5), shadow validation (Idea 6), LLM-assisted transformers (Idea 7).

**Updated recommendation (after implementation).** Use S1-S8 as the system chapters.
Chapter 4 covers S1-S5 plus the "graceful is not lossless" finding (Idea 11). Chapter 5 is
transactional patch sets (Idea 9, which subsumes most of Idea 2). Chapter 6 is the
empirical comparison (Idea 1, with [docs/results.md](results.md)). Chapter 7 is the
operator (Idea 12). Add a new chapter on update classes in real code (Idea 10),
which also motivates the whole thesis ("half of real updates could be hot-patched").

**Suggested publication path.**
0. *Fastest wins with existing evidence:* Idea 10 (MSR / ICSME) and Idea 11 (short or industry paper).
1. Idea 2 as a short paper (ICSE NIER, ICSME or a Middleware workshop), roughly 3 months.
2. Idea 1 with the Idea 8 artifact at ICPE or IEEE CLOUD, roughly 6 months.
3. Idea 4 at SoCC, Middleware or a security workshop (CCSW), at the end of the thesis.
4. Put everything on arXiv first.
