# sandboxd — project overview, and how it differs from Agent Substrate

This document gives a high‑level overview of **sandboxd** and contrasts it with
[Agent Substrate](https://github.com/agent-substrate/substrate), the
Google‑originated Kubernetes runtime that inspired the same core idea. The two
systems solve the same fundamental problem — cheaply hosting many mostly‑idle,
stateful agent sessions — and even share the same key primitive (sandbox
checkpoint/restore of full RAM + filesystem state; both can use gVisor `runsc`). But
they were built for different worlds, and sandboxd exists largely *because* Substrate
could not run where we needed it to.

For the shipped design in depth, read
[architecture-sandboxd.md](architecture-sandboxd.md); for the auth front door,
[architecture-broker.md](architecture-broker.md).

---

## 1. What sandboxd is

sandboxd is a **session‑teleport control plane on Amazon EKS**. It runs warm pools
of privileged worker pods; each worker runs an arbitrary OCI image as an isolated
sandbox — a **nested gVisor sandbox** (`runsc`) by default, or a **Cloud Hypervisor
microVM** for a hardware boundary (per pool). A user's **session** — the sandbox's full RAM +
filesystem state — is decoupled from any particular pod: it can be checkpointed to
**S3**, its worker freed, and later restored (**"teleported"**) onto *any*
interchangeable warm worker on *any* node, with state intact. That gives temporal
oversubscription: many sessions, fewer workers, idle sessions parked in S3.

An MCP client (e.g. Claude Code) reaches a session through an authenticating front
door (**agentgateway + Keycloak + a broker**) and a session‑aware **router**, so a
user keeps one durable session that survives suspend/resume — even across node
loss, scale‑in, and rollouts.

### The load‑bearing distinction: worker vs. sandbox

| | **Worker** | **Sandbox (session)** |
|---|---|---|
| Kubernetes object? | Yes — a Pod from an operator‑managed Deployment | No — a `runsc` container *inside* a worker |
| Placed by | kube‑scheduler (pod resource requests) | the operator picks *which idle worker* (assignment only) |
| Portable? | No (it's just capacity) | **Yes** — teleports across workers via S3 |

The schedulable unit is a real pod; the portable stateful unit is the sandbox. The
operator does **not** reimplement kube‑scheduler — it keeps a lightweight assignment
table and does O(1) "pick any idle worker in the matching pool."

### Components (all on EKS)

- **Router** — thin, stateless, session‑aware reverse proxy. Resolves
  `X‑Session‑ID` → worker via the assignment table; fast‑path proxies to a live
  worker, else singleflights a resume. Read‑only on state.
- **Operator** (kubebuilder) — the control brain and **sole writer** of the
  assignment table. Reconciles pools → worker Deployments; drives the resume
  workflow (claim idle worker → `/run` cold‑start or `/restore` teleport); runs the
  idle‑suspend, checkpoint, session‑GC, and worker‑binding‑reclaim sweeps.
- **Valkey** — in‑cluster Redis‑compatible **hot cache** for assignment
  (CAS‑on‑version, single writer). Not the durable truth.
- **sandboxd worker** — a static Go binary (PID 1, privileged, on a gVisor node)
  that drives `runsc` to run/checkpoint/restore nested sandboxes and does its own S3
  I/O. HTTP API on `:8090`.
- **S3** — per‑session checkpoint store (`sandboxes/<sid>/<snap>/…`). Only the
  checkpoint travels; base image layers stay on the node's containerd cache.
- **Durability** — session state is mirrored to `Session.status` in **etcd** on
  durability‑critical transitions; Valkey is rebuilt from etcd on startup, so a cache
  restart never loses the index or orphans an S3 checkpoint.

### CRDs

`SandboxTemplate` (a pool's worker‑shape — scheduling/resources — plus an *optional*
pinned image), `AppTemplate` (the scheduling‑free workload half: image + ports/health/
idle/iam, run on a generic pool), `WarmPool` (capacity + `minIdle` warm headroom),
`Session` (a teleporting session; `poolRef` = capacity, `appRef` = workload),
`ForkSet` (fan out N sessions from one source), `BaseSnapshot` (a promoted golden
checkpoint to fork from).

A pool is **dedicated** (its `SandboxTemplate` pins one image) or **generic** (image
empty — it runs whatever workload a Session brings via `appRef`), so one pool's
capacity can host many workloads without a warm pool per image. See
[PRD‑arbitrary‑image‑sessions §13](PRD/PRD-arbitrary-image-sessions.md).

### Notable capabilities

Teleport (RAM+FS), warm pools with `minIdle` autoscaling, graceful scale‑in
(`pod-deletion-cost` ordering) + **checkpoint‑on‑terminate** (lossless eviction),
on‑demand suspend, periodic background checkpoints, **ForkSet** fan‑out, **per‑session
AWS IAM credentials** (teleport‑safe), session **garbage collection** (S3 + KV + CR),
**worker‑binding reclaim**, and optional **SPIFFE/SPIRE mTLS** on the control hops.

---

## 2. What Agent Substrate is

Agent Substrate is an open‑source, Google‑originated system that "manages agent‑like
workloads on Kubernetes to achieve higher scale and efficiency" by multiplexing many
idle **actors** onto fewer **worker** pods. Its defining model is the *inverse* of
one‑pod‑per‑session: it snapshots an actor's **RAM + writable filesystem** into "a
single versioned snapshot," streams it to durable object storage, wipes the worker
back into the pool, and re‑animates the actor sub‑second on any warm worker on the
next inbound request.

**Components (from its architecture doc):**

- **`ate-api-server`** — the Control Plane: owns actor/worker lifecycle, holds the
  **Scheduler** that selects a ready worker for a resume, exposes a **gRPC** API
  (`ResumeActor` / `SuspendActor`).
- **`atecontroller`** — reconciles the `WorkerPool` / `ActorTemplate` CRDs into
  standby worker pods.
- **`atelet`** — node DaemonSet supervisor; instructs the sandbox herder to
  freeze/capture and streams the snapshot to storage.
- **`ateom`** — the in‑pod "sandbox herder" that runs the checkpoint/restore, with
  **two backends: `ateom-gvisor` (runsc) and `ateom-microvm`** — i.e. Substrate is
  not gVisor‑only; it abstracts over the isolation runtime.
- **`atenet`** — networking: an **Envoy** router + External Processing server, plus
  DNS. Each actor gets a **DNS name** (`<actor>.<atespace>.actors.resources.substrate.ate.dev`);
  the router extracts the actor from the **`Host` header**, asks the Control Plane
  for its location, resumes it if suspended, then proxies to the worker pod IP.

Dynamic actor/worker state lives in a **Redis/Valkey** store (actor → location +
status; worker → IDLE/BUSY + hosted actor); snapshot durability comes from **GCS**
(S3 exists in code). CRDs live in the kube‑apiserver. Tested on **GKE** and **kind**.

**How it behaves (the parts that matter for the comparison):**

- **Suspend is explicit/eager, not idle‑automatic.** The doc describes suspend via an
  explicit `SuspendActor` call; it notes actors are "very bursty" but does **not**
  describe automatic activity‑based idle detection. Resume is triggered by an inbound
  request at the gateway or an explicit API call.
- **mTLS is on by default for all internal hops** — "all internal system
  communication … is secured via mutual TLS with short‑lived certificates" — which is
  exactly the pod‑certificate identity layer that blocks it on EKS (§4).
- **Ambitious scale targets:** ~1 billion actors (active + idle), 1000 wakeups/second
  per cluster, ~100 ms activation at p95.

**Explicitly *not yet* implemented** (its own "Problems We Haven't Addressed Yet" /
per‑phase notes): **worker autoscaling**, **control‑plane authn/z**, granular
**authorization/identity policy**, and **garbage collection** ("not implemented
yet"). Multi‑tenancy, TTL/expiry, and automatic idle suspension are not addressed.
Status: **v0.0.0, "very early development," APIs almost guaranteed to change.**

> Note: this reflects Substrate as surveyed for this project (mid‑2026, pinned SHA
> `4aedeab`, from its README + `docs/architecture.md` + `docs/glossary.md` +
> `demos/sandbox`). It is a fast‑moving v0.0.0 project; component/CRD names and the
> "not yet implemented" list may have drifted since — re‑check upstream before
> relying on any specific claim here.

---

## 3. The shared idea, and the divergence

Both systems reach the **same core insight**: an agent session is mostly idle, so
don't pin it to a pod — snapshot its full RAM+FS state, park it in object storage, and
re‑hydrate it on any warm worker on demand (sandboxd and Substrate's gVisor backend
both use `runsc` checkpoint/restore for this). sandboxd's "teleport" and Substrate's
"actor suspend/resume" are the same primitive.

They diverge because they were built for different constraints:

- **Substrate** is a general, framework‑agnostic *compute‑runtime layer* — a broad
  platform intended to host any containerized actor at scale, GKE‑first.
- **sandboxd** is a *purpose‑built, EKS‑native* control plane for one job (durable
  per‑user MCP sandbox sessions behind an OAuth front door), and it ships the pieces
  a production deployment needs that Substrate hadn't yet built.

---

## 4. Why sandboxd exists (the EKS blocker)

sandboxd is not a "not‑invented‑here" reimplementation. We first tried to run our
AIO sandbox **on** Substrate. Stage 0 pre‑flight found Substrate **cannot install
on managed Amazon EKS at any version available today**:

- Substrate's install deploys a **pod‑certificate controller** (its mTLS identity
  layer) that the installer *waits on*, and it requires the API server to serve
  `certificates.k8s.io` **`PodCertificateRequest` (KEP‑4317)** and
  **`ClusterTrustBundle` (KEP‑3257)** resources.
- Those APIs sit behind feature gates that default **off even at beta** (a SIG‑auth
  exception), don't reach on‑by‑default until **GA in K8s 1.37**, and **EKS does not
  let customers enable arbitrary control‑plane feature gates**. Empirically confirmed
  on EKS 1.31 / 1.33 / **1.35** — only `certificates.k8s.io/v1` is served; the
  install stalls permanently at the trust‑bundle wait.

That is an upstream/EKS timeline we don't control. Combined with Substrate being
v0.0.0 with **no shipped auth, no multi‑tenancy, no GC, and MCP support that was
aspirational rather than implemented**, the pragmatic path was: keep the working
front door, and build an **EKS‑viable control plane that delivers the same
teleport value today** — sandboxd. (Substrate's own S3 snapshot support and its
suspend/resume model validated the approach; the runtime was just unreachable on our
platform.)

---

## 5. Side‑by‑side

| Dimension | **sandboxd** | **Agent Substrate** |
|---|---|---|
| Core model | Teleport a session (RAM+FS) across warm workers | Multiplex actors across warm workers |
| Isolation runtime | **Nested** gVisor `runsc` (default) **or** Cloud Hypervisor microVMs, per pool (`spec.runtime`) | Pluggable herder: `ateom-gvisor` (runsc) **or** `ateom-microvm` |
| Target platform | **Amazon EKS** (runs today) | GKE / kind; **blocked on managed EKS** (§4) |
| Snapshot store | **S3** (first‑class) | GCS first‑class; S3 in code |
| Dynamic state | Valkey cache **+ etcd durability** (rebuilt on restart) | Redis/Valkey (no documented etcd/durability backing) |
| CRDs | SandboxTemplate (pool worker‑shape + optional image), AppTemplate (workload), WarmPool, Session, ForkSet, BaseSnapshot | ActorTemplate, WorkerPool (+ Atespaces) |
| Pool ↔ workload | **generic** pool (image‑less template) runs any `AppTemplate` a session brings, or **dedicated** pool pins one image; placement is a pool property, workload can't set scheduling | one `ActorTemplate` per actor kind |
| Routing | Router by `X‑Session‑ID` → assignment table (no per‑sandbox DNS) | `atenet`/Envoy by `Host` = per‑actor DNS name |
| Streaming/session semantics | **Carries Streamable‑HTTP MCP** (SSE token‑by‑token, `FlushInterval:-1`) | "Session‑aware routing"; SSE/streaming preservation not documented |
| Lifecycle API | HTTP `/resume`, `/_warm` + declarative CRDs | gRPC `ate-api-server` (`ResumeActor`/`SuspendActor`) |
| Suspend trigger | **Automatic idle detection** (O(due) sweeps) + on‑demand + on‑terminate | **Explicit `SuspendActor`** (no automatic idle detection documented) |
| Scheduler in path? | No — O(1) pick‑any‑idle‑worker (kube‑scheduler placed the workers) | No — Control‑Plane Scheduler claims a warm worker |
| **Auth / multi‑tenancy** | **Shipped**: agentgateway + Keycloak OAuth, JWT passthrough, group gate, per‑user quota | **Not shipped** (control‑plane authn/z listed as unaddressed) |
| **MCP** | **First‑class** — the whole point | Aspirational; shipped demo is bespoke `POST /process`, not MCP |
| **Garbage collection** | **Shipped**: TTL / abandoned / orphan‑S3 / orphan‑CR full‑footprint reap | **"Not implemented yet"** |
| Warm‑capacity autoscaling | `minIdle` warm‑headroom autoscaling | **"Not addressed yet"** (worker autoscaling is future) |
| Lossless eviction | **checkpoint‑on‑terminate** (drain‑wait within grace) | Not documented |
| Fan‑out | **ForkSet** (N sessions from a snapshot or image) | Not documented |
| Per‑session cloud identity | **Per‑session AWS IAM** (teleport‑safe) | Not documented |
| Control‑hop security | SPIFFE/SPIRE mTLS **opt‑in** (off by default) + opt‑in NetworkPolicy | Pod‑certificate mTLS **on by default** — the very thing that blocks EKS |
| Scope / maturity | Narrow, production‑oriented, EKS‑hardened | Broad, framework‑agnostic, v0.0.0 |
| Identity model | One durable session per user (broker‑keyed) | One durable actor per identity |
| Scale ambition | Right‑sized for per‑user MCP fleets; churn‑cost flat with fleet size | ~1B actors, 1000 wakeups/s, ~100 ms p95 activation (stated targets) |

---

## 6. What sandboxd adds beyond Substrate's model

Because sandboxd targets a real production deployment rather than a general runtime,
it ships the layers Substrate leaves to the user (or hadn't built):

1. **An OAuth front door + multi‑tenancy** — Keycloak, agentgateway JWT passthrough,
   group‑gated access, per‑user quota. Substrate ships no auth.
2. **First‑class MCP** — the broker and router carry Streamable‑HTTP MCP (SSE /
   `Mcp‑Session‑Id`) end‑to‑end; sandboxes expose a real MCP hub. Substrate's shipped
   demo is a bespoke `/process` endpoint.
3. **Full garbage collection** — reaping a dead session's whole footprint (S3 + KV +
   CR) across TTL / abandoned / orphan classes, under a least‑privilege S3 identity.
4. **Durability as truth** — etcd‑backed session status with cache rebuild, so a
   Valkey restart can't orphan checkpoints.
5. **Operational hardening for churn** — O(due) sweeps, O(1) pool counts, zero‑etcd‑
   write resumes, graceful scale‑in + checkpoint‑on‑terminate, worker‑binding reclaim.
6. **Per‑session AWS IAM credentials** and **SPIFFE/SPIRE control‑hop mTLS**.
7. **ForkSet / BaseSnapshot** fan‑out for branch‑from‑common‑state workloads.

## 7. What Substrate has that sandboxd does not

- **Breadth / framework‑agnosticism** — Substrate is a general actor runtime for any
  containerized workload (ADK, LangChain, arbitrary servers), not specialized to MCP
  sandbox sessions. sandboxd is deliberately narrower. (It is closing part of this gap:
  **generic pools + `AppTemplate`** let one pool run many admin‑authored workloads;
  raw caller‑supplied *arbitrary* images remain a proposed, governed extension.)
- **A pluggable isolation backend** — `ateom` abstracts the herder over **gVisor and
  microVMs** (`ateom-microvm`). sandboxd now has the same seam: `runtimeDriver` with a
  nested‑gVisor `runsc` default and a Cloud Hypervisor microVM driver, selected per pool
  via `SandboxTemplate.spec.runtime` (see
  [PRD-microvm-runtime-cloud-hypervisor.md](PRD/PRD-microvm-runtime-cloud-hypervisor.md)).
- **gRPC lifecycle API + `kubectl-ate` CLI** as a first‑class programmatic surface;
  per‑actor DNS names and Envoy‑based routing (vs sandboxd's header‑keyed proxy).
- **A much larger multiplexing story** — explicit design targets of ~1B actors, 1000
  wakeups/s, and ~100 ms p95 activation cluster‑wide, plus Atespaces as a grouping
  primitive.
- **Community + upstream momentum** as a general‑purpose platform.

Bear in mind several of Substrate's differentiators are *ambitions or defaults*, not
shipped guarantees, at v0.0.0 — the auth, GC, and autoscaling gaps above are why we
did not adopt it wholesale.

---

## 8. One‑line summary

> **Substrate** is a broad, GKE‑first, early‑stage (v0.0.0) *runtime* for multiplexing
> any stateful actor at very large scale, with a pluggable isolation backend but with
> auth, authorization, garbage collection, and autoscaling still on the roadmap.
> **sandboxd** takes the same checkpoint/restore‑teleport insight and delivers it as a
> narrow, EKS‑native, production‑hardened control plane for durable per‑user MCP
> sandbox sessions — built precisely because Substrate's pod‑certificate identity layer
> cannot run on managed EKS, and shipping the auth, MCP, GC, durability, automatic
> idle‑suspend, and lossless‑eviction layers Substrate leaves open today.
