# Problem Statement

**Topic:** Zero-Downtime Microservice Upgrades via Dynamic In-Memory Binary Patching in Cloud-Native Environments

## 1. Context

Cloud-native microservices are upgraded many times per day. The standard
Kubernetes approach is *replacement*: start new pods, shift traffic, terminate
old pods (rolling update, blue/green, canary). This is robust, but it has costs
that matter in some settings:

* **In-memory state is lost.** Caches, connection pools, session tables,
  counters, ML models and JIT warm-up all start cold, so pods have to rebuild
  them, and the warm-up shows up as tail-latency spikes after every rollout.
* **Connections are disrupted.** Long-lived connections (WebSocket, gRPC
  streams, HTTP keep-alive) must be closed and re-established. Requests racing
  with endpoint removal (the kube-proxy / endpoint propagation delay) can fail.
* **Capacity and time overhead.** `maxSurge` needs spare capacity, image pull
  plus start-up plus readiness can take tens of seconds, and stateful
  singletons cannot be duplicated at all.
* **Small patches pay the full price.** A one-line hot fix triggers the same
  full redeploy as a major release.

*Dynamic software updating* (DSU) replaces code **inside a running process**,
or hands a live process's sockets and state to a successor, without a restart.
It is established in OS kernels (Ksplice, kpatch, livepatch), in Erlang/OTP,
and in L4/L7 proxies (Envoy hot restart, HAProxy, Meta's socket takeover). It
is **not** established for general-purpose, statically compiled microservices
(Go, Rust, C++) running in containers.

## 2. Research problem

> How can a statically compiled microservice in a cloud-native environment be
> upgraded to new code **without dropping requests, without losing in-memory
> state, and without unbounded resource growth**, and how do in-process
> patching mechanisms compare with process-level handoff and with orchestrator
> replacement in availability, latency, correctness and overhead?

## 3. Sub-problems

| # | Sub-problem | Why it is hard |
|---|---|---|
| P1 | **Atomic switch-over.** Requests must see either the old or the new code, never a mix within one request. | Memory model, torn reads, requests in flight during the switch. |
| P2 | **Quiescence / draining.** Determine *when* the old version is no longer executing so it can be released. | A global in-flight counter never drains under steady load (see the review of the original PoC). Long requests and streams delay quiescence indefinitely. |
| P3 | **Loading genuinely new code.** v2 is not known at build time. | Go has no first-class DSU. `plugin` cannot unload and needs identical toolchains. Raw machine-code patching is unsafe and platform-specific. |
| P4 | **State transfer.** Moving in-memory state from v1 to v2, including representation changes. | Schema evolution, pointers, goroutines, consistency during transfer. |
| P5 | **Connection continuity.** For process-level upgrades, not a single accepted or queued connection may be lost. | Accept queues per socket (the `SO_REUSEPORT` pitfall), FD inheritance, keep-alive connections. |
| P6 | **Safety and rollback.** Detect a bad v2 and revert without downtime. Ensure only trusted code is loaded. | Health criteria, partial state transfer, code signing / supply chain. |
| P7 | **Cloud-native integration.** Make the orchestrator aware of in-place upgrades. | Kubernetes models an upgrade as pod replacement; image/spec drift; observability. |
| P8 | **Resource overhead.** Repeated upgrades must not leak memory or slow the hot path. | Plugins are never unloaded; per-request synchronisation cost. |

## 4. Scope of this project (stage 1)

Selected solution families, implemented in Go and compared with a common
harness (see [CONTRACT.md](CONTRACT.md)):

| ID | Family | Mechanism | Addresses |
|---|---|---|---|
| B0 | Baseline | Naive stop-and-restart | (reference) |
| B1 | Baseline | Kubernetes rolling update on kind | (reference) |
| S1 | In-process | Atomic pointer + **per-version epoch draining** (fixed PoC) | P1, P2, P4, P6 |
| S2 | In-process | Go `plugin` (.so) dynamic loading | P3, P8 |
| S3 | In-process | WebAssembly module hot loading (wazero) | P3, P4, P6, P8 |
| S4 | Process-level | New process binds with `SO_REUSEPORT`, old drains | P5 |
| S5 | Process-level | Listener FD passing (`SCM_RIGHTS`) + state transfer | P4, P5, P6 |

Out of scope for stage 1: raw machine-code patching via ptrace and function
trampolines (planned as future work, Linux only).

## 5. Evaluation questions

See [evaluation-methodology.md](evaluation-methodology.md): RQ1 availability,
RQ2 latency impact, RQ3 state preservation, RQ4 resource overhead, RQ5 upgrade
duration, RQ6 safety and rollback.
