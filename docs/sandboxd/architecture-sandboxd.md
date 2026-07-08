# Architecture — sandboxd control plane (router, operator, Valkey, workers)

This is the architecture of the session‑teleport control plane behind the broker:
the **router**, the **operator**, the **Valkey** assignment table, the **sandboxd
worker** agent, and **S3**. It supersedes the pre‑build design narrative in
`checkpoint-restore/docs/ARCHITECTURE.md` — that file's "RESOLVED MODEL" section
remains conceptually accurate, but this document describes the system as shipped.

For the auth front door on the other side of the broker, see
[architecture-broker.md](architecture-broker.md). For field‑level CRD detail, see
[admin-guide-crds.md](admin-guide-crds.md).

## The core idea: decouple session state from the pod

A user's **session** (RAM + filesystem of a nested gVisor sandbox) is decoupled
from any particular pod. It can be checkpointed to S3, its worker freed, and later
restored onto *any* interchangeable warm worker on *any* node — with state intact.
This is "teleport." It gives temporal oversubscription: many sessions, fewer
workers, idle sessions parked in S3.

## The load‑bearing distinction: worker vs. sandbox

| | **Worker** | **Sandbox (session)** |
|---|---|---|
| Kubernetes object? | **Yes** — a normal Pod from a Deployment the operator manages | **No** — a `runsc` root container *inside* a worker pod |
| Placed by | kube‑scheduler (worker pod resource requests) | the operator picks *which idle worker* (assignment only) |
| Has pod IP / DNS? | Yes (VPC‑CNI pod IP) | No — reached via the worker at a stable interior IP |
| Portable? | No (it's just capacity) | **Yes** — teleports across workers via S3 |

Everything follows from this. The schedulable/accountable unit is a real pod (the
worker); the portable stateful unit is the sandbox. The operator therefore does
**not** reimplement kube‑scheduler — it keeps a lightweight assignment table and
does O(1) "pick any idle worker in the matching pool," because kube‑scheduler
already reserved room when it placed each worker (one sandbox per worker).

## Components

```
                    ┌────────────────────────── namespace: sandboxd-controlplane-system ─────────┐
   broker           │                                                                            │
   (X-Session-ID,   │   ┌──────────┐   resume    ┌────────────┐        ┌─────────┐               │
    X-Session-Pool) │   │  router  │────────────►│  operator  │◄──────►│ Valkey  │  assignment   │
   ────────────────►│──►│  (proxy) │  (POST      │(controller)│  KV    │ (KV)    │  table        │
        HTTP /mcp   │   └────┬─────┘   /resume)  └─────┬──────┘        └─────────┘               │
                    │        │ proxy to                │ manages Deployments,                    │
                    └────────┼─────────────────────────┼─────────────────────────────────────────┘
                             │ worker podIP:port       │ writes worker/session KV
                             ▼                         ▼
                    ┌──────────────────────── namespace: default (pools/sessions/workers) ───────┐
                    │   sandboxd worker pod (privileged, gVisor node)                            │
                    │   ┌──────────────┐   runsc run/checkpoint/restore   ┌────────────────────┐ │
                    │   │  sandboxd    │─────────────────────────────────►│ nested gVisor      │ │
                    │   │  agent :8090 │                                  │ sandbox (workload) │ │
                    │   └──────┬───────┘                                  └────────────────────┘ │
                    │          │ checkpoint / restore                                            │
                    └──────────┼─────────────────────────────────────────────────────────────────┘
                               ▼
                              S3   sandboxes/<sid>/<snap>/{checkpoint.img,pages.img,pages_meta.img,config.json}
```

### Router (`cmd/router`)

A thin, stateless, session‑aware reverse proxy. Per request:

1. Resolve identity from headers (`X-Session-ID`, optional `X-Session-Subject`,
   `X-Session-Pool`). Missing session id → `401`.
2. **Fast path:** read the session from Valkey; if it's `Running` on a **live**
   worker, stream‑proxy to that worker's pod IP:port. Liveness is verified against
   the `worker:<pod>` KV entry (must exist, be `busy`, and be bound to this
   session) — mirroring the operator's fencing — so the router never proxies to a
   crashed/pruned worker.
3. **Miss / stale:** singleflight a `POST /resume` to the operator, then proxy.
   As a safety net, if a fast‑path upstream connection fails *before any bytes
   reach the client*, the router buffers a small body and falls through to a
   resume once (transparent teleport instead of a 502).
4. `POST /_warm` is a protocol‑agnostic primitive: "ensure this session is
   Running" (resume if needed) and return `204` — no payload proxied. This is what
   the broker calls on `initialize`.

The router **only reads** the assignment table and stamps `lastActiveAt` (for idle
detection). It never assigns workers or writes session state — that is the
operator's exclusive job. Streaming uses `FlushInterval: -1` so SSE / chunked MCP
output flows token‑by‑token.

### Operator (`cmd`, kubebuilder)

The control brain. It is the **sole writer** of the Valkey assignment table.
Responsibilities:

- **Reconcile pools → worker Deployments.** A `WarmPool` (bound to a
  `SandboxTemplate`) becomes a Deployment of worker pods. `minIdle` autoscaling
  raises the effective replica count to `max(spec.replicas, busy + minIdle)` so
  warm headroom is maintained.
- **Worker discovery.** A label‑scoped pod informer (`sandboxd.io/app=worker`)
  writes a `worker:<pod>` KV entry when a worker becomes Ready, and removes it when
  the pod dies. A 30s prune loop reconciles missed deletes.
- **Resume workflow.** On `POST /resume`, claim an idle worker (atomic SPOP), CAS
  the session to `Resuming`, call the worker's `/run` (cold start from the pool's
  template image) or `/restore` (teleport from an S3 snapshot), wait for ready, CAS
  to `Running`. Fencing (`workerHolds`) prevents split‑brain; if a live sandbox
  already exists it is preferred over restoring.
- **Idle suspend + checkpoints.** A sweeper suspends idle sessions (checkpoint →
  S3, free the worker). Optional periodic background checkpoints (per‑template
  `checkpointIntervalSeconds`) bound crash loss.
- **GC.** Optional TTL/orphan snapshot reaper, running under a *separate,
  least‑privilege* S3 identity (list + delete on `sandboxes/*` only).
- **Lazy Session creation.** On a resume for an unknown session id, the operator
  creates a `Session` object from the pool hint header.

The operator uses CAS‑on‑version (Lua) for every KV write — that's the split‑brain
guard, since it is the single writer but must be safe across restarts/HA.

### Valkey (assignment table)

An in‑cluster Redis‑compatible KV store — the source of truth for *assignment*,
not a cluster/resource model. Keys:

| Key | Value | Writer |
|-----|-------|--------|
| `session:<sid>` | JSON `SessionEntry` (state, pool, workerPod, workerPodIP, snapshotURI, ports, health, version, lastActiveAt) | operator (CAS); router does advisory `StampActive` |
| `worker:<pod>` | JSON `WorkerEntry` (pod, pool, podIP, state idle/busy, sid, version) | operator only |
| `pool:<pool>:idle` | Redis SET of idle worker pod names | operator only |

Keys are namespace‑agnostic today (bare pod/pool/session names) — the control
plane assumes a single worker namespace (see "Boundaries" below).

### sandboxd worker (`checkpoint-restore/sandboxd`)

A single static Go binary in a distroless image, running as PID 1 in a
**privileged** pod on a gVisor node. It drives a pinned `runsc` binary to run,
checkpoint, and restore nested gVisor sandboxes, and does its own S3 I/O. It
exposes an HTTP API on `:8090` (`/run`, `/checkpoint`, `/restore`, `/suspend`,
`/reset`, `/status`, `/capacity`, `/logs`, `/healthz`, `/version`, `/metrics`).

Key mechanics:

- **Nested gVisor:** `runsc run -detach` with `--network=sandbox` (checkpointable
  netstack), a per‑sandbox writable overlay outside the rootfs
  (`-overlay2=root:dir=…`), and `--directfs=false` (directfs needs `/proc/self/uid_map`,
  absent when nested).
- **Rootfs from the node's containerd cache** (hostPath mount of the containerd
  socket + data dir), so image layers stay node‑local; only the checkpoint travels.
- **Checkpoint = atomic RAM + FS** via a single `runsc checkpoint` on the overlay
  (optionally compressed), uploaded to `s3://<bucket>/sandboxes/<sid>/<snap>/`.
- **Restore** re‑establishes the veth/netns with the *same* interior IP so the
  session is reachable at the same `podIP:hostPort` after teleport.
- **Supervisor** does readiness probing (dials the interior IP), restart policy,
  and idle detection.
- **Console streaming** (opt‑in per pool via `SANDBOXD_STREAM_CONSOLE`) surfaces
  the nested workload's stdout/stderr to the worker's `kubectl logs`, sanitized and
  capped.

### S3

Per‑session checkpoint store. Only the checkpoint (RAM pages + FS delta + the
exact `config.json`) travels — the base image rootfs stays on the node's
containerd cache. For a browser‑class workload the S3 round‑trip dominates resume
latency, which is why base images are kept node‑local and pools are pre‑warmed.

## Networking / addressing

- **Worker pod:** ordinary VPC‑CNI pod IP; the only real address. The router dials
  `workerPodIP:hostPort`.
- **Sandbox:** lives in a persistent interior netns inside the worker, joined by a
  veth pair with a **stable interior IP** (`169.254.17.2`). nftables DNAT maps
  `podIP:hostPort → 169.254.17.2:containerPort`; a masquerade rule handles egress.
- The interior IP is constant across restores, so the *address* survives teleport
  even though **sockets do not** — every resume is a fresh MCP session (reconnect
  model). The broker/client re‑`initialize`; no live TCP/SSE is preserved.
- A sandbox has **no** cluster IP, DNS name, or Service. Only workers are
  DNS‑visible. Routing to a sandbox is entirely via the assignment table
  (`sid → workerPod, workerPodIP`), not DNS.

## Per‑session AWS IAM credentials (optional)

A sandbox can assume an AWS IAM role scoped to its session, using the standard AWS
SDK container‑credentials provider — no workload code change. Off unless a pool's
template sets `iam.roleArn`.

- **How:** the worker runs a small **credential vendor** HTTP server on
  `169.254.170.2:8091`. This address is chosen because AWS SDKs only allow
  `AWS_CONTAINER_CREDENTIALS_FULL_URI` to point at loopback / `169.254.170.2` /
  `169.254.170.23`; `.2` (the ECS task‑role address) is used because `.23` is the
  EKS Pod Identity agent address the *worker's own* SDK needs. The vendor address
  is added to the host veth so the sandbox reaches it on‑link via its default
  gateway. On `/run` the operator passes the session's role ARN; the worker
  registers it, injects `AWS_CONTAINER_CREDENTIALS_FULL_URI` +
  `AWS_CONTAINER_AUTHORIZATION_TOKEN` into the sandbox, and on request calls
  `sts:AssumeRole` (tagged `sandbox-session=<sid>`), caching + refreshing before
  expiry.
- **Identity boundary:** the sandbox gets *only* its session role — never the
  worker's checkpoint identity, never the node role (IMDS is unreachable from the
  sandbox netns). The vendor's own `AssumeRole` runs as the worker's Pod Identity,
  which must be granted `sts:AssumeRole` on the allow‑listed session roles; the
  target role's trust policy names the worker role and can require the session tag.
- **Auth:** the per‑session token is `HMAC(fleet‑key, sid)` — deterministic, so the
  same value is computed on any worker. That's what makes it **teleport‑safe**:
  after a session restores on a different worker, the vendor re‑registers the role
  and the injected env (baked into the checkpoint) still matches, so AWS access
  resumes with no client involvement. Credentials themselves are never
  checkpointed — only the *ability to fetch* teleports.
- Enabled by the operator flag `--cred-token-secret` (fleet HMAC key from a Secret)
  + per‑pool `SandboxTemplate.spec.iam.roleArn` (a `Session.spec.iam.roleArn`
  overrides it). See [PRD-sandbox-iam-credentials.md](../PRD-sandbox-iam-credentials.md).

## Session lifecycle (state machine)

```
        POST /resume (miss)                   idle sweeper
Absent ───────────────► Resuming ──► Running ─────────────► Suspending ──► Suspended
   ▲   (cold start /run                 │  (checkpoint→S3,               │
   │    or restore from                 │   free worker)                 │
   │    snapshot)                       │                                │
   └────────────────────────────────────┘◄───────────────────────────────┘
                     next request → resume from snapshot (teleport)
```

- **Absent → Resuming → Running:** first contact. Cold start runs the pool
  template's image; if a snapshot exists, restore it instead.
- **Running → Suspending → Suspended:** idle beyond the template timeout →
  checkpoint to S3, free the worker. State lives only in S3.
- **Suspended → Resuming → Running:** the next request teleports the session onto
  any idle worker (possibly a different node) by restoring the snapshot.

The `Session` CRD's `.status.phase` mirrors this (`Absent`/`Running`/`Suspending`/
`Suspended`/`Resuming`), projected from the authoritative KV entry.

## Consistency and HA

- The operator is the **single KV writer**; every write is CAS‑on‑version. Running
  multiple operator replicas is safe (leader election for reconcile loops; CAS
  guards the assignment table regardless).
- The router is **read‑only** on the table plus advisory activity stamps; it can be
  scaled horizontally freely.
- **Fencing:** a resume verifies `workerHolds(pod, sid)` before trusting a Running
  record; the router's fast path applies the same check. A worker that dies has its
  KV entry removed by the informer (or the 30s prune), after which routing
  re‑resumes onto a healthy worker.

## Scheduling model

- Worker placement is **pure pass‑through**: the operator injects no
  nodeSelector/toleration/affinity/resources — it applies exactly what the
  `SandboxTemplate.spec.scheduling` and `.spec.resources` declare. To pin workers to
  gVisor nodes you must set `nodeSelector: {sandbox: gvisor}` + the matching
  toleration in the template.
- Node spread across hosts needs `minDomains: 2` in a topology‑spread constraint
  (a plain `maxSkew` can't force scale‑up when one node trivially satisfies the
  skew).
- Capacity = pool `replicas`; warm headroom = `minIdle`. Keep `minIdle` ≥ the
  expected concurrent new‑session rate so new users find a warm worker (important
  for slow‑booting images: a saturated pool makes the first user eat a cold start
  and can `503`).
- **Graceful scale‑in** has two layers:
  - *Ordering:* the operator stamps each worker pod with
    `controller.kubernetes.io/pod-deletion-cost` — low on idle workers, high on busy
    ones — so a scale‑down (minIdle contraction, HPA, or lowered `replicas`) deletes
    **idle** workers before **busy** ones. The cost tracks KV busy/idle state in
    near‑real‑time (re‑synced on every claim/release nudge).
  - *Losslessness:* when a **busy** worker's pod does terminate (scale‑in beyond the
    idle count, node drain, rollout, eviction), **checkpoint‑on‑terminate** kicks in.
    The `WorkerDiscoveryReconciler` sees the pod enter Terminating and drives the
    suspend flow (`SuspendForTerminate`): the worker checkpoints the session to S3,
    the operator marks it `Suspended` with the fresh snapshot and removes the worker.
    The worker's SIGTERM handler drain‑waits (keeps serving until its sandbox is
    gone, bounded by `SANDBOXD_DRAIN_DEADLINE`) within the pod's 120s termination
    grace period, so the checkpoint completes before kubelet SIGKILLs it. The session
    then teleport‑resumes on its next request with no lost state. Best‑effort: if the
    checkpoint can't finish within the grace window (huge session, SIGKILL, node
    loss) it degrades to resuming from the last snapshot — never a wedge.

## Trust boundaries and what's next

- The router currently **trusts the `X-Session-ID` header** from the broker; only
  the broker can reach it in‑cluster. The planned **P1.5** phase adds SPIRE mTLS
  (sandboxd `:8090`, operator `/resume`, router→worker) and a NetworkPolicy locking
  workers to router/operator‑only ingress, which also closes this header‑trust gap.
- **Single worker namespace:** the operator assumes one namespace
  (`--resume-namespace`) for templates/pools/sessions and worker‑pod prune, and KV
  keys carry no namespace. Multi‑namespace (per‑tenant) workers are a known future
  item; the manager cache is already cluster‑wide and RBAC is already a ClusterRole,
  so the remaining work is KV key namespacing + the resume/prune glue.

## Component reference

| Component | Image (reference) | Ports | Namespace |
|-----------|-------------------|-------|-----------|
| operator | `…/sandboxd-operator` | `:8081` health, `:8082` `/resume` | `sandboxd-controlplane-system` |
| router | `…/sandboxd-router` | `:8080` (`/mcp`, `/_warm`, `/healthz`) | `sandboxd-controlplane-system` |
| Valkey | `valkey/valkey:8-alpine` | `:6379` | `sandboxd-controlplane-system` |
| sandboxd worker | `…/sandboxd` | `:8090` | `default` (pools) |

See [install-guide-sandboxd.md](install-guide-sandboxd.md) to deploy these and
[admin-guide-crds.md](admin-guide-crds.md) for the CRD fields that drive them.
