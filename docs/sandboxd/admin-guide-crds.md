# Admin guide — sandboxd CRD reference

Reference for every custom resource the sandboxd operator serves. All are in API
group **`core.sandboxd.io`**, version **`v1alpha1`**.

| Kind | Short name | Purpose |
|------|-----------|---------|
| `SandboxTemplate` | `sbxt` | Blueprint for what to run as a sandbox (image, ports, health, idle policy, worker overrides). |
| `WarmPool` | `wp` | Desired set of warm workers built from one template; owns worker scaling. |
| `Session` | `sess` | A durable user session (usually created lazily by the operator); its status mirrors the KV assignment entry. |
| `ForkSet` | `fork` | Fan out N independent child Sessions from one common source — a snapshot (`baseRef`) or a pool's image. Owns its children. |
| `BaseSnapshot` | `basesnap` | A promoted, pinnable "golden" checkpoint (copy‑on‑promote to `bases/`) that a `ForkSet` restores its forks from. |

Field tables list the JSON field name, type, whether it's required, the
default/validation, and meaning. "Required" means the field has no default and the
schema rejects the object without it.

---

## SandboxTemplate (`sbxt`)

The operator‑authored blueprint. Its fields map onto the worker's `/run` and
`/restore` request bodies (see [api-reference-sandboxd-worker.md](api-reference-sandboxd-worker.md)).
Has a `status` subresource; no printer columns.

### `.spec`

| Field | Type | Required | Default / validation | Meaning |
|-------|------|----------|----------------------|---------|
| `image` | string | **Yes** | — | OCI image run **as the sandbox** (the nested gVisor workload) — not the worker pod image. |
| `cmd` | []string | No | — | Overrides the image entrypoint+cmd. |
| `env` | []string | No | — | Added to the sandbox process environment. |
| `ports` | []PortMap | No | — | Exposed via the worker's DNAT (`podIP:host → interiorIP:container`). |
| `health` | Health | No | — | Drives the worker readiness probe (and thus router health/idle detection). |
| `idle` | IdlePolicy | No | (see IdlePolicy) | How long a sandbox may idle before checkpoint/reclaim. |
| `checkpointIntervalSeconds` | int | No | `0` (disabled); min 0 | Periodic background checkpoints while Running (checkpoint to S3, leave running) every N seconds, bounding crash loss to ~N seconds. Opt‑in (adds S3 churn + brief pauses). |
| `workerImage` | string | No | empty ⇒ operator global default | Overrides the sandboxd **worker** image for this pool (NOT the workload image). The worker image carries the pinned `runsc` that checkpoint/restore depends on, so it's normally one global value (operator `--worker-image`); override only to canary a new worker build on one pool. Sessions can't teleport across workers with incompatible `runsc`. |
| `streamConsole` | bool | No | `false` | Surfaces the nested workload's stdout/stderr to the worker's stdout (→ `kubectl logs`) by setting `SANDBOXD_STREAM_CONSOLE=1` on this pool's workers. The console is attacker‑controlled and multi‑tenant over a worker's lifetime, so it's opt‑in per pool. The session‑scoped `/logs` API stays the production path. |
| `iam` | IAMSpec | No | — | Lets sandboxes in this pool assume an AWS IAM role (`iam.roleArn`); the worker vends per‑session temporary credentials. Off unless set. A `Session` may override per session. Requires the operator's `--cred-token-secret`. |
| `resources` | corev1.ResourceRequirements | No | — | Worker sizing hint → the worker pod's resource requests/limits. |
| `scheduling` | SchedulingSpec | No | — | Worker‑pod placement (nodeSelector/tolerations/affinity/spread). Applied verbatim; the operator injects no defaults. |

### `.status`

| Field | Type | Meaning |
|-------|------|---------|
| `conditions` | []metav1.Condition | Standard condition list (`listType=map` keyed by `type`). |

### Example

```yaml
apiVersion: core.sandboxd.io/v1alpha1
kind: SandboxTemplate
metadata: { name: aio, namespace: default }
spec:
  image: ghcr.io/agent-infra/sandbox:latest
  ports: [{ container: 8080, host: 8080 }]
  health: { probe: http, probePort: 8080, probePath: /v1/health }
  idle: { timeoutSeconds: 600, action: suspend }
  streamConsole: true                     # surface workload console on this pool
  workerImage: <registry>/sandboxd:v42    # optional per-pool worker canary
  resources:
    requests: { cpu: "500m", memory: "1Gi" }
  scheduling:
    nodeSelector: { sandbox: gvisor }
    tolerations: [{ key: sandbox, operator: Equal, value: gvisor, effect: NoSchedule }]
    topologySpreadConstraints:
      - { maxSkew: 1, minDomains: 2, topologyKey: kubernetes.io/hostname,
          whenUnsatisfiable: DoNotSchedule,
          labelSelector: { matchLabels: { sandboxd.io/app: worker, sandboxd.io/pool: aio-pool } } }
```

> To pin workers to gVisor nodes you **must** set `scheduling.nodeSelector` +
> `tolerations` — nothing is defaulted. Node spread across hosts needs
> `minDomains: 2` (a plain `maxSkew` won't force scale‑up).

---

## WarmPool (`wp`)

Declarative desired set of warm workers, all built from one `SandboxTemplate`.
Owns worker scaling. Has a `status` subresource **and a `scale` subresource**
(`.spec.replicas` / `.status.replicas` / `.status.selector`) so it's HPA‑scalable.

Printer columns: `Replicas` (`.status.replicas`), `Idle` (`.status.idle`),
`Busy` (`.status.busy`) — visible in `kubectl get warmpool`.

### `.spec`

| Field | Type | Required | Default / validation | Meaning |
|-------|------|----------|----------------------|---------|
| `templateRef` | LocalRef | **Yes** | — | Names the `SandboxTemplate` this pool's workers serve. |
| `replicas` | int32 | Yes (no `+optional`) | default `1`; min 0 | Desired number of warm workers. HPA‑scalable via the scale subresource. |
| `minIdle` | int32 | No | default `1`; min 0 | Keep at least N idle workers ready. The operator raises the effective replica count to `max(replicas, busy + minIdle)`, scaling up only — so it doesn't fight an HPA. |
| `arbitraryImage` | bool | No | — | Marks this pool as the landing zone for arbitrary‑image sessions (looser warm guarantees, fed by a generic base image). |

### `.status`

| Field | Type | Meaning |
|-------|------|---------|
| `replicas` | int32 | Worker pods currently owned by this pool (scale subresource statuspath). |
| `idle` | int32 | Workers currently holding no session. |
| `busy` | int32 | Workers currently holding a session. |
| `selector` | string | Label selector for the scale subresource / HPA. |
| `conditions` | []metav1.Condition | Standard condition list. |

### Example

```yaml
apiVersion: core.sandboxd.io/v1alpha1
kind: WarmPool
metadata: { name: aio-pool, namespace: default }
spec:
  templateRef: { name: aio }
  replicas: 4
  minIdle: 2
```

```sh
kubectl get wp -n default
# NAME       REPLICAS   IDLE   BUSY
# aio-pool   4          2      2
```

> **Sizing `minIdle`:** keep it ≥ the expected number of concurrent *new* sessions.
> A cold‑start with no idle worker returns `503` and can poison an MCP client's
> cached tool list. This matters most for slow‑booting images (a browser‑class
> image cold‑starts ~40–45s).

> **Scale‑in prefers idle workers.** When a pool scales down (minIdle contraction,
> a lowered `replicas`, or an HPA), the operator sets
> `controller.kubernetes.io/pod-deletion-cost` on worker pods (idle = low, busy =
> high) so the ReplicaSet controller deletes idle workers before busy ones. It's
> best‑effort ordering, not a guarantee — if the scale‑in removes more pods than
> there are idle workers, a busy worker (live session) can still be deleted.

---

## Session (`sess`)

A durable user session. In normal operation you don't create these by hand — the
operator **creates a Session lazily** on the first `/resume` for an unknown id,
using the pool hint from the broker. You may create one explicitly for the
arbitrary‑image mode or to override lifecycle. The object's `metadata.name` **is**
the session id (e.g. `sess-<principal>-<hash>`). Has a `status` subresource.

Printer columns: `Phase` (`.status.phase`), `Worker` (`.status.workerPodIP`).

### `.spec`

| Field | Type | Required | Meaning |
|-------|------|----------|---------|
| `poolRef` | LocalRef | No | Selects a `WarmPool` (template mode). Mutually exclusive with `image`. |
| `image` | string | No | Caller‑supplied arbitrary image (arbitrary‑image mode; authz‑gated at the front door before creation). Mutually exclusive with `poolRef`. |
| `cmd` | []string | No | Entrypoint override (arbitrary‑image mode). |
| `env` | []string | No | Env (arbitrary‑image mode). |
| `ports` | []PortMap | No | Ports (arbitrary‑image mode). |
| `subject` | string | No | Opaque identity the router matches the JWT‑derived subject against. |
| `iam` | IAMSpec | No | Assume an AWS IAM role for this session's sandbox (`iam.roleArn`), overriding the pool template's `iam`. |
| `lifecycle` | SessionLifecycle | No | Overrides template idle/TTL for this session. |
| `forkFrom` | ForkProvenance | No | Set by the `ForkSet` controller on a fork child: `{baseRef, snapshotURI}` records the base it was seeded from (snapshot source; empty for image forks). Makes the child self‑describing and is the base‑reclaim ref‑count key. Not set by hand. |
| `suspendRequest` | string | No | **Edge‑triggered, one‑shot** request to checkpoint+suspend this session **now** (docs/PRD-on-demand-suspend.md). Set it to a fresh **opaque token** (uuid/timestamp/etc.); when it differs from `status.lastSuspendHandled` the operator performs exactly one checkpoint→S3→Suspended→free‑worker, then sets the watermark equal. It is **not** a level‑triggered desired‑state — reactive resume (a request to the router) may bring the session back to Running afterward and will **not** re‑suspend it (the token is already handled). Wait for `status.lastSuspendHandled == spec.suspendRequest && status.snapshotURI != ""` to know the snapshot is durable (e.g. before promoting a `BaseSnapshot`). |

#### SessionLifecycle

| Field | Type | Required | Meaning |
|-------|------|----------|---------|
| `idleTimeoutSeconds` | int | No | Overrides the template idle timeout. |
| `idleAction` | string | No | Overrides the template idle *action* for this session: `suspend` (checkpoint→S3, free worker), `reset` (discard state, free worker), or `none`. Lets an ephemeral fork choose `reset`‑on‑idle without a dedicated pool. Empty = inherit the template's action. |
| `ttlAfterSuspendSeconds` | int | No | How long the S3 checkpoint is retained after suspend before GC reaps the whole session footprint (S3 snapshot + KV entry + this `Session` CR). Unset falls back to the operator's `--default-ttl-after-suspend-seconds`; if that is also `0`, the checkpoint is kept forever. |

### `.status` (durable mirror of the KV assignment entry)

The operator mirrors this to etcd so the Valkey cache can be rebuilt after a
restart. **It mirrors only durability‑critical transitions** — `Suspended`
(idle‑suspend + checkpoint‑on‑terminate) and periodic‑checkpoint advances — not
`Resuming`/`Running`/`Suspending`. So `kubectl get sess` shows the last *durable*
state (e.g. `Suspended`), not necessarily live `Running`: a running session that's
never been suspended may show an empty phase. This is intentional — mirroring
Running buys no recovery (a wiped Running session falls back to its last snapshot
anyway) and would only add etcd write pressure. The fields below are a lossless
mirror so a rebuild needs no template re‑resolution.

| Field | Type | Validation | Meaning |
|-------|------|-----------|---------|
| `phase` | string | Enum: `Absent`, `Running`, `Suspending`, `Suspended`, `Resuming` | Durable session state (last mirrored). |
| `pool` | string | — | Pool the worker is claimed from (for rebuild). |
| `workerPod` | string | — | Bound worker pod name (fencing key); empty when suspended. |
| `workerPodIP` | string | — | Set while Running/Resuming. |
| `snapshotURI` | string | — | Current checkpoint location (once one exists). |
| `image` | string | — | Resolved image (recorded for restore identity). |
| `ports` | []PortMap | — | Exposed ports (replayed on restore). |
| `health` | Health | — | Readiness/restart config (replayed on restore). |
| `iamRoleArn` | string | — | Session's assumable AWS role (replayed on restore). |
| `lastActiveAt` | metav1.Time | — | Last request time (mirrored coarsely, on transitions — not every request). |
| `lastSuspendHandled` | string | — | Watermark for `spec.suspendRequest`: the token the operator most recently completed, set **only after** the checkpoint is durably in S3 and the session is Suspended. Equal to `spec.suspendRequest` ⇒ the on‑demand suspend is done; differing ⇒ pending/in‑flight. |
| `conditions` | []metav1.Condition | — | Standard condition list (includes a `SuspendRequest` condition surfacing on‑demand‑suspend progress: `Suspended`/`SuspendFailed`). |

```sh
kubectl get sess -n default
# NAME                                   PHASE       WORKER
# sess-jicowan-93b7baf854168a42          Running     10.0.5.178
```

---

## ForkSet (`fork`)

Fans out **N independent child `Session`s from one common source** in a single
declarative object (docs/PRD-snapshot-fork.md). A `ForkSet` is to forked Sessions what
a `WarmPool` is to worker pods: the controller creates and owns N `Session` children
(ownerRefs) and rolls their readiness up into `.status`. The **source** is selected by
`baseRef`:

- **`baseRef` set → snapshot source.** Children restore (`/restore`) from a `BaseSnapshot`
  — identical RAM+FS state (the "branch from a common reached state" / RL rollout case).
- **`baseRef` omitted → image source.** Children cold‑start (`/run`) from `pool`'s
  template image — independent per‑boot init. No `BaseSnapshot` involved.

Has a `status` subresource. Printer columns: `Desired`, `Ready`, `Phase`.

### `.spec`

| Field | Type | Required | Meaning |
|-------|------|----------|---------|
| `baseRef` | LocalRef | No | The `BaseSnapshot` to fork from (snapshot source). Omit for image‑source fan‑out. |
| `count` | int32 | **Yes** | Number of fork children (N) to create; min 1. |
| `namePrefix` | string | No | Deterministic child naming (`sess-fork-<prefix>-<n>`) so a harness can address a specific fork. Defaults to the ForkSet name. |
| `pool` | string | **Yes** | Pool that places the forks; for the image source it also supplies the template image. Must be `runsc`‑compatible with the base (snapshot source). |
| `activation` | string | No | `Eager` (materialize all N now) or `Lazy` (born Suspended/Absent, materialize on first contact). Default `Lazy`. |
| `lifecycle` | SessionLifecycle | No | Per‑fork idle policy applied to every child — notably `idleAction: reset` for ephemeral rollouts (leaves no snapshot) vs `suspend` for durable branches. |
| `subject` | string | No | Owner identity for attribution / fan‑out quota. |
| `iam` | IAMSpec | No | Per‑fork AWS role, overriding the base's / template's `iam`. |

### `.status`

| Field | Type | Meaning |
|-------|------|---------|
| `desired` | int32 | Fork children requested (mirrors `spec.count`). |
| `ready` | int32 | Children whose Session has reached Running (read from the KV assignment table). |
| `forks` | []string | Created child session ids — what a harness reads back to address each fork. |
| `phase` | string | `Progressing` \| `Ready` \| `Failed`. |
| `conditions` | []metav1.Condition | Standard condition list. |

### Example

```yaml
apiVersion: core.sandboxd.io/v1alpha1
kind: ForkSet
metadata: { name: rl-batch, namespace: default }
spec:
  baseRef: { name: golden-cartpole-v3 }   # omit for image-source fan-out
  count: 16
  namePrefix: rollout
  pool: aio-pool
  activation: Eager
  lifecycle: { idleAction: reset, idleTimeoutSeconds: 600 }
```

```sh
kubectl apply -f forkset.yaml
kubectl get fork rl-batch -n default          # watch .status.ready reach .status.desired
kubectl get fork rl-batch -n default -o jsonpath='{.status.forks}'   # the N session ids to drive
```

> **Addressing a fork:** send its session id as `X-Session-ID` to the router (each
> child is a normal session on its own worker). The router is unchanged — it resolves
> whatever id it's handed. Deleting the `ForkSet` cascade‑deletes its child Sessions.

> **Where forked state lives is a workload concern.** A fork restores the workload's
> whole checkpointed state, but caller‑written data only survives if the workload
> persisted it somewhere the checkpoint captures. E.g. the **AIO sandbox** runs each
> `/v1/shell/exec` in an isolated shell — `/tmp` writes don't persist across exec calls
> or checkpoint/restore, but writes under the working dir **`/home/gem`** do. Put
> fork‑relevant state where your target image persists it.

---

## BaseSnapshot (`basesnap`)

A promoted, forkable "golden" checkpoint, decoupled from any single session's mutable
snapshot lineage. The controller resolves a **Suspended** source session's current
snapshot and does an S3 server‑side **copy‑on‑promote** into a fork‑stable
`bases/<name>/` prefix — distinct from the per‑session `sandboxes/<sid>/` space the GC
orphan‑S3 sweep touches, so a base is structurally exempt from orphan reaping. A
finalizer reclaims the `bases/<name>/` objects when the CR is deleted. Has a `status`
subresource. Printer columns: `Ready`, `Refs`, `Pinned`, `Phase`.

### `.spec`

| Field | Type | Required | Meaning |
|-------|------|----------|---------|
| `sourceSessionRef` | LocalRef | **Yes** | The Suspended `Session` whose current checkpoint is promoted to this base. |
| `pinned` | bool | No | Keep indefinitely: while true the base is never auto‑reclaimed even when `refCount` reaches 0. |

### `.status`

| Field | Type | Meaning |
|-------|------|---------|
| `snapshotURI` | string | The fork‑stable `bases/<name>/…` prefix the base was copied to. |
| `image` / `runscVersion` / `ports` / `health` / `iamRoleArn` | — | Restore identity captured from the source at promote time (forks are self‑describing). |
| `refCount` | int32 | Holds keeping the base alive: forks not yet past their first restore, plus explicit pins. Drops as forks materialize. |
| `ready` | bool | True once copy‑on‑promote completes (base is forkable). |
| `phase` | string | `Pending` \| `Ready` \| `Reclaimed` \| `Failed`. |

### Example

```yaml
apiVersion: core.sandboxd.io/v1alpha1
kind: BaseSnapshot
metadata: { name: golden-cartpole-v3, namespace: default }
spec:
  sourceSessionRef: { name: sess-golden }   # must be Suspended with a snapshot
  pinned: true
```

> **Reclaim:** an unpinned base becomes eligible only when `refCount == 0` and a grace
> has elapsed (the base reaper then deletes the CR; the finalizer clears its S3).
> Deleting the CR directly (incl. a pinned one) also reclaims its S3 via the finalizer.
> A fork depends on the base **only for its first restore** — afterward it has its own
> lineage — so a base whose forks are all Running (and unpinned) is immediately
> reclaimable.

---

## Shared / common types

These are embedded in the CRDs above and mirror the worker's on‑wire JSON so the
operator can hand them straight to `/run` and `/restore`.

### PortMap — worker pod port → sandbox container port (DNAT)

| Field | Type | Required | Validation | Meaning |
|-------|------|----------|-----------|---------|
| `container` | int | Yes (no `+optional`) | 1–65535 | Port the sandbox process listens on. |
| `host` | int | No | 0–65535 | Port on the worker pod IP. `0` defaults to `container`. |

### Health — how the worker probes/restarts the sandbox

| Field | Type | Required | Enum | Meaning |
|-------|------|----------|------|---------|
| `restartPolicy` | string | No | `none`, `cold`, `restore` | Restart behavior on unexpected sandbox exit. |
| `probe` | string | No | `none`, `tcp`, `http` | Readiness probe type. |
| `probePort` | int | No | — | Interior container port to probe. |
| `probePath` | string | No | — | HTTP path to probe (http probe only). |

### IdlePolicy — suspend‑on‑idle behavior

| Field | Type | Required | Default / validation | Meaning |
|-------|------|----------|----------------------|---------|
| `timeoutSeconds` | int | No | default `300`; min 0 | Seconds of inactivity before the action fires. `0` = never auto‑suspend. |
| `action` | string | No | default `suspend`; enum `suspend`, `reset`, `none` | On idle: `suspend` (checkpoint→S3, free worker), `reset` (discard state, free worker), or `none`. |

### LocalRef — reference to another object by name (same namespace)

| Field | Type | Required | Meaning |
|-------|------|----------|---------|
| `name` | string | **Yes** | Name of the referenced object. |

### IAMSpec — AWS IAM role the sandbox may assume

| Field | Type | Required | Meaning |
|-------|------|----------|---------|
| `roleArn` | string | No | ARN of the IAM role the sandbox assumes (per‑session temporary credentials vended by the worker). Authorization for which sessions may use which role is a front‑door / control‑plane decision. Requires the operator's `--cred-token-secret` and the worker's role to be permitted to `sts:AssumeRole` it. |

### SchedulingSpec — worker‑pod placement (pass‑through)

| Field | Type | Required | Meaning |
|-------|------|----------|---------|
| `nodeSelector` | map[string]string | No | Constrains which nodes workers land on. Applied verbatim. |
| `tolerations` | []corev1.Toleration | No | Tolerations for worker pods. Applied verbatim. |
| `affinity` | *corev1.Affinity | No | Node/pod/anti‑affinity. Applied verbatim. |
| `topologySpreadConstraints` | []corev1.TopologySpreadConstraint | No | Spread constraints. Applied verbatim. |

---

## Operational notes

- **Editing a `SandboxTemplate` does not auto‑reconcile its `WarmPool`.** The
  operator watches `WarmPool` + owned Deployments, not templates. After changing a
  template (e.g. `workerImage`, `streamConsole`, scheduling), nudge the pool to roll
  workers:

  ```sh
  kubectl annotate warmpool <pool> -n default sandboxd.io/nudge="$(date +%s)" --overwrite
  ```

- **Per‑pool worker image vs. teleport.** Overriding `workerImage` on one pool is
  for canarying. Sessions cannot teleport between workers running incompatible
  `runsc` versions, so keep the worker image aligned across any pools you expect to
  teleport between.

- **Watching state:** `kubectl get wp` for pool idle/busy; `kubectl get sess` for
  per‑session phase/worker. The authoritative state is the Valkey assignment table;
  the CRD status is a projection and can briefly lag.
