# TDD — sandboxd Control Plane

Status: Draft (2026-07-05), on the `checkpoint-restore` branch.
Companion to [`PRD-control-plane.md`](PRD-control-plane.md) — read the PRD first.
The PRD is the *what/why* (and records decisions O1–O9); this TDD is the *how*:
the concrete CRD Go types, the KV schema + CAS protocol, the `Resume` contract,
component/repo layout, and build/tooling. It pins the things the PRD left
"illustrative" so implementation can begin without re-litigating shape.

Scope of this TDD: enough to build **MVP = P0–P3 (incl. P1.5 security)**. Later
phases (HA reconciliation, GC, hardening) are noted where they constrain MVP
choices but not fully specified.

---

## 1. Do we need this doc before coding? (yes, and why it's short)

The PRD is concrete on flow and decisions, but four things are expensive to get
wrong *after* code exists and must be pinned first:

1. **CRD Go types** — kubebuilder turns structs → CRDs; once applied with stored
   objects, renames/type changes are migrations. Pin them (§3).
2. **KV schema + CAS protocol** — the split-brain safety (PRD §8 risk 2) rests on
   it; design-level, not improvise-in-code (§4).
3. **`Resume(sid)` contract** — the seam between router and control plane (§5).
4. **Component topology + repo layout** — module boundaries, which binaries exist,
   merge-vs-split (§2).

Everything else in the PRD is codeable directly. This doc stays ~lean; it does not
restate the PRD.

---

## 2. Components, repo layout, build

### 2.1 Components (all new components are pure Go)

| Component | Role | Framework | Build | New? |
|---|---|---|---|---|
| **operator** | Reconciles `SandboxTemplate`/`WarmPool`/`Session` CRDs → worker Deployments; idle lifecycle; snapshot GC (later). **Hosts the control-plane `Resume` handler** for the MVP (merge, §2.3). | kubebuilder (controller-runtime) | ko | yes |
| **router** | Thin streaming reverse proxy: JWT validate → `sid` → KV read → fast-path proxy, or on miss call operator `Resume` (singleflight). | plain Go (`net/http`, `httputil.ReverseProxy`) | ko | yes |
| **sandboxd** | Worker agent: runs/checkpoints/restores nested gVisor sandboxes. Unchanged except adding mTLS on `:8090` (P1.5). | existing | Dockerfile (ships non-Go `runsc`) | exists |

Only three deployables for the MVP: **operator**, **router**, **sandboxd**
(+ SPIRE, + Valkey in-cluster as infra).

### 2.2 Repo layout

New top-level Go module for the control plane, sibling to `sandboxd/`:

```
checkpoint-restore/
  sandboxd/                      # existing worker (unchanged module)
  controlplane/                  # NEW module: github.com/jicowan/aio-sandbox/controlplane
    go.mod
    PROJECT                      # kubebuilder marker
    api/v1alpha1/                # CRD Go types (§3) — kubebuilder-generated scaffolding
      sandboxtemplate_types.go
      warmpool_types.go
      session_types.go
      groupversion_info.go
    internal/
      controller/                # reconcilers (kubebuilder)
        warmpool_controller.go
        session_controller.go    # lifecycle: idle→suspend, GC
      resume/                    # Resume(sid) workflow: pick worker, /run|/restore, CAS
      assign/                    # KV client + CAS (§4)
      workercache/               # in-mem idle-worker index (substrate pattern)
      sandboxdclient/            # typed HTTP client for sandboxd's API
    cmd/
      operator/main.go           # manager + Resume HTTP handler (merged, §2.3)
      router/main.go             # streaming proxy
    config/                      # kubebuilder manifests (CRDs, RBAC, deploy)
  shared/                        # NEW module: shared Go types (O9) — no k8s deps
    go.mod                       # github.com/jicowan/aio-sandbox/shared
    sbxapi/                      # sandboxd request/response types (portMap, health, ...)
    resumeapi/                   # Resume request/response types
```

**Why a `shared` module (O9).** Contract safety without protobuf: sandboxd, the
operator, and the router all import `shared/sbxapi` + `shared/resumeapi`, so a
field change breaks compilation on both sides. `shared` has **no Kubernetes or
cloud deps** so anything can import it cheaply. sandboxd migrates its in-package
`portMap`/`health` structs to `shared/sbxapi` (behavior-preserving; JSON tags
unchanged — see §5.3).

### 2.3 Control-plane responsibilities (in scope / out of scope)

"The control plane" is a set of **responsibilities**, independent of which binary
hosts them. It is the layer that **decides and records**; the router **forwards**;
sandboxd **executes**; the broker **translates MCP + owns the session's front
door**. Defining it as responsibilities (not a binary) is what makes the merge in
§2.4 safe — we merge the *deployable*, keep the *responsibilities* as a clean
internal boundary that can split later.

**In scope (control plane owns these):**

1. **Assignment authority.** Sole writer of the KV assignment table (§4) and its
   CAS protocol; owns session→worker and worker idle/busy state. This is the
   split-brain guard (PRD §8 risk 2). Router only *reads* KV; sandboxd doesn't
   touch it.
2. **The `Resume(sid)` workflow.** On a router miss: re-check KV → pick idle
   worker (`workercache`) → CAS `Resuming` → sandboxd `/run` (cold) or `/restore`
   (from `snapshotURI`) → poll `/status` ready → CAS `Running` → return worker IP.
   Idempotent/singleflight (§5.1).
3. **Worker-pool reconciliation.** Watch `WarmPool` CRDs → maintain the worker
   Deployment to `replicas`/`minIdle`; track worker registration in KV. (Inherently
   a kubebuilder controller.)
4. **Idle lifecycle.** Watch idle sessions (router-stamped `lastActiveAt`, O3) →
   sandboxd `/suspend` → record `Suspended` + `snapshotURI` → return worker to the
   idle pool. Also `reset`/`none` actions per the template's idle policy.
5. **Session projection + GC (mostly P4).** Reconcile the optional `Session` CR for
   observability; GC expired S3 snapshots on `ttlAfterSuspendSeconds`.

**Out of scope (explicitly NOT the control plane):**

- **Proxying / the data path** — router (§5.2). The control plane never sees
  request bodies or agent streams.
- **JWT validation on the hot path** — router validates (O4); the broker/front
  door mints. The control plane trusts the subject the router passes to `/resume`.
- **Running/checkpointing sandboxes itself** — sandboxd executes; the control
  plane only *calls* sandboxd's API.
- **Choosing the image / arbitrary-image authz** — the broker/front door decides
  (O6a); the control plane runs whatever the Session/`/resume` names.
- **MCP semantics, session mapping, warm-on-`initialize` trigger** — the broker
  (§9). The control plane exposes `Resume`/lifecycle; the broker decides *when* to
  call them.

Note that #3 and #4 are already CRD reconcilers, and #1/#2 need the same
informer-cached worker/session view — which is *why* the merge in §2.4 is natural
rather than a compromise.

### 2.4 Merge vs. split (control-plane service vs. operator)

**Decision (MVP): merge** the control-plane `Resume` workflow into the **operator
binary**. The operator already runs controller-runtime with informer caches over
Sandbox/Worker state; `Resume` needs exactly that state (idle-worker selection,
assignment CAS). One binary = fewer deployables, shared cache, simpler P0–P3.

The **router stays separate** (it's the data-plane hot path with different scaling
and blast-radius characteristics; PRD O2). So the split that matters — thin proxy
vs. assignment authority — is preserved: router calls the operator's `Resume`
endpoint over HTTP+mTLS.

Revisit splitting `Resume` into its own service if/when the data-path decision
volume warrants independent scaling (post-MVP). The `internal/resume` package is
written free of controller-runtime types at its boundary so it can be lifted into
its own `cmd/` later without a rewrite.

### 2.5 Build & tooling

- **kubebuilder** scaffolds the operator (`PROJECT`, CRD types, RBAC markers,
  controller-gen for CRD YAML + deepcopy).
- **ko** builds all pure-Go images (operator, router) — no Dockerfiles, matches
  the GA-only/minimal-deps ethos. sandboxd keeps its Dockerfile (ships `runsc`).
- **Go 1.26** (matches sandboxd's `go.mod`). controller-runtime + client-go pinned
  to a version supporting the target EKS (≥1.31).
- No protoc/gRPC toolchain (O9).

---

## 3. CRD Go types (pin before `make generate`)

API group **`sandboxd.io`**, version **`v1alpha1`** (our own serving version — a
plain `apiextensions.k8s.io/v1` CRD, GA, no feature gate; PRD platform
constraint). All three are namespaced.

> **Design note — explicit fields vs. embedded PodSpec (revisit later).** We
> deliberately model `SandboxTemplate` with explicit fields (image/cmd/env/ports/
> health/idle) that map onto sandboxd's `/run`, rather than embedding a full
> `corev1.PodSpec` the way agent-sandbox's SandboxTemplate does. Rationale: the
> *sandbox* is a nested gVisor workload driven via sandboxd's HTTP API, not a
> kubelet-scheduled pod — so most PodSpec fields are meaningless for it. Only the
> **worker pod** is a real pod, and it's operator-managed + generic. The seam that
> *is* legitimately worker-pod shaping is **scheduling** (nodeSelector/tolerations/
> anti-affinity), which we expose via `SandboxTemplate.spec.scheduling`
> (`SchedulingSpec`) with gVisor defaults. **Future direction:** as more worker-pod
> knobs are needed (resources already exist; later: sidecars, volumes, security
> context), consolidate them under a small worker-pod-template sub-struct rather
> than growing many top-level fields — moving toward agent-sandbox's embedded-spec
> shape for the *worker* pod specifically, while keeping the *sandbox* run-config
> explicit. Not needed for the MVP.

### 3.1 SandboxTemplate

```go
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=sbxt
type SandboxTemplate struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec SandboxTemplateSpec `json:"spec"`
}

type SandboxTemplateSpec struct {
    // Image run AS THE SANDBOX (nested gVisor workload), not the worker.
    // +kubebuilder:validation:Required
    Image string `json:"image"`
    Cmd  []string `json:"cmd,omitempty"`
    Env  []string `json:"env,omitempty"`
    // +kubebuilder:validation:MinItems=0
    Ports  []PortMap `json:"ports,omitempty"`
    Health *Health   `json:"health,omitempty"`
    Idle   IdlePolicy `json:"idle,omitempty"`
    // Worker sizing hint → WarmPool pod resources.
    Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

type PortMap struct {
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=65535
    Container int `json:"container"`
    Host      int `json:"host,omitempty"` // 0 → default to Container
}

type Health struct {
    // +kubebuilder:validation:Enum=none;cold;restore
    RestartPolicy string `json:"restartPolicy,omitempty"`
    // +kubebuilder:validation:Enum=none;tcp;http
    Probe     string `json:"probe,omitempty"`
    ProbePort int    `json:"probePort,omitempty"`
    ProbePath string `json:"probePath,omitempty"`
}

type IdlePolicy struct {
    // 0 = never auto-suspend
    // +kubebuilder:default=300
    TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
    // +kubebuilder:validation:Enum=suspend;reset;none
    // +kubebuilder:default=suspend
    Action string `json:"action,omitempty"`
}
```

`PortMap`/`Health` deliberately mirror sandboxd's on-wire JSON (`container`/`host`;
`restartPolicy`/`probe`/`probePort`/`probePath`) so the operator can hand them
straight to sandboxd. These are the CRD-facing copies; the *client* wire types
live in `shared/sbxapi` (§5.3) — identical JSON tags, converted 1:1.

### 3.2 WarmPool

```go
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=wp
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:printcolumn:name=Replicas,type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name=Idle,type=integer,JSONPath=`.status.idle`
// +kubebuilder:printcolumn:name=Busy,type=integer,JSONPath=`.status.busy`
type WarmPool struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   WarmPoolSpec   `json:"spec"`
    Status WarmPoolStatus `json:"status,omitempty"`
}

type WarmPoolSpec struct {
    // +kubebuilder:validation:Required
    TemplateRef LocalRef `json:"templateRef"`
    // +kubebuilder:default=1
    // +kubebuilder:validation:Minimum=0
    Replicas int32 `json:"replicas"`
    // Keep at least N idle workers ready to accept a session.
    // +kubebuilder:default=1
    // +kubebuilder:validation:Minimum=0
    MinIdle int32 `json:"minIdle,omitempty"`
    // Marks this pool as the landing zone for arbitrary-image sessions (O6c).
    // Such a pool relaxes warm guarantees and is fed by a generic base image.
    ArbitraryImage bool `json:"arbitraryImage,omitempty"`
}

type WarmPoolStatus struct {
    Replicas int32  `json:"replicas"`
    Idle     int32  `json:"idle"`
    Busy     int32  `json:"busy"`
    Selector string `json:"selector,omitempty"` // for the scale subresource / HPA
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type LocalRef struct{ Name string `json:"name"` }
```

The **scale subresource** makes the pool HPA-drivable (PRD §4.2). The controller
reconciles a Deployment of worker pods parameterized from
`worker-deploy.yaml` (image = sandboxd, not the template image — workers are
generic; the sandbox image is supplied at `/run` time).

### 3.3 Session

Per O1 (KV-authoritative), the `Session` CR is an **optional reconciled
projection for observability** — the assignment table (§4) is the source of
truth. We still define the CRD (useful for `kubectl get sessions`, and it gives
the front door a declarative create path), but the hot path never reads it.

```go
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=sess
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name=Phase,type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name=Worker,type=string,JSONPath=`.status.workerPodIP`
type Session struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"` // .metadata.name == session ID
    Spec   SessionSpec   `json:"spec"`
    Status SessionStatus `json:"status,omitempty"`
}

type SessionSpec struct {
    // WHAT TO RUN — exactly one of PoolRef (template mode) or inline Image (arbitrary).
    PoolRef *LocalRef `json:"poolRef,omitempty"`
    // Arbitrary-image mode (O6): authz-gated at the front door before creation.
    Image string   `json:"image,omitempty"`
    Cmd   []string `json:"cmd,omitempty"`
    Env   []string `json:"env,omitempty"`
    Ports []PortMap `json:"ports,omitempty"`

    // Opaque identity the router matches the JWT-derived subject against (O4).
    Subject   string          `json:"subject,omitempty"`
    Lifecycle SessionLifecycle `json:"lifecycle,omitempty"`
}

type SessionLifecycle struct {
    IdleTimeoutSeconds     int `json:"idleTimeoutSeconds,omitempty"`     // overrides template
    TTLAfterSuspendSeconds int `json:"ttlAfterSuspendSeconds,omitempty"` // GC the S3 checkpoint
}

type SessionStatus struct {
    // +kubebuilder:validation:Enum=Absent;Running;Suspending;Suspended;Resuming
    Phase       string `json:"phase,omitempty"`
    WorkerPodIP string `json:"workerPodIP,omitempty"`
    SnapshotURI string `json:"snapshotURI,omitempty"`
    Image       string `json:"image,omitempty"`
    LastActiveAt *metav1.Time `json:"lastActiveAt,omitempty"`
    Conditions  []metav1.Condition `json:"conditions,omitempty"`
}
```

Validation to enforce in a webhook or at the front door (not expressible purely
in CRD schema): **exactly one of `poolRef` / `image`** set; `image` requires the
arbitrary-image entitlement (O6a).

---

## 4. Assignment table (KV) — schema + CAS protocol

**Store:** **Valkey, run in-cluster** (a Deployment + Service) for both dev and
initial production. Decision: minimize dependence on AWS-native services — the
only AWS dependency we keep is **S3** (itself easily replaceable with any
S3-compatible object store). ElastiCache/MemoryDB remains a drop-in later option
(same Valkey/Redis protocol) but is not required and not assumed.

Authoritative for session + worker state (O1). **The operator is the SOLE
writer** — `Resume`/lifecycle *and* worker discovery (§4.3) all write through the
operator. The **router only reads** it per request. sandboxd never touches KV (no
Valkey credentials on the pod that hosts untrusted code). Single-writer is what
makes the CAS invariants (§4.2) hold.

### 4.1 Keys & records

```
session:<sid>      → JSON SessionEntry     (the assignment record)
worker:<podName>   → JSON WorkerEntry       (registration + busy/idle)
pool:<pool>:idle   → SET of <podName>       (idle-worker index for O(1) pick)
lock:resume:<sid>  → short-TTL token        (optional cross-replica singleflight; §5.2)
```

```go
// shared/resumeapi (also used by router + operator)
type SessionEntry struct {
    SID         string   `json:"sid"`
    State       string   `json:"state"`        // Absent|Running|Suspending|Suspended|Resuming
    Pool        string   `json:"pool"`
    WorkerPodIP string   `json:"workerPodIP"`  // set while Running/Resuming
    WorkerPod   string   `json:"workerPod"`
    Image       string   `json:"image"`        // replayed on restore (arbitrary-image safe)
    SnapshotURI string   `json:"snapshotURI"`  // current checkpoint (one lineage; overwritten)
    Ports       []PortMap `json:"ports"`
    Version     int64    `json:"version"`      // CAS token
    LastActiveAt int64   `json:"lastActiveAt"` // unix ms; stamped by router (O3)
}

type WorkerEntry struct {
    Pod     string `json:"pod"`
    Pool    string `json:"pool"`
    PodIP   string `json:"podIP"`
    State   string `json:"state"`   // idle|busy
    SID     string `json:"sid"`     // "" when idle
    Version int64  `json:"version"`
}
```

### 4.2 CAS protocol (the split-brain guard, PRD §8 risk 2)

Every state-changing write is a **compare-and-set on `Version`**. Implementation
options in Valkey: `WATCH`/`MULTI`/`EXEC` optimistic transaction, or a Lua script
that checks-and-sets atomically (preferred — one round trip, no watch churn). The
invariant: *a write succeeds only if the record's `Version` matches what the
writer read; on success `Version++`; on mismatch the writer reloads and retries.*

Two assignment invariants the CAS enforces:
1. **One session → one worker.** Moving a session to `Resuming/Running` also flips
   the chosen worker `idle→busy` in the **same logical transaction** (Lua script
   over both keys). If the worker was already `busy`, the pick fails → reselect.
2. **One worker → one session.** A worker leaves the `pool:<pool>:idle` set
   atomically as it's claimed, so two concurrent `Resume`s can't grab it.

Reconciliation (post-MVP, P4) repairs drift against ground truth using sandboxd
`/capacity`; the MVP relies on CAS + the operator being the sole writer.

### 4.3 Worker discovery (operator-written, not self-registered)

Workers do **not** self-register. The operator runs a **pod-label informer**
(controller-runtime watch on worker pods, e.g. `app=sandboxd-worker` +
pool label) and writes/updates `WorkerEntry` in KV on pod add/update/delete:

- Pod **Ready** → upsert `WorkerEntry{state:idle}`, add to `pool:<pool>:idle`.
- Pod **deleted / NotReady** → remove from the idle set and delete/mark the entry
  (any session it held falls to `SUSPENDED`/reconcile per §5.3 / PRD §5.3).
- Pod IP / pool come from the pod object; no KV client or Valkey credentials on
  sandboxd.

Rationale (decided): keeps the operator the **sole KV writer** (preserving the
CAS invariants in §4.2), keeps sandboxd unchanged, and reuses the informer cache
the operator already maintains. sandboxd's `/capacity` + `/status` stay the
ground truth the operator reconciles against — they are *read*, never a KV write
path from the worker.

**Watch scoping (churn safety).** The pod watch is scoped at the **cache /
ListWatch level**, not merely by a reconcile predicate — the manager's Pod cache
carries a label selector (`cache.Options.ByObject[*Pod]{Label:
sandboxd.io/app=worker}`), so the API server only ever streams *worker* pods to
the operator. Cluster-wide churn of unrelated pods never enters our informer or
memory. (A `predicate.LabelSelectorPredicate` alone would filter only reconcile
events while still watching/caching every pod — the wrong layer; we deliberately
scope the cache instead.) Consequence: the operator's cached client can read only
worker pods, which is all it needs.

**Future scale option — watch EndpointSlices, not Pods (deferred).** Front each
pool's workers with a **headless Service** and watch its **EndpointSlices** rather
than Pods. EndpointSlices already carry, per ready endpoint, the IP + a
`targetRef.name` (= pod name) with readiness precomputed — exactly the
`{podName, podIP, ready}` tuple `WorkerEntry` needs — in far fewer objects than
pods, on a path (kube-proxy/CoreDNS) built for high churn. Cost: a headless
Service per pool + endpoint→pod indirection. Adopt if worker-pool size or churn
ever justifies it; the label-scoped pod watch above is adequate for a bounded
warm pool (tens of workers). Deferred to a P4 hardening phase.

---

## 5. The `Resume` contract & request paths

### 5.1 Router → operator: `POST /resume`

HTTP+JSON over mTLS (O9). Called by the router **only on a KV miss**.

```
POST /resume            (operator, internal, mTLS)
req:  { "sid": "sess-7f3a", "subject": "app/acme/user/alice/session/7f3a" }
resp: 200 { "workerPodIP": "10.0.3.95", "state": "Running" }
      503 { "error": "no capacity", "retryAfterSeconds": 5 }   // pool exhausted
      409 { "error": "version conflict" }                       // caller may retry
```

Idempotent: concurrent `/resume` for the same `sid` converge (CAS + optional
`lock:resume:<sid>`). The handler implements the control-plane half of PRD §5.1:
re-check KV → `pickIdleWorker` → CAS to `Resuming` → sandboxd `/run` or `/restore`
→ poll `/status` until ready (bounded by the resume deadline, O8) → CAS to
`Running` → return `workerPodIP`.

### 5.2 Router per-request algorithm (concrete)

```go
func handle(w, r):
    tok := bearer(r)                       // Authorization header
    claims := validateJWT(tok, jwks)       // local, cached (O4); 401 on fail
    sid := deriveSID(claims)               // subject + session claim
    e := kv.GetSession(sid)                // one read
    if e.State == "Running" && alive(e.WorkerPodIP):
        stampActive(sid)                   // O3 bracketing: mark active (start)
        proxyStream(w, r, e.WorkerPodIP)   // §5.4, fast path
        stampActive(sid)                   // bracketing: mark active (end)
        return
    ip := singleflight(sid, func() {        // dedup concurrent misses in-replica
        return operator.Resume(sid, claims.Subject)   // mTLS call, bounded (TTFB clock)
    })
    if ip == "" { writeRetryAfter(w); return }   // 503, O8
    proxyStream(w, r, ip)
```

`singleflight` = `golang.org/x/sync/singleflight` per-replica; the operator's CAS
covers cross-replica. `proxyStream` is the streaming proxy (§5.4/PRD §5.5).

### 5.3 sandboxd client + shared types

`internal/sandboxdclient` is a typed HTTP client over sandboxd's existing API,
using `shared/sbxapi` request/response structs. **sandboxd refactor (small,
behavior-preserving):** move its in-package `portMap` (network.go) and `health`
(supervisor.go) structs into `shared/sbxapi` with **identical JSON tags**
(`container`/`host`; `restartPolicy`/`probe`/`probePort`/`probePath`/
`idleTimeoutSec`), and import them back. No wire change, so existing deployments
and the v40 image behavior are unaffected; it just makes the contract shared and
compiler-checked.

### 5.4 Streaming proxy (PRD §5.5) — concrete requirements

```go
proxy := &httputil.ReverseProxy{
    Rewrite: func(pr *httputil.ProxyRequest) { pr.SetURL(workerURL) },
    FlushInterval: -1,                 // flush immediately: SSE/chunked stream through
    Transport: mtlsTransport,          // SPIRE-sourced client cert (O7)
}
```

- `FlushInterval: -1` is mandatory — without it SSE/chunked agent output buffers.
- Do **not** set a short `Transport.ResponseHeaderTimeout` that would kill a slow
  first byte incorrectly; the **TTFB clock** (O8) is enforced around `Resume`, not
  the proxy. The **response clock** is an **idle** read timeout on the streaming
  body (no bytes for N seconds → cancel), not a wall-clock cap.
- Propagate `r.Context()` cancellation to the upstream so client disconnect tears
  down the sandbox-side stream.
- Pass `Content-Type: text/event-stream` and chunked encoding through unmodified.

---

## 6. Security wiring (P1.5, PRD §6)

### 6.1 SPIRE — a NEW cluster prerequisite (not yet installed)

SPIRE is **not currently in the cluster** and must be installed as infra
(alongside Valkey) **before P1.5**. It is a prerequisite, not application code.

**Install:** SPIRE **server** (StatefulSet + its own datastore; a PVC-backed
SQLite is fine for MVP, RDS/managed later) and SPIRE **agent** (DaemonSet on the
gVisor nodes). Deploy via the upstream `spire` Helm chart or plain manifests
(GA-only; no alpha/beta APIs).

**Trust domain:** `spiffe://sandboxd` (matches the SPIFFE IDs already named in the
PRD).

**Attestation (all GA primitives):**
- **Node attestation:** `k8s_psat` — the agent proves node identity with a
  projected ServiceAccount token; server verifies via `TokenReview`.
- **Workload attestation:** the `k8s` workload attestor keys on
  ServiceAccount / namespace / pod labels. Registration entries:
  - `spiffe://sandboxd/worker/<pool>` → sandboxd worker pods (by SA/label)
  - `spiffe://sandboxd/router` → router pods
  - `spiffe://sandboxd/operator` → operator pods

**The gVisor-specific point (important):** the SPIRE agent hands SVIDs over a
**Workload API unix socket mounted into each pod**, and the identity it issues is
the **worker pod (sandboxd) identity — NOT the nested gVisor sandbox**. That is
exactly what we want: mTLS is control-plane↔sandboxd; the untrusted nested
workload never receives a SPIFFE identity. Mount the agent socket
(`/run/spire/agent-sockets/…` hostPath, or the CSI driver) into sandboxd, router,
and operator pods — this is at the pod/host level and is unaffected by the nested
sandbox.

**Consumption:** components fetch + auto-rotate SVIDs via
`go-spiffe/v2/workloadapi` and enforce peer SPIFFE IDs in their TLS config.

### 6.2 mTLS + NetworkPolicy

- **sandboxd**: wrap the `:8090` listener in mTLS requiring a router/operator SVID
  (the one additive sandboxd change). Its outbound calls (S3) stay as-is.
- **operator `/resume`**: mTLS, requires the router SVID.
- **router→sandboxd** data-plane origination: mTLS client cert from the router SVID
  (§5.4 `mtlsTransport`).
- **NetworkPolicy** (`networking.k8s.io/v1`, VPC CNI enforced, O7): worker pods
  accept ingress only from router/operator pods — covers `:8090` **and** the DNAT
  data ports; egress limited to S3/DNS.

### 6.3 Sequencing / interim posture

P0–P1 can run **before** SPIRE lands, with **NetworkPolicy-only** isolation
(router-only ingress). Be explicit that this is *isolation, not encryption*: the
"encrypted channel" requirement (PRD §6) is only **met at P1.5** when SPIRE mTLS is
in. Don't mark P1 as "secure-complete" — it's network-isolated but not yet
mutually authenticated or encrypted.

---

## 7. Broker integration (largely unchanged)

The existing broker (`broker/broker_mcp.py`, Python/FastAPI MCP broker) stays in
place and stays the **MCP front door**. It already: answers MCP `initialize`
locally, maps `mcp-session-id` → a sandbox, checks JWT audience, forwards to a
router, and streams SSE/JSON responses through. Today it targets
`sandbox-router-svc` with `X-Sandbox-*` headers and creates sandboxes via the
agent-sandbox client (`create_sandbox`). **Only its backend-facing half changes;
its MCP-facing half is untouched.**

Three contained changes:

1. **Downstream target → our router.** Point `AIO_ROUTER_URL` at our router
   Service instead of `sandbox-router-svc`. (Config, not code.)
2. **Addressing → pass the JWT, drop `X-Sandbox-*`.** Per O4 the router derives
   `sid` from the JWT it already receives from agentgateway, so the broker
   forwards the bearer token rather than constructing `X-Sandbox-ID`/`-Namespace`/
   `-Port` headers. Net simplification.
3. **Lifecycle → warm-on-`initialize` via the control plane.** Replace the
   agent-sandbox `create_sandbox` call with create/resume of a **Session** through
   the control plane (or lazily via the first request). Because the broker already
   does its lifecycle work in the `initialize` handler, this is the O8
   proactive-warming hook landing exactly where lifecycle already lives — a swap
   of the call, not new plumbing.

**Boundary (who owns what):** the broker owns MCP semantics, the
**MCP-session ↔ our-session** correspondence, JWT audience checks, and the
*decision* to warm-on-`initialize`. The control plane owns `Resume`/lifecycle
mechanics; the router owns proxying + `sid` derivation. The broker never talks to
sandboxd or the KV directly.

**Stays as-is:** language (Python), MCP handling, local `initialize` answer,
`protocolVersion` rewrite, SSE/streaming passthrough (which composes with the
router's streaming proxy, §5.4), session bookkeeping.

Open: whether warm-on-`initialize` calls the operator's `Resume` directly or just
creates a `Session` CR and lets the controller warm it — ties into O8 and the
front-door validation question (§9).

---

## 8. Phase → work mapping (what to build, in order)

Mirrors PRD §10; each item names the package/artifact.

- **P0 — WarmPool provisioning.** `api/v1alpha1` types (§3) → `make generate`;
  `warmpool_controller.go` reconciles the worker Deployment to `spec.replicas`
  (defer `minIdle` autoscaling to P4 — the field exists but isn't enforced yet);
  operator's pod-label informer writes `WorkerEntry` on pod add/ready/delete
  (§4.3) + `workercache`. Valkey deployed in-cluster. Assign by hand (no router).
- **P1 — Assignment table + cold start.** `assign/` (KV + CAS, §4); `resume/`
  cold-start path (`/run`); `router` fast path + `/resume` call; JWT validate.
- **P1.5 — Security.** SPIRE **install** (new prereq, §6.1) + `go-spiffe` in
  sandboxd/router/operator; mTLS on `:8090` and `/resume`; NetworkPolicy (§6).
- **P2 — Idle suspend.** `session_controller.go` idle watch (O3 bracketing via
  router-stamped `LastActiveAt`) → sandboxd `/suspend` → `Suspended` + snapshotURI.
- **P3 — Restore-on-connect.** `resume/` `Suspended` path (`/restore` with
  `SnapshotURI`+`Image`); singleflight + CAS; TTFB deadline + `503 Retry-After`.
- **Broker cutover (with P1/P3).** Repoint broker to our router, pass JWT, and
  wire warm-on-`initialize` (§7). Small Python diff; can land incrementally.
- **P4+ — HA, reconciliation (`/capacity`), snapshot GC, arbitrary-image pool
  (O6c), registry policy (O6b).**

Infra prerequisites (before the phases that need them): **Valkey (in-cluster)**
(P0/P1), **SPIRE** (P1.5).

---

## 9. Open items this TDD does NOT close (carried from PRD)

- **O6a/O6b** — arbitrary-image authz rule + registry allow-list/signatures
  (front-door concern; gates P4 arbitrary-image pool).
- **O8 numbers** — concrete TTFB/resume deadline (~15s?) and response idle timeout;
  set against the real client timeout once the front door is chosen.
- **Webhook vs. front-door validation** — enforcing "exactly one of poolRef/image"
  and the arbitrary-image entitlement. MVP: enforce at the front door / operator
  admission logic; a validating webhook is a P4 hardening option.
- **Merge→split of `Resume`** — kept mergeable (§2.4); split only if scaling needs it.
- **Multi-namespace workers (future).** Today the operator assumes a single
  namespace for SandboxTemplate/WarmPool/Session *and* worker pods
  (`--resume-namespace`, and the discovery/prune sweep looks pods up there).
  Eventually workers (and pools) should be able to run in **different namespaces**
  — e.g. per-tenant namespaces, or workers isolated from control-plane objects.
  Requires: namespace on the `WorkerEntry`/pool, the discovery watch spanning
  namespaces (or per-namespace caches), and RBAC across them. Not needed for the
  MVP; the KV records already key on pod name so the data model mostly carries
  over.
