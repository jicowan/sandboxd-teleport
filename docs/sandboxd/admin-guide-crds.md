# Admin guide — sandboxd CRD reference

Reference for every custom resource the sandboxd operator serves. All are in API
group **`core.sandboxd.io`**, version **`v1alpha1`**.

| Kind | Short name | Purpose |
|------|-----------|---------|
| `SandboxTemplate` | `sbxt` | Blueprint for what to run as a sandbox (image, ports, health, idle policy, worker overrides). |
| `WarmPool` | `wp` | Desired set of warm workers built from one template; owns worker scaling. |
| `Session` | `sess` | A durable user session (usually created lazily by the operator); its status mirrors the KV assignment entry. |

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
| `lifecycle` | SessionLifecycle | No | Overrides template idle/TTL for this session. |

#### SessionLifecycle

| Field | Type | Required | Meaning |
|-------|------|----------|---------|
| `idleTimeoutSeconds` | int | No | Overrides the template idle timeout. |
| `ttlAfterSuspendSeconds` | int | No | How long the S3 checkpoint is retained after suspend before GC. |

### `.status` (mirrors the authoritative KV assignment entry)

| Field | Type | Validation | Meaning |
|-------|------|-----------|---------|
| `phase` | string | Enum: `Absent`, `Running`, `Suspending`, `Suspended`, `Resuming` | Current session state (projected from KV). |
| `workerPodIP` | string | — | Set while Running/Resuming. |
| `snapshotURI` | string | — | Current checkpoint location (once one exists). |
| `image` | string | — | Resolved image (recorded for restore identity). |
| `lastActiveAt` | metav1.Time | — | Last request time stamped by the router. |
| `conditions` | []metav1.Condition | — | Standard condition list. |

```sh
kubectl get sess -n default
# NAME                                   PHASE       WORKER
# sess-jicowan-93b7baf854168a42          Running     10.0.5.178
```

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
