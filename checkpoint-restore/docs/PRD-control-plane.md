# PRD — sandboxd Control Plane (session-aware warm pool + router)

Status: Draft (2026-07-03), on the `checkpoint-restore` branch.
Audience: implementers of the control plane that sits on top of the proven
`sandboxd` worker agent. Read [`ARCHITECTURE.md`](ARCHITECTURE.md) and
[`NETWORKING-LIFECYCLE.md`](NETWORKING-LIFECYCLE.md) first — this PRD assumes the
worker primitives they describe (`/run`, `/checkpoint`, `/restore`, `/suspend`,
`/reset`, `/capacity`, `/status`) already exist and are validated end-to-end
(cold-run reliability + teleport across workers, verified 2026-07-03).

---

## 1. Problem & goal

`sandboxd` proves the hard part: a gVisor sandbox's RAM+filesystem state can be
checkpointed to S3, its worker freed, and the state restored ("teleported") onto
a different worker on a different node with state intact. What does **not** exist
yet is the layer that turns that primitive into a service:

- something that **provisions and maintains a pool of warm workers**,
- something that lets an operator **declare which image to run** (as a reusable
  `SandboxTemplate`) — or lets an authorized user **bring their own arbitrary
  image** — and **how long a sandbox may sit idle** before it is checkpointed and
  its worker reclaimed (see §4.1, §4.4, and O6),
- and — the crux — a **session-aware router** that knows *which worker currently
  holds a given user's session* and routes traffic there, and that **restores
  the user's checkpoint onto a fresh worker** when no worker holds it.

**Goal:** a control plane that makes a user's sandbox feel always-available and
sticky to their session, while the underlying worker is ephemeral — freed when
idle, recreated on demand, and reattached to the session transparently on the
next request.

We build **our own controller and router** that reproduce agent-substrate's
WorkerPool + transparent resume-on-connect model — but **without substrate's
dependencies**, because substrate's runtime cannot run on managed EKS (§9). We
borrow the proven *patterns* (singleflight resume-on-connect, an assignment
table, a WorkerPool CRD) and express them in kubernetes-sigs **agent-sandbox**
vocabulary (`WarmPool` → `Template` → `Session`), on top of the already-proven
`sandboxd` worker. Everything we depend on must be GA (see platform constraint).

### Platform constraint (hard requirement)

The target platform is **Amazon EKS**, which does **not** allow enabling
Kubernetes **alpha** feature gates or **beta** API services (no
`--feature-gates` on the API server, no opt-in beta serving). Everything this
control plane depends on must therefore be:

- **GA APIs or CRDs we install ourselves.** Our own CRDs
  (`SandboxTemplate`/`WarmPool`/`Session`) are fine — a `CustomResourceDefinition`
  is GA (`apiextensions.k8s.io/v1`) and does not require any feature gate. We
  serve them at our own stable version (e.g. `sandboxd.io/v1alpha1` is just *our*
  naming; it is not a Kubernetes alpha API and needs no cluster feature gate).
- **No dependency on alpha/beta cluster features.** In particular: no reliance on
  Google Pod Snapshots (GKE-only — this is exactly why kubernetes-sigs
  agent-sandbox's deep-hibernation path doesn't port to EKS), and any use of
  Gateway API must be a **GA channel** install that the cluster already supports,
  not an alpha/experimental channel resource. Prefer a plain Service/Deployment
  ingress for the router over anything requiring an experimental Gateway feature.

This constraint is a primary input to the build-vs-adopt decision in §9.

### Non-goals (this PRD)

- Reimplementing the kube-scheduler. Worker **pods** are placed by Kubernetes;
  the control plane only assigns **sessions to already-running workers**
  (an O(1) "pick an idle worker" decision, not bin-packing).
- Socket survival across checkpoint/restore. gVisor sockets do **not** survive
  C/R; clients reconnect. The router makes reconnect transparent; it does not
  preserve in-flight TCP connections across a teleport.
- Multi-sandbox-per-worker. The one-sandbox-per-worker invariant holds.
- Authn/authz of end users. We assume the existing broker/front-door mints the
  session identity; this PRD consumes it (see §5.2) but does not issue it.

---

## 2. Key concepts & vocabulary

| Term | Meaning | Analog |
|---|---|---|
| **Worker** | A privileged pod running `sandboxd` + pinned `runsc`. Hosts **at most one** sandbox at a time. Registers itself with the control plane on startup. | substrate `Worker`; agent-sandbox warm `Sandbox` pod |
| **WarmPool** | Declarative desired set of workers, all built from one `SandboxTemplate`. Owns worker scaling. | agent-sandbox `SandboxWarmPool`; substrate `WorkerPool` |
| **SandboxTemplate** | **Operator-authored** blueprint: the OCI **image** to run as the sandbox, ports, resources, idle policy. Referenced by a WarmPool and by Sessions. Default way to say "what to run"; a Session may instead carry an arbitrary image inline, authz-gated (§4.4, O6). | agent-sandbox `SandboxTemplate`; substrate `ActorTemplate` (immutable, versioned) |
| **Session** | A user's logical sandbox instance, identified by an opaque **session ID**. Outlives any single worker. Maps to exactly one checkpoint lineage in S3. | substrate `(appID,userID,sessionID)`; agent-sandbox `SandboxClaim` |
| **Session token** | Opaque bearer credential presented by the client on every request. Carries/【resolves to】the session ID. Lets the router decide continue-vs-restore. | substrate session JWT |
| **Assignment table** | The control plane's source of truth: `sessionID → {state, workerPodIP, snapshotURI, image, ...}`. Backed by a fast KV store. | substrate Redis worker/actor state |
| **Router** | Session-aware reverse proxy. Resolves `sessionID → worker`, triggers restore on a miss, forwards the request. | agent-sandbox `sandbox-router` + substrate `atenet` resumer |

A **Session** is the durable thing. A **Worker** is disposable. The assignment
table is the bridge, and the router is the enforcement point.

---

## 3. The session lifecycle (the heart of the design)

A session is a small state machine in the assignment table. The router and the
lifecycle controller are the only writers.

```
                    first request (no entry)
                            │
                            ▼
      ┌──────────┐  assign idle worker + /run   ┌─────────┐
      │  ABSENT  │ ───────────────────────────▶ │ RUNNING │◀─┐
      └──────────┘                              └─────────┘  │ subsequent requests
            ▲                                        │       │ route straight to
            │                                         │ idle  │ worker (fast path)
            │ GC (snapshot TTL)              timeout  │       │
            │                                         ▼       │
      ┌──────────┐   /suspend → S3, free worker  ┌────────────┐
      │ (deleted)│◀───────────────────────────── │ SUSPENDING │
      └──────────┘                                └────────────┘
                                                        │
                                                        ▼
                                                 ┌───────────┐
   request arrives, no worker holds session      │ SUSPENDED │
   ───────────────────────────────────────────▶ │ (in S3)   │
   restore snapshot onto an idle worker          └───────────┘
                            │                            │
                            └────────── RESUMING ────────┘
                                    (restore-on-connect)
```

States: `ABSENT` (never seen / GC'd) · `RUNNING` (a worker holds it, reachable)
· `SUSPENDING` (checkpoint in flight) · `SUSPENDED` (state only in S3, no worker)
· `RESUMING` (restore in flight onto a chosen worker).

### The three request paths the router must implement

1. **Continuation (fast path).** Session is `RUNNING` and its `workerPodIP` is
   live. Forward the request straight to that worker. This is the common case
   and must add negligible latency (one KV lookup).
2. **Restore-on-connect.** Session is `SUSPENDED` (checkpoint in S3, no worker
   holds it). The router/control plane picks an idle worker, calls
   `/restore` with the session's `snapshotURI`, flips the entry to `RUNNING`,
   then forwards the request. Concurrent requests for the same session must
   **not** trigger duplicate restores — coalesce them (§5, singleflight).
3. **Cold start.** Session is `ABSENT` (first time, no checkpoint). Since there
   is no state to be sticky to yet, **any idle worker in the pool can serve the
   request** — pick whichever is free (cheapest: nearest/least-loaded/first
   idle), `/run` the template's image, record `RUNNING`, forward. Stickiness
   begins only *after* the first checkpoint exists; the initial placement is
   unconstrained. (Contrast paths 1 and 2, which are pinned to a specific worker
   or specific checkpoint.)

The user cannot tell these apart except by latency; from their side the session
is simply always there. **This is the requirement from the design review: the
router (or control plane it consults) must know which worker holds the session,
and must restore from checkpoint when none does.**

### Worked example — session ID → checkpoint → restore

Tracing one session through the assignment table (the control plane's KV store)
shows how the **session ID selects which S3 checkpoint to restore**. Say Alice's
session ID is `sess-7f3a` on pool `aio-pool` (template image
`python:3.12-slim`).

1. **First request (cold start).** No entry exists. Control plane picks any idle
   worker `10.0.5.12`, `/run`s the template image, writes:
   ```
   sess-7f3a → { state: RUNNING, workerPodIP: 10.0.5.12,
                 image: python:3.12-slim, snapshotURI: "", version: 1 }
   ```
   No `snapshotURI` yet — nothing has been checkpointed.
2. **Alice works, then goes idle.** Lifecycle controller calls
   `POST /suspend {sandboxId: sess-7f3a}` on the worker. sandboxd checkpoints to
   S3 and returns `snapshot: "sandboxes/sess-7f3a/snap-1783090885"`. Control plane
   updates the entry and frees the worker:
   ```
   sess-7f3a → { state: SUSPENDED, workerPodIP: "",
                 image: python:3.12-slim,
                 snapshotURI: "sandboxes/sess-7f3a/snap-1783090885", version: 2 }
   ```
   The worker returns to the idle pool; Alice's state now lives **only** in S3.
3. **Alice returns (restore-on-connect).** Her session token resolves to
   `sess-7f3a`. Router looks up the entry, sees `SUSPENDED` + a `snapshotURI`,
   picks a *different* idle worker `10.0.3.95`, and calls:
   ```
   POST /restore { sandboxId: sess-7f3a,
                   image: python:3.12-slim,
                   snapshot: "sandboxes/sess-7f3a/snap-1783090885" }
   ```
   **The `snapshotURI` from the table is exactly what tells the worker which S3
   checkpoint to pull.** On ready, the entry flips to
   `{ state: RUNNING, workerPodIP: 10.0.3.95, ..., version: 3 }` and the request
   is forwarded. Alice resumes with state intact on a new worker/node.
4. **Idle again → re-checkpoint.** A new `/suspend` produces
   `snap-1783094000`; the control plane **overwrites** `snapshotURI` to point at
   the newest snapshot (this is the "one checkpoint lineage" in §2 — the table
   always holds the *current* pointer; older snapshots become GC candidates).

So: **session token → session ID → assignment-table lookup → `snapshotURI` →
`/restore`.** The session ID never encodes the S3 path; the control plane's
assignment table owns and maintains the `sessionID → snapshotURI` mapping, and
the router consults it on every request.

---

## 4. CRDs / API surface

We follow the agent-sandbox shape: a **Template** blueprint, a **WarmPool** that
scales workers from it, and a per-session **Claim/Session** object. A Session may
either reference a template or carry an arbitrary image inline (§4.4). Field
names are illustrative; the intent is what matters.

### 4.1 `SandboxTemplate` — what to run

```yaml
apiVersion: sandboxd.io/v1alpha1
kind: SandboxTemplate
metadata: { name: aio-python }
spec:
  # The OCI image run AS THE SANDBOX (nested gVisor workload), not the worker.
  image: docker.io/library/python:3.12-slim
  cmd: ["python", "-m", "http.server", "8080"]     # optional; else image default
  env: ["FOO=bar"]                                  # optional
  ports:
    - { container: 8080, host: 8080 }               # exposed via worker DNAT
  # Readiness: how the worker knows the sandbox is serving (drives router health).
  health: { probe: tcp, probePort: 8080 }
  # Idle policy: how long the sandbox may be idle before checkpoint + reclaim.
  idle:
    timeoutSeconds: 300        # 0 = never auto-suspend
    action: suspend            # suspend (checkpoint→S3, free worker) | reset (discard) | none
  # Worker sizing hint (informs the WorkerPool pod resources).
  resources: { cpu: "1", memory: 2Gi }
```

The `image`/`cmd`/`env`/`ports`/`health` map **directly** onto the sandboxd
`/run` request body — the template is essentially a stored `/run` payload plus an
idle policy. `idle.timeoutSeconds`/`action` map onto the worker's existing
supervisor idle detection (`health.idleTimeoutSec`) and the `/suspend` vs
`/reset` endpoints.

### 4.2 `WarmPool` — how many workers

```yaml
apiVersion: sandboxd.io/v1alpha1
kind: WarmPool
metadata: { name: aio-pool }
spec:
  templateRef: { name: aio-python }
  replicas: 4                    # desired warm workers; HPA-scalable via /scale subresource
  minIdle: 1                     # keep at least N idle workers ready to accept a session
status:
  replicas: 4
  idle: 2
  busy: 2
```

The WarmPool controller reconciles a Deployment of **worker pods** (the existing
`worker-deploy.yaml`, parameterized). It does **not** put the sandbox image in
the worker pod spec — the worker is generic; the sandbox image comes from the
template at `/run`/`/restore` time. `minIdle` ensures restore-on-connect and
cold-start always have a landing spot without waiting for a pod to schedule.

### 4.3 `Session` — the durable per-user instance

Created by the broker/front-door (or lazily by the router on first request).
This is the lease/claim analog and the assignment-table's declarative face.

```yaml
apiVersion: sandboxd.io/v1alpha1
kind: Session
metadata: { name: sess-7f3a... }        # == session ID
spec:
  poolRef: { name: aio-pool }
  # opaque identity the router matches the session token against
  subject: "app/acme/user/alice/session/7f3a"
  # WHAT TO RUN — exactly one of:
  #   templateRef: reference an operator-authored SandboxTemplate (default, safest)
  #   image/cmd/env/ports: an INLINE, user-supplied arbitrary image (see §4.4).
  # If both are set, inline fields override the referenced template.
  templateRef: { name: aio-python }      # OR omit and supply inline:
  # image: ghcr.io/alice/my-tool:1.4     #   arbitrary image chosen by the caller
  # cmd:   ["/app/serve"]
  # ports: [ { container: 8080, host: 8080 } ]
  lifecycle:
    idleTimeoutSeconds: 300              # overrides template if set
    ttlAfterSuspendSeconds: 86400        # GC the S3 checkpoint after this
status:
  phase: Running | Suspended | Resuming | Suspending
  workerPodIP: 10.0.5.12                 # set while Running/Resuming
  snapshotURI: sandboxes/sess-7f3a/snap-...   # set once first checkpoint exists
  lastActiveAt: 2026-07-03T14:33:00Z
```

> **Implementation note.** `Session.status` and the **assignment table** hold the
> same facts. Two viable implementations: (a) CRD-as-truth (controller watches
> `Session`, KV is a cache) or (b) KV-as-truth (fast path never touches the API
> server; `Session` CR is optional/for observability). Given the router is on the
> hot path and must do a lookup **per request**, we lean toward **(b): KV is
> authoritative** (Valkey/ElastiCache/DynamoDB), and the `Session` CR is an
> optional reconciled projection. This matches substrate (Redis-authoritative).

### 4.4 Two ways to say "what to run": template vs. arbitrary image

Nothing in the machinery requires the image to come from an operator-authored
template. sandboxd's `/run` already accepts an **arbitrary image reference** and
pulls it via the node containerd cache; the template layer is a *governance*
choice layered on top, not a technical constraint. We therefore support **both
modes**, and they can coexist in the same pool:

- **Template mode (default).** `Session.spec.templateRef` names a
  `SandboxTemplate`. The operator owns an allow-list of images; users select, not
  supply. Safest, and keeps the warm pool's containerd cache hot (fast cold
  start). This is what substrate/agent-sandbox do.
- **Arbitrary-image mode.** `Session.spec.image` (+`cmd`/`env`/`ports`) carries a
  **user-supplied image**. The control plane passes it straight to `/run` on an
  idle worker. This is the interesting capability the user asked for: let a caller
  bring their own sandbox image. It is fully supported by the worker today.

**Why this is safe enough to offer:** the worker runs the image as a **nested
gVisor sandbox** — gVisor *is* the isolation boundary, and running untrusted code
in it is the entire point of the platform. Arbitrary images don't weaken that
boundary. What they add is a **governance surface**, handled outside the worker:

- **AuthZ gate.** Whether a caller may use arbitrary-image mode (vs
  template-only) is an authorization decision at the broker/front-door, tied to
  the session token's subject (§5.2). Untrusted/anonymous callers get
  template-only; trusted callers may bring an image.
- **Registry policy.** Optionally constrain arbitrary images to allow-listed
  registries or require signatures (cosign/policy) — enforced by the control
  plane before `/run`, not by the worker.
- **Quotas.** Per-subject limits on concurrent sessions / pool share, so one
  caller can't exhaust the warm pool (a broker/control-plane concern regardless
  of image source).

**Tradeoffs to accept (not blockers):**

- **Cold-start penalty on first pull.** A never-before-seen image isn't in the
  node's containerd cache, so the first `/run` on a given worker pays a pull cost
  (measured: cold `golang:1.23-alpine` ~5.7s end-to-end — still fast, but not the
  ~1.5s of a warm image). Warm pools are most efficient when scoped to a known
  image set; fully-arbitrary images erode the pre-warm benefit but don't break it.
  Mitigations if this matters: a per-node image pre-puller, or pools dedicated to
  arbitrary-image sessions with looser warm guarantees.
- **Checkpoint identity travels with the session.** `/restore` hard-checks the
  image + runsc version. The assignment table already records the session's
  `image` alongside its `snapshotURI` (§3 worked example), so restore of an
  arbitrary-image session works unchanged — the recorded image is replayed.

This is the substance of open question **O6**: default template-only, with
arbitrary-image sessions as an authz-gated opt-in. No worker change is needed to
enable it; the work is the authz/registry-policy/quota gate in the control plane.

---

## 5. Router design

The router is the single ingress data-plane component. One static Gateway/LB
sits in front of it; all sandbox traffic flows through it.

### 5.1 Per-request algorithm

```
on request R with session token T:
  sid = resolve(T)                         # validate token → session ID (+ subject)
  e   = KV.get(sid)                         # assignment entry (atomic)

  if e.state == RUNNING and alive(e.workerPodIP):
        return proxy(R → e.workerPodIP)     # (1) CONTINUATION — fast path

  # miss: need a worker. coalesce concurrent misses for the same sid.
  singleflight(sid):
     e = KV.get(sid)                        # re-check under the flight (double-checked)
     if e.state == RUNNING: return          # someone else attached it
     w = pickIdleWorker(e.poolRef)          # from workercache; may block up to minIdle refill
     if e.state == SUSPENDED:
        KV.cas(sid, RESUMING, worker=w)     # optimistic; lose→retry from top
        sandboxd(w)./restore{snapshotURI:e.snapshotURI, image:e.image, ports, health}
     else:                                  # ABSENT → cold start
        KV.cas(sid, RESUMING, worker=w)
        sandboxd(w)./run{image:template.image, cmd, env, ports, health}
     wait until sandboxd(w)./status.ready
     KV.cas(sid, RUNNING, workerPodIP=w.ip)
  return proxy(R → e.workerPodIP)
```

Key properties:
- **One KV read on the fast path.** Continuation must be cheap; everything
  expensive is gated behind a cache miss.
- **Singleflight per session.** Substrate's `ActorResumer` does exactly this: N
  simultaneous requests to a suspended session trigger **one** restore; the rest
  wait on it. Without this, a burst of requests would try to restore the same
  checkpoint onto N workers — a correctness bug (split brain) and a cost bug.
- **Optimistic concurrency (CAS + version).** Two routers (HA) must not both
  claim the same idle worker for different sessions, nor the same session onto
  two workers. Every assignment write is a compare-and-set on a version; the
  loser retries. This is substrate's `version` field pattern.
- **Bounded resume wait.** The inbound request holds until the sandbox is
  `ready` or a deadline (substrate uses ~15s). On timeout, return 503/retry-after
  rather than hang. Restore of a compressed checkpoint is fast (measured
  sub-second to a few seconds), so this budget is comfortable.

### 5.2 Session token → session ID

The router must extract a stable session ID from each request. Options, in order
of preference:

1. **Bearer session token** (opaque or JWT) issued by the broker, carrying a
   subject like `app/acme/user/alice/session/7f3a`. The router validates it
   (signature/introspection) and derives `sid`. This is the substrate model and
   the one this PRD assumes.
2. **Header** (`X-Session-ID`) — agent-sandbox's `X-Sandbox-ID` style — for
   trusted internal callers where the broker already authenticated.

The token is what lets the router distinguish **"continue Alice's existing
session"** from **"this is a new session, cold start"** from **"Alice's session
is checkpointed, restore it."** Without a stable session identity there is no way
to know which checkpoint to restore — hence it is a hard requirement, not an
add-on.

### 5.3 Worker registration & discovery

- On startup each worker registers with the control plane (writes
  `{poolRef, podIP, state:idle, version}` to KV, or is discovered by the
  WarmPool controller via pod labels and written on its behalf). A `workercache`
  (substrate pattern) keeps an in-memory index of idle workers per pool for O(1)
  `pickIdleWorker`.
- `/capacity` on each worker is the ground truth for busy/idle and is used to
  reconcile the KV against reality (e.g., after a worker crash/restart).
- Worker liveness: a worker that dies takes its session's `RUNNING` entry with
  it. Reconciliation detects the dead `workerPodIP`, and — because the session's
  last checkpoint is in S3 — the next request restores from `SUSPENDED`. **A
  session is only as durable as its last checkpoint**; see §8 risk.

---

## 6. Secure worker ↔ control-plane channel

**Requirement.** Traffic between the control plane/router and the workers must be
**encrypted and mutually authenticated**, and **worker pods must not accept
traffic directly** from arbitrary sources — the router is the only ingress to a
worker (both to sandboxd's control API and to the sandbox's exposed data ports).

This is almost certainly *why substrate runs `podcertificate-controller`*: it
mints a per-pod **SPIFFE** identity and does mTLS between control plane, router,
and workers. That API (`PodCertificateRequest`) is exactly what's blocked on EKS
(§9). So we need the same guarantee — service identity + mTLS — using **GA-only**
mechanisms. Two things to solve: **(a) identity/encryption** and **(b) network
isolation**.

### 6.1 Identity + mTLS — recommend SPIRE

**SPIRE (CNCF SPIFFE) is the GA-native replacement for substrate's pod-cert
approach, and gets us the identity model substrate wanted without the blocked
API.** SPIRE does not depend on any alpha/beta Kubernetes API: its Kubernetes
workload attestation uses **projected ServiceAccount tokens** (GA since 1.20)
and the **TokenReview** API (GA), and node attestation uses `k8s_psat` (GA
primitives). SPIRE-server issues short-lived **X.509-SVIDs**; each workload
fetches its SVID over the local Workload API socket (`go-spiffe/v2/workloadapi`).

- Each worker gets a SPIFFE ID, e.g. `spiffe://sandboxd/worker/<pool>`; the
  router/control plane get `spiffe://sandboxd/router` and `.../controller`.
- **sandboxd** presents its SVID and **requires** the peer to present a
  router/controller SVID (mutual). The router does the reverse. So the control
  channel (`:8090` — `/run`, `/checkpoint`, `/restore`, `/suspend`, …) is mTLS,
  and a worker rejects any caller that isn't the router/controller.
- SVIDs are short-lived and auto-rotated by SPIRE — no long-lived secrets, no
  manual cert plumbing. This matches the substrate/SPIFFE security posture on a
  GA-only substrate.

**Cost:** run SPIRE (server StatefulSet + agent DaemonSet on the gVisor nodes),
register entries for the worker/router/controller SPIFFE IDs, and link
`go-spiffe` into sandboxd + the router to source SVIDs and enforce peer IDs.
Modest, well-trodden, and entirely GA.

**Alternatives considered:**
- **cert-manager + a private issuer** (GA CRDs, no gates): simpler to stand up,
  but coarser identity (per-Deployment, not per-workload SPIFFE), and rotation/
  distribution (SDS) is more manual. Viable fallback if SPIRE is deemed too heavy.
- **Service mesh sidecar mTLS (Istio/Linkerd):** gives mTLS "for free," but adds
  a sidecar to every **privileged nested-gVisor** worker (fiddly), and some mesh
  ingress features pull in non-GA Gateway resources we must avoid. Overkill here.

### 6.2 Network isolation — workers don't accept direct traffic

mTLS authenticates the control channel; a **Kubernetes `NetworkPolicy`** (GA,
`networking.k8s.io/v1`) enforces that nothing *else* can even reach a worker:

- **Ingress to worker pods is allowed only from the router/controller** (matched
  by pod/namespace label selector). This covers **both** sandboxd's `:8090`
  control API **and** the DNAT'd sandbox data ports (`podIP:hostPort`) — recall
  the sandbox's exposed ports are reachable by anything that can route to the pod
  IP, so the NetworkPolicy must gate those too. Result: the **only** path to a
  running sandbox is client → router → worker. This mirrors agent-sandbox's
  default (ingress to a sandbox permitted only from `sandbox-router`).
- **Egress** from workers is restricted to what a sandbox legitimately needs (S3
  endpoint / VPC endpoint, DNS); the interior-netns masquerade already NATs
  sandbox egress behind the pod IP, so policy is applied at the pod boundary.
- The router terminates the client connection, authenticates the session (§5.2),
  and forwards over the mTLS channel to the chosen worker — so the data plane is
  encrypted end-to-end between router and worker, and workers are never directly
  addressable by clients.

> NetworkPolicy requires a CNI that enforces it. The VPC CNI supports network
> policies on EKS today (GA), so this is available without any alpha/beta opt-in.
> Confirm it's enabled on the target cluster (**O7**).

### 6.3 What this buys vs. substrate

We reach substrate's security goal — mutually-authenticated, encrypted
control/data channels with per-workload identity, and workers that only accept
router traffic — **without** its EKS-blocked `PodCertificateRequest`/
`ClusterTrustBundle` dependency, by substituting SPIRE (GA) for the pod-cert
controller and NetworkPolicy (GA) for ingress lock-down.

---

## 7. Idle → checkpoint → reclaim

The template/session declares `idle.timeoutSeconds` + `action`. Mechanism:

1. The worker's **supervisor already tracks idle** (`lastReadyAt`, exposes
   `idle:true` on `/status` once `now-lastReadyAt > idleTimeoutSec`). The idle
   signal is defined by the readiness probe today.
2. A **lifecycle controller** (or the router, since it sees request activity)
   watches for idle sessions. On idle:
   - `action: suspend` → call worker `/suspend` (checkpoint→S3, free worker),
     set session `SUSPENDED`, record `snapshotURI`, return the worker to the idle
     pool. **This is the money-saver:** idle sessions cost only S3 storage.
   - `action: reset` → call `/reset` (discard state, free worker). For sessions
     that don't need to survive.
   - `action: none` → leave running.
3. **Activity resets the timer.** Because the router is on the request path, it
   is the natural place to stamp `lastActiveAt` and cancel a pending suspend
   (agent-sandbox has time-based TTL only; we add true **inactivity** timeout,
   which is what the user asked for). Care: race between "suspend fired" and "new
   request arrived" — resolve via the session state machine (a request that
   arrives during `SUSPENDING` waits and then triggers `RESUMING`).

Idle detection based purely on the readiness probe is coarse (it says the
sandbox is *up*, not *unused*). Router-observed request activity is the better
idle signal and should be authoritative for `lastActiveAt`; the worker probe
remains the liveness/health signal. **Open question O3.**

---

## 8. Risks & open questions

**Risks**

1. **A session is only as durable as its last checkpoint.** If a worker dies
   while `RUNNING`, all state since the last checkpoint is lost — restore-on-
   connect brings back the last *checkpointed* state, not the live state.
   Mitigation: checkpoint-on-idle is the primary save point; consider periodic
   background checkpoints for long-lived sessions (cost/latency tradeoff). This
   is inherent to the teleport model and must be stated to users.
2. **Split brain.** Two workers holding the same session (e.g., HA router race,
   or restore fired while the old worker was actually still alive). Mitigation:
   CAS/version on every assignment write + `/capacity` reconciliation + fence a
   worker before restore. Must be designed carefully; it is the top correctness
   risk.
3. **Thundering herd on restore.** A pool-wide event (node loss) suspends many
   sessions at once; a traffic spike then restores them all, exhausting idle
   workers. Mitigation: `minIdle` headroom, restore queue with backpressure,
   `503 Retry-After`.
4. **runsc version skew.** `/restore` hard-rejects a version mismatch. The pool
   must be version-homogeneous; rolling a new runsc means draining
   (checkpoint-all) first. Already enforced at the worker; the pool controller
   must respect it during updates.
5. **CPU-feature match for restore.** gVisor restore requires the target host to
   have the checkpoint host's CPU features. The pool must pin compatible instance
   types (already a documented constraint).
6. **Worker exposure / lateral movement.** A worker runs untrusted user code in a
   nested gVisor sandbox; if a worker were directly reachable it would be an
   attack surface and a lateral-movement risk. Mitigation: §6 — mTLS on the
   control channel (SPIRE) + NetworkPolicy so the router is the only ingress.
   Depends on the CNI enforcing NetworkPolicy (O7) and on SPIRE being deployed.

**Open questions**

- **O1 — KV vs CRD as source of truth.** §4.3 leans KV-authoritative for hot-path
  latency. Confirm, and pick the store (Valkey/ElastiCache vs DynamoDB). Affects
  HA and consistency model.
- **O2 — Where does resume-on-connect live?** In the router itself (lower
  latency, but router becomes stateful/privileged), or a separate control-plane
  service the router calls (cleaner, one more hop)? Substrate splits them
  (`atenet` router calls `ateapi`). Recommend **separate control-plane service**
  for the assignment/restore decision; router stays a thin proxy that consults
  it. Confirm.
- **O3 — Idle signal.** Router-observed request activity vs worker readiness
  probe (§7). Recommend router activity as authoritative for idle.
- **O4 — Token format & issuer.** Who mints the session token, and does the
  router validate locally (JWT + JWKS) or via introspection? Ties into the
  existing broker/front-door.
- **O5 — Session affinity vs load.** One session = one worker at a time (sticky).
  No load-spreading of a single session (it's stateful). Confirm no multi-worker
  session requirement.
- **O6 — Image source: template vs. arbitrary (§4.4).** Decision taken: support
  **both** — template mode as the default (operator allow-list, like
  substrate/agent-sandbox) and **arbitrary-image mode** as an authz-gated opt-in
  where a caller brings their own image. Remaining to confirm: (a) the authz
  rule for who may use arbitrary-image mode (broker/front-door, per token
  subject); (b) whether to additionally enforce registry allow-listing / image
  signatures; (c) whether arbitrary-image sessions share the general warm pool or
  get their own (looser warm guarantee). No worker change needed either way.
- **O7 — Identity + isolation mechanism (§6).** Confirm **SPIRE** for per-workload
  SPIFFE mTLS (recommended, GA-only) vs cert-manager fallback; and confirm the
  target cluster's CNI (VPC CNI) has **NetworkPolicy enforcement enabled** so
  workers can be locked to router-only ingress. Both are prerequisites for the
  "workers don't accept direct traffic" requirement.

---

## 9. Build vs. adopt agent-substrate

The user's steer: *if we can adapt or extend agent-substrate rather than build
our own controller, we should — but explore the tradeoff.* We explored it, and
the answer is already on record in our own repo: **adopting the substrate
runtime on managed EKS is blocked today, for the same no-alpha/no-beta reason in
the platform constraint above.** This is not speculation — it was proven during a
pre-flight Stage-0 evaluation
([`../../substrate/stage0/NOTES.md`](../../substrate/stage0/NOTES.md), NO-GO
2026-07-01; design rationale in
[`../../substrate/docs/DESIGN-NOTES.md`](../../substrate/docs/DESIGN-NOTES.md)).

### Why substrate can't run on EKS (the hard blocker)

Substrate's install deploys a **`podcertificate-controller`** (its mTLS
service-identity layer) and the installer *blocks* on it until specific
`ClusterTrustBundle`s exist. That controller requires the API server to serve two
`certificates.k8s.io` resources:

- `PodCertificateRequest` (KEP-4317) — gate `PodCertificateRequest`, alpha 1.34,
  beta 1.35, **off-by-default even at beta**, GA target 1.37.
- `ClusterTrustBundle` (KEP-3257) — gate `ClusterTrustBundle`, alpha 1.27, beta
  1.33, **off-by-default even at beta**, GA target 1.37.

Both sit behind feature gates that are **off by default even at beta** (the
SIG-auth exception), and **EKS does not let customers enable arbitrary
control-plane feature gates** (aws/containers-roadmap#512, open since 2019).
Empirically confirmed on live clusters in our account: EKS **1.31, 1.33, and even
1.35** all serve `certificates.k8s.io/v1` only — no `v1beta1`, no
`clustertrustbundles`, no `podcertificaterequests`. The 1.35 result is decisive:
`PodCertificateRequest` is *already beta* there and still not served. Earliest
plausible unblock is Kubernetes **1.37** (gates GA) *and* EKS shipping+enabling
it — not GA upstream as of that evaluation, so not a near-term option.

**This is exactly the constraint the user flagged:** EKS forbids alpha features
and beta-service enablement, and substrate's identity layer depends on precisely
those. So "adopt substrate wholesale on EKS" is **off the table for the
production track** — independent of how portable its *storage* layer is (S3 is
in fact supported and Pod-Identity-friendly; that was never the blocker).

> Note: a separate portability scan suggested substrate was "~85% portable"
> because S3/GCS is cleanly abstracted and GCP coupling is light. That scan
> **missed the pod-certificate/API-gate blocker**, which is load-bearing and
> fatal on EKS. Storage portability is real but irrelevant while the install
> can't complete. Trust the Stage-0 empirical result over the code-coupling scan.

### The only ways to run substrate itself, and why we reject them here

1. **`kind` spike (local only).** A local kind cluster *can* enable the gates, so
   substrate's demo runs there. Useful to evaluate the suspend/resume model on its
   merits, but it **does not prove EKS** and isn't a production path. Optional,
   out of scope for this PRD.
2. **Pod-certificate bypass** (`--auth-mode=jwt`, run without the cert
   controller). Uncertain, diverges from stock, and it's a yak-shave against a
   **v0.0.0** project the maintainers call "not ready for production." Not worth
   it.
3. **Wait for K8s 1.37+ on EKS.** A timeline we don't control. Revisit later.

Add to this what DESIGN-NOTES already established: substrate has **no auth /
multi-tenancy / GC**, **no shipped MCP** (the demo exposes a bespoke `/process`,
not MCP), and its router's MCP-session-continuity story is unverified. It is a
promising model, not a production foundation for us today.

### Decision: build our own control plane; borrow substrate's *patterns*, not its runtime

We build the control plane described in §§3–8 on top of **sandboxd** (proven on
our exact EKS gVisor nodes), and we **lift the cloud-neutral, self-contained
ideas** substrate got right — without taking on its EKS-blocked runtime:

- **`ActorResumer` singleflight resume-on-connect** (§5.1) — the pattern, and if
  license-compatible (Apache-2.0) the small self-contained Go logic, reimplemented
  against our KV. This is the concurrency substrate clearly solved and §8 flags as
  our top correctness risk; we borrow the design, not the deployment.
- **`WorkerPool` CRD shape** (§4.2) — a plain `apiextensions.k8s.io/v1` CRD (GA,
  no gates), modeled on substrate's WorkerPool/`atecontroller`.
- **Assignment-table model** — session/worker state in Valkey/ElastiCache with
  optimistic-CAS versioning, exactly substrate's Redis-authoritative approach
  (which, notably, is *not* the part that's blocked).
- **`ActorTemplate` → `Actor` → `Worker`** decomposition informs our
  `SandboxTemplate` → `Session` → `Worker` model.

What we deliberately do **not** take: substrate's `podcertificate-controller`/
mTLS identity (blocked on EKS — we use the existing broker/JWT front door
instead), its Envoy xDS/ExtProc router (we use a plain Go reverse proxy that
needs only GA APIs), and its `atelet`+`ateom-gvisor` worker (sandboxd already
fills that role and is validated on our nodes).

**Net:** the honest reuse is *architectural*, not *operational*. We keep sandboxd
as the worker, build a thin GA-only control plane, and treat substrate as a
well-designed reference we're free to mine (Apache-2.0) — revisiting a real
adoption only if/when K8s 1.37+ with those gates reaches EKS.

This decision makes the phased plan below **worker = sandboxd, control plane =
ours, ingress = GA-only**.

---

## 10. Phased delivery

Each phase is independently demoable and builds only on proven primitives.

- **P0 — WorkerPool provisioning.** WarmPool CR (plain GA CRD) + controller that
  maintains N worker pods from a parameterized deployment; `minIdle` respected.
  Workers register in KV. `pickIdleWorker` works. *(No routing yet; assign by
  hand.)* *(Optional, parallel, local-only: a `kind` spike of substrate to
  evaluate its suspend/resume model on its merits — explicitly NOT on the EKS
  critical path, per §9.)*
- **P1 — Assignment table + cold start.** KV schema + control-plane service.
  Router (thin) that does token→sid, cold-starts a session onto an idle worker
  via `/run`, records `RUNNING`, proxies. Continuation fast path. *(No
  suspend/restore yet.)*
- **P1.5 — Secure channel + isolation (§6).** Deploy SPIRE (server + agent
  DaemonSet on gVisor nodes); register SPIFFE IDs for worker/router/controller;
  link `go-spiffe` into sandboxd + router so the control channel is mTLS with peer
  enforcement. Apply a NetworkPolicy so worker pods accept ingress only from the
  router/controller (control API **and** DNAT data ports). Hard requirement, so
  it lands before suspend/restore rides the channel. *(cert-manager is the
  fallback if SPIRE is deferred — see O7.)*
- **P2 — Idle suspend.** Lifecycle controller: idle session → `/suspend` → S3 →
  `SUSPENDED`, worker returned to pool. `lastActiveAt` stamped by router.
- **P3 — Restore-on-connect.** The crux. Router miss on `SUSPENDED` → singleflight
  restore onto idle worker → `RUNNING` → proxy. CAS/version concurrency.
  Bounded wait + `503 Retry-After`. *(This closes the loop the user described.)*
- **P4 — HA + reconciliation.** Multiple router/control-plane replicas; worker
  crash reconciliation via `/capacity`; split-brain fencing; snapshot GC on TTL.
- **P5 — Hardening.** Thundering-herd backpressure, periodic checkpoints (opt-in),
  metrics/tracing across the resume path, runsc rolling-update drain.

**Definition of done (MVP = P0–P3, incl. P1.5):** a user presents a session
token, gets a sandbox; goes idle → it checkpoints and the worker is freed;
returns later with the same token → the router restores their checkpoint onto a
*different* worker and their state is intact — all transparent to the client
except latency. Throughout, the router↔worker channel is mTLS and workers accept
no direct client traffic (§6).

---

## 11. Mapping to what exists

| Control-plane need | sandboxd primitive it drives |
|---|---|
| Cold start a session | `POST /run` (image/cmd/env/ports/health from template) |
| Continuation | proxy to `workerPodIP:hostPort` (DNAT already set up) |
| Idle suspend | `/status` (`idle:true`) → `POST /suspend` → `snapshotURI` |
| Restore-on-connect | `POST /restore` (snapshotURI + image + ports/health) |
| Discard session | `POST /reset` or `DELETE /sandbox` |
| Find idle worker | `GET /capacity` (busy/idle + sandboxId) |
| Worker health/liveness | `GET /status`, `GET /healthz` |
| Version gate for restore | `/version`; `/restore` rejects mismatch |
| Secure control channel | mTLS via SPIRE SVIDs on sandboxd's `:8090` (new; §6.1) |
| Worker not directly reachable | NetworkPolicy: router-only ingress (new; §6.2) |

The worker layer is functionally complete for the MVP: the control plane is
orchestration, assignment state, and session-aware routing — no new *lifecycle*
capability is required from the worker for P0–P3. The **one** additive change to
sandboxd is wrapping its `:8090` listener in SPIRE-sourced mTLS (§6.1); the rest
of the security requirement (router-only ingress) is a NetworkPolicy applied
around the unchanged worker.
