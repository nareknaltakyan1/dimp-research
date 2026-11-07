# Literature Survey: Zero-Downtime Upgrades, Dynamic Software Updating and Hot Handoff for Cloud-Native Go Microservices

*Project: "Zero-Downtime Microservice Upgrades via Dynamic In-Memory Binary Patching in Cloud-Native Environments"*
*Author role: research scientist. Status: survey v1, November 2026.*
*BibTeX keys refer to `paper/refs.bib`.*

## How this survey was verified

Every work cited here was checked in November 2026. For peer-reviewed papers, the DOI was resolved through the Crossref API and the title, authors, venue, year and pages were compared with the bibliography entry. Papers without a DOI (USENIX, arXiv) were checked on the publisher or arXiv page. Grey literature (kernel and Kubernetes documentation, project READMEs, engineering blogs) is marked as such in `refs.bib` and was fetched at the stated URL. Specific claims about results (for example "60 ms downtime") come from the abstract, the publisher page or the official documentation that was read. Where only metadata could be read and the content could not, the text says so (marked **[metadata only]**). Several candidate references that could not be confirmed were dropped, among them a "Kubernetes in-place pod resize" article in a little-known journal, several low-quality 2025-2026 "zero-downtime" papers, and any claim about LLM-generated *live* patches (see Section 2.9).

---

## 1. Introduction and scope

Dynamic software updating (DSU) means changing a running program without stopping it, while keeping its in-memory state and its open connections (Hicks and Nettles [`hicks2005dsu`]). Surveys by Seifzadeh et al. [`seifzadeh2013survey`] and Ahmed et al. [`ahmed2020mapping`] cover the classic design space: when to update (update points, quiescence), what to update (functions, types, data), and how to carry state forward (state transfer, object transformers). Industry, in practice, avoids DSU almost entirely. The cloud-native default is to *replace* processes: a Kubernetes rolling update creates new pods and deletes old ones [`k8s-deployment`; `burns2016borg`]. That works because microservices are expected to be stateless or to keep their state outside the process [`dragoni2017microservices`]. It costs connection churn, cold caches, extra resources (surge) and a mixed-version window. Upgrades are also a major failure source in their own right. Dumitraş and Narasimhan [`dumitras2009upgrades`] and Zhang et al. [`zhang2021upgradefailures`] both document upgrade failures in distributed systems, and Zhang et al. find many caused by incompatible data or state between versions.

Our project sits between these two worlds. It asks whether Go microservices on Kubernetes can be upgraded *in-process* (atomic pointer swap with per-version epoch draining, Go plugins, WebAssembly modules through wazero) or by *fast process-level handoff* (SO_REUSEPORT, listener-FD passing / tableflip-style re-exec with state transfer), and how those mechanisms compare with orchestrator-level replacement on downtime, tail latency, state continuity and safety.

The survey follows the taxonomy requested by the project:

| § | Family | Representative work |
|---|---|---|
| 2.1 | Kernel live patching | Ksplice, kpatch, kGraft, Linux livepatch, KShot, Plugsched, KUP |
| 2.2 | User-space / binary DSU | Ginseng, UpStare, POLUS, Kitsune, Katana, LibCare, Proteos/MCR, Active DSU, DBMS live patching |
| 2.3 | Language-runtime DSU | Erlang/OTP, JVM HotSwap/DCEVM/JVolve/Rubah/Javelus, .NET Hot Reload, Go plugins and monkey patching, WebAssembly |
| 2.4 | Process-level handoff / hot restart | nginx, HAProxy, Envoy, tableflip, systemd socket activation, Meta Zero Downtime Release |
| 2.5 | Checkpoint/restore and live migration | VM live migration, CRIU, container migration, Kubernetes checkpointing |
| 2.6 | Cloud-native orchestration | rolling update, blue/green, canary, service-mesh shifting, in-place resize, OpenKruise |
| 2.7 | Multi-version execution and update validation | Mx, VARAN, Mvedsua, Tedsuto, Sinatra |
| 2.8 | Update points, quiescence and state-transfer theory | Kramer and Magee, tranquillity, version consistency, Proteus, Hayden et al., Chait-Roth et al. |
| 2.9 | Recent work, 2020-2026 | eBPF-based patching, embedded hot patching, automated hot-patch generation, Wasm components |

For each family we describe the **mechanism**, **safety guarantees**, **state handling**, **overhead**, **limitations**, and **relevance to Go microservices on Kubernetes**.

---

## 2. Taxonomy

### 2.1 Kernel live patching

**Mechanism.** Ksplice [`arnold2009ksplice`] (EuroSys 2009) builds patches automatically at the object-code level by comparing pre- and post-patch kernel builds. It then redirects calls from old functions to new ones with trampolines, after briefly stopping the machine to check that no thread is executing the patched functions. The mainline Linux *livepatch* subsystem [`linux-livepatch`] redirects calls with the kernel's ftrace framework. Its documentation describes it as a hybrid of kGraft's per-task consistency model and kpatch's stack-trace checking. kpatch [`kpatch`] builds a patch module (`kpatch-build`) by compiling the kernel with and without the source patch and comparing the binaries, then hands the result to the livepatch infrastructure. Its README now marks it as being in maintenance mode, with an in-kernel successor. kGraft was announced by SUSE in 2014 [`lwn2014kgraft`].

**Safety.** Livepatch uses a *per-task consistency model*. A task switches to the new code only when it is known not to be running a patched function: either its sleeping stack has been checked (this needs reliable stack traces), or it is returning from the kernel to user space, or it is in the idle loop [`linux-livepatch`]. This is effectively *per-thread quiescence*, and it maps directly onto our per-request versioning (Section 2.8). KShot [`zhou2020kshot`] (DSN 2020) moves patch preparation into an SGX enclave and patch application into SMM, so patching still works if the kernel itself is compromised. It shows that the patch delivery path is part of the attack surface.

**State.** Function-level patching handles data-structure changes poorly. The kpatch README states that patches changing static data are not directly supported, and that when a patch changes how dynamically allocated data is used, `kpatch-build` cannot verify that it is safe. Shadow variables and callbacks are the workarounds [`kpatch`]. Plugsched [`ma2023plugsched`] (ASPLOS 2023) addresses this for one whole subsystem. It turns the Linux scheduler into a module and *rebuilds* scheduler state when the module is swapped, and its abstract reports downtime below tens of milliseconds. KUP [`kashyap2016kup`] (USENIX ATC 2016) takes a different route. It checkpoints applications with CRIU, boots a new kernel in place and restores the applications, reporting about 3 s of total downtime even for major kernel version changes that hot patching cannot handle.

**Overhead.** In steady state, overhead is one extra indirect jump per patched function. Applying a patch costs a brief stop or a per-task transition period.

**Limitations.** Function granularity, little support for data or semantic changes, the need to patch everything at once, and special build tooling.

**Relevance.** We do not patch the kernel, but kernel live patching supplies three ideas we reuse: (i) a per-task consistency model as a template for per-request version pinning; (ii) the separation between a *small, function-level hot patch* and a *large, state-transforming update* (Plugsched, KUP), which corresponds to our split between in-process swap and process handoff; (iii) patch provenance as a security concern (KShot).

### 2.2 User-space binary and in-memory DSU (C/C++ and native code)

**Compiler-assisted DSU.** Ginseng [`neamtiu2006ginseng`] (PLDI 2006) compiles C programs so that functions are called through indirection and types are wrapped, which lets updates change both functions and types at update points that the programmer specifies. Neamtiu and Hicks [`neamtiu2009timely`] extend this to multi-threaded programs with *induced update points* and relaxed synchronization. UpStare [`makris2009upstare`] (USENIX ATC 2009) uses *stack reconstruction* to update immediately: active stack frames are rewritten to their new-version equivalents, so that once the update is applied only new code runs. POLUS [`chen2007polus`] (ICSE 2007) relaxes consistency. Old and new versions of data coexist, and write-through synchronization keeps them coherent, so the update need not wait for quiescence. Kitsune [`hayden2012kitsune`] (OOPSLA 2012) changed the model. Instead of swapping code inside one running image, it reloads the *whole program* and transfers the heap state through programmer-assisted, generated transformers ("xfgen"), with explicit update points. Proteos [`giuffrida2013proteos`] (ASPLOS 2013) and Mutable Checkpoint-Restart (MCR) [`giuffrida2014mcr`] (Middleware 2014) apply the same "new process plus automated state transfer" idea to an OS and to generic server programs. Giuffrida and Tanenbaum [`giuffrida2012statetransfer`] argue that *state transfer* rather than code swapping is the hard and error-prone part. PSU [`karpoff2020psu`] (SBAC-PAD 2020) is a more recent framework for multi-threaded C.

**Binary-level hot patching.** Katana [`ramaswamy2010katana`] (ARES 2010) patches ELF executables at the object level, with no source-level DSU support. LibCare [`libcare`] attaches to a running process with `ptrace`, maps patch code into it and overwrites the original function entries with unconditional jumps. It is used for security fixes such as glibc GHOST. Its README states that patches do not survive a process restart.

**Updating active code.** Active DSU [`strombaeck2024activedsu`] (Onward! 2024) targets functions that never return, such as a server main loop. It rewrites the call stack to migrate the locals and control flow of active functions. This is the "infinite loop" problem that every Go server's `Serve()` loop also has.

**Domain-specific live patching.** Fruth and Scherzinger argue for live patching of DBMS binaries [`fruth2022dbms`; `fruth2024case`]. They inject domain-specific *quiescence points* so that threads can move to the patched version, and measure how workload and quiescence method affect throughput and tail latency. In PACMMOD 2024 [`fruth2024kv`] they live-patch Redis Cluster, distributing patches over the cluster's gossip protocol. In one scenario they report cluster-wide updates "orders of magnitude faster" than rolling updates, with no unfavourable throughput or tail-latency trade-off. This is the closest prior work to our goal of in-memory upgrades for a *cluster* service.

**Safety.** These systems guarantee type safety at update points (Ginseng, through static analysis [`stoyle2005mutatis`]), or "only new code after the update" (UpStare), or nothing beyond the programmer's assertions (binary patchers).

**State.** These systems use either in-place state (Ginseng, POLUS, binary patchers) or whole-heap transfer to a new image (Kitsune, MCR, Proteos).

**Overhead.** Ginseng-style indirection costs a few percent in steady state. Kitsune and MCR cost almost nothing in steady state but take time at update to transfer the heap. Binary patchers add one jump.

**Limitations.** C-specific compilers or toolchains, manual transformer and update-point annotations, and fragile binary rewriting.

**Relevance to Go.** None of these systems targets Go. Go has a moving runtime, a garbage collector, goroutine stacks that grow and move, inlining, and a linker that does not support unloading (Section 2.3), and all of these break the assumptions that binary patchers rely on. Kitsune and MCR's "restart and transfer state" model is the conceptual ancestor of our process-level handoff with state transfer (solution family S4/S5). Fruth et al.'s quiescence points correspond to our per-request version boundary.

### 2.3 Language-runtime DSU

**Erlang/OTP.** Erlang was designed for hot code loading [`armstrong2007erlang`]. The runtime keeps up to two versions of a module (current and old). A process moves to the current code only by making a fully qualified call, and when a third version is loaded the old code is purged and processes still running it are killed [`erlang-codeloading`]. This is a *language-level two-version epoch scheme*: version pinning per process, explicit switch points, and a bounded number of live versions. Our version-aware draining design is a deliberate Go analogue of it.

**JVM.** Standard HotSwap (`Instrumentation.redefineClasses`) replaces method bodies. The Java SE documentation states that active frames keep running the old bytecode, new calls use the new definition, static state is kept, and initializers are not re-run. Unsupported changes throw an exception [`java-instrumentation`]. DCEVM [`wurthinger2010dcevm`] modifies HotSpot to allow arbitrary class changes, including field and hierarchy changes. JVolve [`subramanian2009jvolve`] adds VM-supported DSU with object transformers applied during GC at safe points, and Magill et al. [`magill2012tos`] synthesize such transformers automatically. Rubah [`pina2014rubah`] runs on a stock JVM by bytecode rewriting and transfers state by heap traversal, either eagerly or lazily, with essentially no steady-state overhead. Javelus [`gu2012javelus`; `gu2014lowdisruptive`] lowers disruption with lazy object updating. Cazzola and Jalili [`cazzola2016dodging`] show that update points chosen without care can be unsafe even when the runtime mechanism is sound. A 2025 preprint by Mlinaric and Mornar [`mlinaric2025javadsu`] compares the change types supported by current Java DSU options and their resource demands **[preprint; abstract only read]**.

**.NET.** Hot Reload (formerly Edit and Continue) applies supported edits to a running app, but the Microsoft documentation restricts it to `Debug` configurations. Release builds, trimming and ReadyToRun are unsupported, and "rude edits" force a restart [`dotnet-hotreload`]. It is a developer-productivity tool, not a production DSU mechanism.

**Go.** Go has no supported DSU. The standard `plugin` package [`go-plugin`] loads shared objects that are built with `-buildmode=plugin`. Its documentation warns that a plugin cannot be closed, that it works only on Linux, FreeBSD and macOS, that it is poorly supported by the race detector, and that the host and plugin must be built with the same toolchain version, build tags, flags and dependency sources, or runtime crashes are likely. The request to unload plugins (golang/go #20461, opened 2017) is still open with milestone *Unplanned* [`go-issue-20461`]. Go maintainers point out that values, types and goroutines that came from a plugin can outlive any unload. So every plugin-based upgrade *leaks* the old version's code and data, which bounds how many upgrades a process can take before it must restart. Monkey patching libraries such as `bouk/monkey` [`bouk-monkey`] rewrite function entries with jumps at run time. The README itself says it "is not threadsafe. Or any kind of safe", that it fails with inlining, and that it needs memory pages that are both writable and executable. The repository is archived. These are test tools, not upgrade mechanisms. A safe in-process upgrade for Go therefore has to be *designed into the application*: an atomic pointer to a versioned handler table plus an explicit draining protocol (our S1), plugins with accepted leakage (S2), or a sandboxed module runtime (S3).

**WebAssembly.** Wasm gives a portable, sandboxed and formally specified module format [`haas2017wasm`]. In Go, wazero [`wazero`] is a zero-dependency runtime that needs no CGO, which makes it possible to instantiate a new module version, route new requests to it, and drop the old instance. Unlike Go plugins, the old instance can actually be reclaimed. The Component Model [`wasm-component-model`] adds typed interfaces between components. We found **no peer-reviewed study of Wasm-based hot swapping in server applications** (searched Crossref, ACM DL, arXiv and general web, November 2026). Wasm's isolation also means that state either lives in the host or must be explicitly exported and imported across the module boundary, which turns state transfer into an explicit interface.

**Safety and overhead summary.** Managed runtimes give type-safe swaps at method granularity, with old frames finishing on old code. Arbitrary changes need VM extensions and transformers. Steady-state overhead is close to zero (Rubah, JVolve), and update latency depends on heap size when an eager transformation is used.

### 2.4 Process-level handoff and hot restart

Process-level handoff accepts that the process is replaced and focuses on **never refusing a connection** and **draining in-flight work**. The key primitive is that the kernel object behind a listening socket can be shared or passed between processes.

* **nginx binary upgrade.** `USR2` makes the master start the new binary, which inherits the listening sockets. `WINCH` gracefully stops the old workers, and `QUIT` retires the old master. Rollback is possible because the old master keeps its listeners open [`nginx-control`].
* **HAProxy seamless reload.** Tarreau [`tarreau2017haproxy`] explains why plain SO_REUSEPORT reloads lose connections: each socket has its own accept queue, which is reset when the old socket closes. HAProxy's fix passes the listening FDs to the new process over a UNIX socket with `SCM_RIGHTS` (`expose-fd listeners`, `-x`). In his `ab` test, failed requests dropped from 11 (restart) and 1 (plain reload) to 0.
* **SO_REUSEPORT.** Added in Linux 3.9, it spreads incoming connections across sockets that are bound to the same port by hashing the 4-tuple [`kerrisk2013reuseport`]. Kerrisk notes that adding or removing listeners can drop connections that are mid-handshake. In 2021 Iwashima posted a patch series [`iwashima2021migrate`] adding `net.ipv4.tcp_migrate_req`, which migrates pending and accept-queue connections to surviving listeners in the group when one closes. It is off by default. *Which kernel version first shipped it is not confirmed here.* This matters directly for our SO_REUSEPORT solution: without migration, accept-queue loss is expected.
* **Cloudflare tableflip.** This Go library [`tableflip`; `cloudflare2018graceful`] follows nginx's model. `Upgrade()` fork-execs the new binary with inherited listener FDs, the child calls `Ready()` when initialized, and the parent then drains and exits. Its stated goals are no old code after a successful upgrade, a grace period for initialization, tolerance of crashes during initialization, and a single upgrade at a time. The blog post explains why SO_REUSEPORT alone is not enough.
* **Envoy hot restart.** The new Envoy process fully initializes, takes copies of the listen sockets from the old process over a UNIX domain socket, starts listening and then tells the old process to drain. Counters and most gauges are transferred over the same RPC channel. The documentation says explicitly that hot restart "does not transfer existing connections" [`envoy-hotrestart`].
* **systemd socket activation.** The init system owns the listening socket, so a service can be restarted while connections queue in the kernel ("Not a single connection is lost during the upgrade") [`poettering2011socketactivation`].
* **Meta Zero Downtime Release (ZDR).** In SIGCOMM 2020 [`naseer2020zdr`], Naseer et al. describe socket takeover between parallel instances of Facebook's L7 proxies (Proxygen) for TCP and UDP/QUIC (the latter with user-space forwarding of packets for old connections). They add *Downstream Connection Reuse* for long-lived MQTT connections and *Partial Post Replay* for in-flight uploads, where a restarting server hands partial request state downstream to be replayed elsewhere. The APNIC summary by the authors reports updates "up to tens of times a day" for application servers.
* **FD stealing.** `pidfd_getfd(2)` (Linux 5.6) lets a suitably privileged process duplicate another process's FD without that process cooperating [`pidfd-getfd`]. It is an alternative to `SCM_RIGHTS` for sidecar-driven handoff, and it needs ptrace-level permission.

**Safety.** Connection-level: no refused connections if the listeners are passed rather than re-bound. Application-level correctness (state) is *not* addressed. Envoy moves statistics, ZDR moves partial requests, and everything else starts empty.

**State.** Only kernel objects (sockets) are preserved automatically. Application state must be explicitly serialized (Envoy stats, ZDR replay) or kept outside the process.

**Overhead.** Two processes coexist during draining (twice the memory and a cold cache), and the new process pays its full start-up time. Long-lived connections need a draining deadline or an application protocol for migration (ZDR).

**Limitations.** Linux-specific semantics, platform differences (tableflip supports Linux and macOS only), and in-flight requests are drained rather than migrated.

**Relevance.** This is the most mature family and the strongest baseline for our in-process approaches. Our S4/S5 solutions implement it in Go. The open research questions are (i) state-transfer correctness (Gap G4) and (ii) how it interacts with Kubernetes readiness and EndpointSlices (Gap G5).

### 2.5 Checkpoint/restore and live migration

Live migration of VMs [`clark2005livemigration`] (NSDI 2005) uses iterative pre-copy of memory and reports service downtimes as low as 60 ms. CRIU [`criu`] checkpoints a Linux process tree or container to disk and restores it, and is integrated with Docker, Podman, LXC and Kubernetes. TCP connection repair (`TCP_REPAIR`), merged around Linux 3.5, lets established TCP connections be serialized and restored without the peer noticing [`corbet2012tcprepair`]. Container live migration has been studied with CRIU [`stoyanov2018containermigration`], for edge service handoff with Docker [`ma2017edgehandoff`] (SEC 2017), and for multi-container Kubernetes pods across clusters [`poggiani2024podmigration`] (2024). Kubernetes' *Forensic Container Checkpointing* (KEP-2008) adds a kubelet endpoint to checkpoint a running container. It reached beta in v1.30, restore happens outside Kubernetes, and pod-level checkpointing and migration are listed as non-goals [`kep2008`].

**Relevance.** Checkpoint/restore preserves state *with* the code. It moves the *same* version, so it is not an upgrade mechanism by itself. KUP [`kashyap2016kup`] and MCR [`giuffrida2014mcr`] show how C/R becomes an upgrade mechanism when combined with state transformation, and the cost is that you need a state-mapping function. For Go, CRIU-restoring a different binary is not meaningful, so C/R is useful only as a baseline for "state-preserving restart of the same version" and as a fallback for rollback.

### 2.6 Cloud-native orchestration

Kubernetes descends from Borg and Omega [`verma2015borg`; `burns2016borg`]. The Deployment controller's `RollingUpdate` strategy replaces pods gradually, governed by `maxSurge` and `maxUnavailable` (both 25% by default), while `Recreate` kills all pods first [`k8s-deployment`]. Pod termination is graceful (30 s default, with a `preStop` hook). Endpoint removal runs *concurrently* with shutdown, and terminating endpoints are marked `ready=false` but may stay `serving` [`k8s-podlifecycle`]. This creates the well-known race in which traffic still arrives after SIGTERM. Blue/green and canary releases, and service-mesh traffic shifting (Istio weighted routing [`istio-trafficshifting`]), move traffic *between pods*. In-place pod resize (KEP-1287) reached stable in v1.35, but it covers *only CPU and memory* and explicitly does not change images or code [`kep1287`]. OpenKruise's in-place update changes a container's image without recreating the pod, which avoids rescheduling, IP reallocation and volume re-mounting [`openkruise-inplace`]. The container process is still restarted, so in-memory state is lost. Empirical studies of zero-downtime deployment on Kubernetes exist but are thin (for example Rakshit and Banerjee [`rakshit2024zdd`], a short 5-page conference paper).

On the theory side, Chait-Roth, Namjoshi and Wies [`chaitroth2026consistent`] (POPL 2026) formalize *update consistency* for microservices whose workers share a data store. They prove that update methods that ignore semantic properties such as commutativity must allow inconsistent mixed-mode behaviour, and they give algorithms that control how requests are routed to old or new workers. They also observe that language-level DSU methods target single servers.

**Safety.** Readiness probes, PodDisruptionBudgets and rollback by ReplicaSet revision. No semantic guarantee about mixed-version interaction (see Chait-Roth et al.).

**State.** In-process state is discarded by design.

**Overhead.** Surge capacity (by default up to 25% extra pods), connection re-establishment, cold caches and JIT/warm-up, plus image pull and scheduling latency.

**Limitations.** The whole pod is the unit of change. There is no first-class notion of "update the code inside a running container".

**Relevance.** Kubernetes is the substrate we must fit into. The gap is an orchestration-level API and controller that *triggers an in-process or in-pod upgrade*, keeps readiness, observability and rollback semantics, and verifies what code is actually running (Gaps G5 and G6).

### 2.7 Multi-version execution and update validation

Multi-version execution (MVE) runs old and new versions side by side and compares their behaviour. Mx [`hosek2013mx`] (ICSE 2013) runs both versions and survives crashes in either. VARAN [`hosek2015varan`] (ASPLOS 2015) makes N-version execution efficient by sharing a ring buffer of system-call results. Mvedsua [`pina2019mvedsua`] (ASPLOS 2019) combines Kitsune DSU with VARAN. The update is applied to a forked copy while the original keeps serving. Afterwards the two versions' outputs are compared, with expected divergences declared in a DSL, and any unexpected divergence triggers a rollback, which is safe because the states were kept in sync. It was evaluated on Redis, Memcached and vsftpd. Tedsuto [`pina2016tedsuto`] (ICST 2016) is a general framework for *testing* dynamic updates, and Hayden et al. [`hayden2012testing`] evaluate update safety by systematic testing over update points. Sinatra [`venkateshwaran2021sinatra`] (arXiv 2021) applies MVE-based DSU to unmodified browsers by recording and replaying non-deterministic JavaScript events.

**Relevance.** These works provide the validation methodology our project lacks: (i) running v1 and v2 on mirrored traffic within one process or pod and comparing their responses, and (ii) testing at every possible update point. No such tool exists for Go HTTP services (Gap G7).

### 2.8 Update points, quiescence, tranquillity and state-transfer theory

Kramer and Magee [`kramer1990evolving`] defined **quiescence**. A component may be updated when it is passive and no transaction that involves it is in progress or will be started, which may require blocking connected components. They revisited the concept in 2025 [`kramer2025quiescence`] **[metadata only: a 5-page TSE article; abstract not accessible to us]**. Vandewoude et al. [`vandewoude2007tranquility`] proposed **tranquillity**, a weaker, less disruptive condition that only requires the component not to be in the middle of a transaction that it will interact with again. They note that tranquillity is not always reachable and that quiescence must then be used as a fallback. Ma et al. [`ma2011versionconsistent`] and Baresi et al. [`baresi2017versionconsistency`] defined **version consistency** for distributed transactions: each transaction must run *entirely* in the old or entirely in the new configuration. Their dynamic dependency tracking reduces disruption compared with quiescence. Ajmani, Liskov and Shrira [`ajmani2006modular`] (ECOOP 2006) let nodes in a distributed system run different versions simultaneously, with simulation objects that translate between them.

For single programs, Proteus [`stoyle2005mutatis`] (POPL 2005) gave a formal calculus with static *con-freeness* checks that guarantee type-safe updates at update points. Hayden et al. [`hayden2012specifying`] specify update correctness as relations between old and new program traces and verify them by program merging. Hayden et al. [`hayden2012quiescence`] studied quiescence in multithreaded programs empirically. They found that waiting for all threads to reach quiescence can be slow, and that some threads, such as long-running loops, may never get there without program changes.

**Concurrency analogues.** Read-copy-update (RCU) [`guniguntala2008rcu`] and epoch-based reclamation [`hart2007reclamation`; `brown2015reclaiming`] solve a structurally identical problem: readers keep using an old version until a *grace period* has passed, and the old version is reclaimed only once every reader that could have seen it has finished. Hart et al. compare quiescent-state-based, epoch-based and hazard-pointer reclamation and show that their performance differs sharply depending on workload and on delayed threads.

**Implications for our project.** The student's proof of concept used a *single global in-flight counter* and waited for it to reach zero before declaring v1 drained. Under steady load, new requests keep arriving, so the counter may never reach zero. This is exactly the global-quiescence liveness failure that Kramer and Magee's blocking, Hayden et al.'s measurements and the RCU literature anticipate. The fix that the theory suggests is *version consistency at request granularity*: tag every request with the version it was admitted under (an epoch), keep one counter per epoch, admit new requests only to the new epoch, and wait for the old epoch's counter alone to drain. That gives the same structure as RCU grace periods and Erlang's two-version rule. With a bounded number of live epochs (at most two), draining finishes in a time bounded by the longest in-flight request rather than by the arrival process. Making this argument formally, with a liveness bound and a mechanized or model-checked proof (for example in TLA+), is a small but publishable contribution (Gap G3).

### 2.9 Recent work, 2020-2026

* **eBPF as a patching substrate.** RapidPatch [`he2022rapidpatch`] (USENIX Security 2022) hot-patches RTOS firmware with eBPF patches checked by a verifier, and reports average patch delay under 8 µs. bpftime [`zheng2025bpftime`] (OSDI 2025) is a user-space eBPF runtime that uses binary rewriting and MPK isolation to extend applications safely. It could serve as a *verifier-checked* hook mechanism for user-space hot fixes, but we found no work that uses it for DSU of Go services.
* **Automated hot-patch generation.** AutoPatch [`salehi2024autopatch`] (CCS 2024) uses LLVM to derive hot patches from official source patches for embedded devices, and reports fixing over 90% of 62 CVEs. Zhou et al. [`zhou2025stackframe`] (CCS 2025) patch heterogeneous embedded systems by reconstructing stack frames. Retcon [`watson2024retcon`] (IPSN 2024) provides live updates for embedded event-driven applications. These are all static or compiler-based. **We found no peer-reviewed work in which an LLM generates *live* patches together with their state transformers.** The closest work is LLM-based kernel driver adaptation and kernel crash fixing, which produce ordinary patches, not live patches, so we do not cite them as DSU work. LLM-assisted state-transformer synthesis therefore remains an open direction (Gap G9).
* **Data systems.** Fruth and Scherzinger [`fruth2024case`; `fruth2024kv`] bring live patching to DBMSs and Redis Cluster (Section 2.2).
* **Kernel.** Plugsched [`ma2023plugsched`] updates whole subsystems. KShot [`zhou2020kshot`] protects patching with trusted execution environments.
* **Active functions.** Active DSU [`strombaeck2024activedsu`] (Section 2.2).
* **Microservice update theory.** Chait-Roth et al. [`chaitroth2026consistent`] (Section 2.6). Kramer and Magee revisit quiescence [`kramer2025quiescence`].
* **Wasm components.** Tooling is mature (wazero, Component Model), but no peer-reviewed DSU study exists (Section 2.3).
* **Kubernetes.** In-place resource resize reached stable in v1.35 [`kep1287`], container checkpointing is in beta [`kep2008`], and OpenKruise offers in-place image update [`openkruise-inplace`]. None of these preserves process memory across a *code* change.

---

## 3. Comparison table

Legend for **downtime**: "none" = no refused connections or failed requests in principle; "drain" = no refused connections, but in-flight work must finish under the old version; "ms" / "s" = order of a short service pause.

| Approach | Granularity | State preservation | Downtime | Safety guarantee | Language-agnostic? | Cloud-native fit |
|---|---|---|---|---|---|---|
| Kernel livepatch / kpatch / kGraft / Ksplice | Function (kernel) | Full (in place); data changes only via shadow variables | none / brief stop | Per-task consistency; stack checks | Kernel only | Node-level; transparent to pods |
| Plugsched / KUP | Subsystem / whole kernel | Rebuilt (Plugsched) or C/R (KUP) | tens of ms / about 3 s | State rebuild / C/R | Kernel only | Node-level |
| Ginseng / UpStare / POLUS | Function + type (C) | Full, in place | none to ms | Type safety at update points (Ginseng); "new code only" (UpStare) | No (C, special compiler) | Poor (custom toolchain) |
| Kitsune / MCR / Proteos | Whole program | Heap transferred via transformers | ms to s (state-size dependent) | Programmer-specified update points + transformers | No (C) | Moderate (one process) |
| Katana / LibCare (binary patching) | Function (ELF) | Full, in place | none | None beyond patch author | Native ELF only | Poor (ptrace, privileged) |
| Erlang/OTP hot loading | Module | Full; process-level switch | none | Two-version rule; explicit switch | No (BEAM) | Good inside BEAM |
| JVM HotSwap / DCEVM / JVolve / Rubah / Javelus | Method / class | Full; transformers for class changes | none to ms | Old frames finish on old code; transformers | No (JVM) | Moderate |
| .NET Hot Reload | Method | Full | none | Rude-edit detection; Debug only | No (.NET) | Development only |
| Go plugin swap (our S2) | Package / plugin | Host-held state preserved | none (drain) | Toolchain-match requirement; no unload | No (Go, Linux/macOS/FreeBSD) | Moderate (image must contain plugins) |
| Go atomic-pointer + epoch drain (our S1) | Handler table / service | Full (in process) | none (drain) | Per-request version consistency (if designed) | Pattern is language-agnostic | Good (no pod churn) |
| Wasm module swap (our S3) | Module | Host-held or exported state | none (drain) | Sandbox isolation; typed interface | Yes (any language targeting Wasm) | Good |
| Go monkey patching (bouk/monkey) | Function | Full | none | None ("not threadsafe") | No | Unsuitable for production |
| SO_REUSEPORT re-bind | Process | None (unless external) | Some connections may be lost without `tcp_migrate_req` | Connection-level only | Yes | Good (single pod) |
| FD passing / tableflip / nginx / HAProxy / Envoy | Process | Sockets only; app state needs explicit transfer | none (drain) | No refused connections | Yes | Good (single pod) |
| Meta ZDR | Process + protocol | Sockets + partial requests (replay) | none | Connection + request continuity | Partly (protocol support) | Very good (at hyperscale) |
| systemd socket activation | Process | Sockets (held by init) | queueing delay | Connection-level | Yes | Limited in containers |
| CRIU / container live migration | Process / container | Full (same version) | ms to s | Same-version fidelity; TCP repair | Yes | Emerging (KEP-2008 beta) |
| K8s rolling / blue-green / canary / mesh shifting | Pod | None in process (external only) | none at service level if probes are correct | Readiness, PDB, rollback; no mixed-version semantics | Yes | Native |
| K8s in-place resize (KEP-1287) | Pod resources | Full (no code change) | none | n/a (no code change) | Yes | Native (stable v1.35) |
| OpenKruise in-place image update | Container | None in process (container restarts) | restart time | Workload controller | Yes | Native add-on |
| MVE (Mx, VARAN, Mvedsua) | Process | Synchronized across versions | none | Divergence detection + rollback | Mostly (system-call level) | Not yet applied |

---

## 4. Synthesis: what the literature does and does not tell us

1. **The dichotomy is in-place versus replace-and-transfer.** Since Kitsune and MCR, the DSU community has moved toward *restarting into a new process and transferring state*, because swapping code in place is fragile. Process-level hot restart (nginx, Envoy, tableflip, ZDR) is the industrial version of the same idea, but it transfers *only sockets*. Our project measures both ends for Go: in-process swap (S1-S3) and handoff with explicit state transfer (S4/S5).
2. **State transfer is the hard part.** Giuffrida and Tanenbaum, Magill et al., and Zhang et al.'s upgrade-failure study all identify state and data-format incompatibility as the main cause of failure. Industrial hot restart sidesteps the problem.
3. **Quiescence can fail to arrive.** Global quiescence may never arrive under load (Kramer and Magee; Hayden et al. 2012). Tranquillity and version consistency weaken it, and RCU and epochs give a practical implementation pattern. The student's "global in-flight counter" proof of concept is a textbook instance of this liveness problem.
4. **Go is an outlier.** Erlang, the JVM and .NET all have runtime-level support. Go has none: plugins cannot be unloaded, and monkey patching is unsafe. In-process DSU in Go has to be an *application-level architectural pattern* or go through a sandboxed runtime (Wasm).
5. **Orchestrators are unaware of in-process upgrades.** Kubernetes treats the pod as the unit of change. Recent features (in-place resize, checkpointing, OpenKruise) get close but do not preserve memory across a code change. Chait-Roth et al. show that mixed-version consistency needs semantics-aware routing, a concern that applies equally inside one process during draining.
6. **Validation exists for C, not for cloud services.** MVE and DSU testing frameworks offer differential checking and rollback, but they are tied to C or the JVM and have not been integrated with canary analysis or service meshes.

---

## 5. Research gaps

* **G1. No principled DSU for Go.** No peer-reviewed system gives dynamic updates for Go programs. The standard plugin mechanism cannot unload code (golang/go #20461), and monkey patching is unsafe and unsupported. Safe *application-level* patterns (versioned handler tables, explicit update points, plugin or Wasm sandboxes) have not been designed, characterised or compared in the literature.
* **G2. No controlled comparison of in-process and process-level zero-downtime mechanisms.** Each family is evaluated in isolation, with different workloads and metrics. Examples are HAProxy's `ab` test, Envoy's documentation, Fruth et al.'s Redis experiments, and ZDR's production telemetry. No study measures in-process swap (atomic pointer, plugin, Wasm), SO_REUSEPORT, FD passing with state transfer, and Kubernetes rolling update *under the same workload and metric definitions* (failed requests, p99/p99.9 latency during the upgrade, time-to-new-version, time-to-drain, memory overhead, state continuity).
* **G3. Draining and quiescence under continuous load lack a liveness-bounded, version-aware treatment for request-driven services.** Global in-flight counters may never drain. Version consistency and RCU grace periods give the right structure, but no work instantiates them for HTTP/gRPC microservices, gives a drain-time bound (for example a function of maximum request duration and not of the arrival rate), or verifies the protocol (for example with TLA+ or model checking).
* **G4. The correctness of state transfer during process handoff is undefined and untested.** Industrial hot restart transfers sockets and, at most, metrics. Academic state transfer (Kitsune, MCR) targets C heaps. No correctness criterion exists for *application state* handed between Go processes during a live handoff: no-loss and no-duplication of updates, monotonic counters, exactly-once effects for in-flight requests, behaviour under a crash during handoff. Nor is there a testing method in the style of Tedsuto or Hayden et al. for it.
* **G5. Orchestration does not model in-process upgrades.** Kubernetes has no first-class way to say "upgrade the code inside this running pod". In-place resize covers only resources, and OpenKruise in-place still restarts the container. Open questions: how readiness and EndpointSlice semantics, PodDisruptionBudgets, rollout history and rollback, and mixed-version routing (Chait-Roth et al.) should behave when the version changes *inside* a pod.
* **G6. The security of in-memory patching in cloud-native settings has not been studied.** Supply-chain frameworks (TUF [`samuel2010tuf`], in-toto [`torresarias2019intoto`], Sigstore [`newman2022sigstore`]) protect images and artifacts, but nothing binds a *runtime-loaded patch* to a signed provenance chain, enforces admission policy for it, or attests which code is actually running once the image digest no longer describes the process. KShot [`zhou2020kshot`] shows that the patching path is an attack surface. In-process patch endpoints (`/admin/upgrade`) are a new remote code-loading surface.
* **G7. There is no lightweight validation or rollback for in-process upgrades of cloud services.** MVE (Mvedsua) and DSU testing (Tedsuto) are tied to C or the JVM and to system-call interposition. No equivalent exists for Go or HTTP services: shadowing a fraction of in-process requests to v2, comparing responses with declared expected divergences, and rolling back automatically, all integrated with canary analysis.
* **G8. WebAssembly as a DSU vehicle has not been characterised.** Wasm runtimes allow modules to be instantiated and *reclaimed*, which Go plugins do not, but no peer-reviewed work measures Wasm-module hot swap in server applications: swap latency, steady-state overhead against native Go, state-export interfaces, memory reclamation, or the effect on tail latency.
* **G9. Automated generation of updates and state transformers.** Transformer synthesis exists for Java [`magill2012tos`], and automated hot-patch derivation exists for embedded C [`salehi2024autopatch`]. We found no work, LLM-based or otherwise, that generates or checks *state-transfer functions* for microservice upgrades, nor any that evaluates LLM-generated live patches.
* **G10. There is no agreed benchmark or metric set for "zero downtime".** Definitions vary ("no refused connections", "no 5xx", "no latency spike"). The cloud-performance methodology guidelines of Papadopoulos et al. [`papadopoulos2021methodological`] and tail-latency concerns [`dean2013tail`] are rarely applied in upgrade studies. A reusable, open benchmark harness with a stateful workload (as in our `docs/CONTRACT.md`) would itself be a contribution.

---

## 6. Positioning of this project

Our implementations map onto the gaps as follows. S1 (atomic pointer with per-version epochs) addresses G1 and G3. S2 (Go plugins) and S3 (wazero) address G1 and G8. B0, S4 and S5 (rolling restart, SO_REUSEPORT, FD passing / re-exec with state transfer) address G2 and G4. A Kubernetes operator or sidecar that triggers in-process upgrades with signed patches addresses G5 and G6. The shared contract (stateful counter, `X-Version`, upgrade event log) is a first step toward G10. The concrete publication plan is in `docs/paper-ideas.md`.

---

## 7. Notes on verification and exclusions

* **Dropped as unverifiable or low quality:** several 2025-2026 articles found through Crossref with "zero downtime" or "in-place pod resize" in the title, published in venues we could not assess. Also dropped: claims about specific LLM live-patching systems, and the claim that `tcp_migrate_req` first shipped in a particular kernel version.
* **Content not fully read:** Kramer and Magee 2025 (metadata only). Mlinaric and Mornar 2025 (abstract only). Active DSU (SPLASH programme abstract only). Plugsched (abstract only).
* **Earlier versions not separately cited:** journal versions that duplicate conference papers, such as SIGPLAN Notices reprints of PLDI and OOPSLA papers. The KShot TDSC extension (2023) was seen but not added.
