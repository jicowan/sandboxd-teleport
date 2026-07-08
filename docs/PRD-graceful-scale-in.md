# PRD — graceful WarmPool scale‑in (drain idle workers first)

Status: **Proposed** (not scheduled). Decision‑ready spec; implement if/when
autoscaling under load is exercised. Grounded in the shipped code on the
`checkpoint-restore` branch. Related: [architecture-sandboxd.md](sandboxd/architecture-sandboxd.md),
[admin-guide-crds.md](sandboxd/admin-guide-crds.md).

## 1. Summary

When a `WarmPool` scales **in** (fewer replicas), the operator should make
Kubernetes preferentially delete **idle** worker pods and avoid killing **busy**
ones. It will do this by setting the standard annotation
**`controller.kubernetes.io/pod-deletion-cost`** on each worker pod — a **low
(negative)** cost on idle workers and a **high (positive)** cost on busy workers —
so the ReplicaSet controller removes idle workers first on scale‑down. Today
nothing sets this, so scale‑in can evict a busy worker mid‑session.

## 2. Problem

Workers are a plain `Deployment`/ReplicaSet managed by the operator
(`warmpool_controller.go`). Scale‑in happens by lowering the Deployment's
`replicas`; the built‑in **ReplicaSet controller then chooses which pods to
delete**. That controller has **no knowledge of session assignment** — it doesn't
read the Valkey assignment table — so it can delete a worker that is currently
**busy** running a live session.

Consequences of killing a busy worker:

- The running gVisor sandbox is destroyed. If a recent checkpoint exists, the
  session can teleport‑resume on its next request (a latency hit + reconnect);
  if not, in‑flight state since the last checkpoint is **lost**.
- It's silent — nothing warns that a scale‑in evicted an active user.

Scale‑in occurs in two ways today, both affected:

1. **minIdle contraction.** `effReplicas = max(spec.replicas, busy + minIdle)`
   (`warmpool_controller.go`). When `busy` drops, `effReplicas` recomputes lower
   and the operator shrinks the Deployment back toward `spec.replicas`. This
   *usually* has idle pods to spare, but the ReplicaSet controller isn't
   *guaranteed* to pick them.
2. **Baseline scale‑down.** An admin edits `spec.replicas`, or an HPA drives the
   `scale` subresource down (WarmPool is HPA‑scalable). Under load this can
   directly target busy workers.

Kubernetes provides the intended mechanism for exactly this —
`controller.kubernetes.io/pod-deletion-cost` (GA) — and we don't use it.

## 3. Goals / non‑goals

### Goals

1. On scale‑in, idle workers are deleted **before** busy workers, best‑effort, via
   `pod-deletion-cost`.
2. The signal tracks reality: a pod's cost is updated **when its busy/idle state
   changes**, using the assignment table (KV) as the source of truth (already the
   operator's model).
3. No change to the data path, the resume workflow, or the CRD API surface.
4. Minimal, least‑privilege RBAC expansion.

### Non‑goals

- **Not** a hard guarantee that a busy worker is never killed. `pod-deletion-cost`
  is a *preference*, honored by the ReplicaSet controller on a best‑effort basis;
  it is not a pod‑disruption veto. (A stronger guarantee — graceful checkpoint‑on‑
  terminate — is a separate future item; see §8.)
- **Not** a new autoscaling policy. This only changes *which* pod is chosen when a
  scale‑in already decided to remove one.
- **Not** a scheduler/eviction (drain, PDB) change — that's node lifecycle, a
  different axis.

## 4. Background — how the operator already tracks worker state

The mechanism this PRD needs already exists:

- **`WorkerDiscoveryReconciler`** (`worker_discovery.go`) is a pod informer over
  worker pods (label `sandboxd.io/app=worker`). It already reconciles on every
  worker pod add/update/delete and writes the `worker:<pod>` KV entry
  (`idle`/`busy`).
- **Busy/idle transitions are observable in the operator:** the resume workflow
  sets a worker `busy` (claim) and the suspend/reset paths set it back to `idle`
  (release), mutating KV and nudging the pool via `PoolNotifier`.

So the operator already knows, per pod, whether it's busy or idle and when that
changes. What's missing is **writing that knowledge onto the pod object** as a
deletion‑cost annotation.

## 5. Proposed design

### 5.1 The annotation

Set on each worker pod:

```
controller.kubernetes.io/pod-deletion-cost: "<int>"
```

- **Idle worker:** a **low** cost, e.g. `"0"` (or a small negative like `"-100"`).
- **Busy worker:** a **high** cost, e.g. `"100"` (positive).

The ReplicaSet controller deletes **lowest‑cost pods first** on scale‑in, so idle
workers go before busy ones. Exact values are an implementation detail (any strict
ordering idle < busy works); pick simple constants and document them.

> Optional refinement (later): rank busy workers among themselves by "cheapest to
> lose" — e.g. a *recently checkpointed* busy worker gets a lower cost than one
> with lots of un‑checkpointed state — so if a busy worker must die, it's the one
> that teleport‑resumes most cheaply. Out of scope for v1; note it.

### 5.2 Where it's set

In `WorkerDiscoveryReconciler` (or a thin sibling), set/patch the annotation at the
same points the KV state is written:

- When a worker registers/refreshes as **idle** → ensure cost = idle value.
- When the resume path marks a worker **busy** → set cost = busy value.
- When suspend/reset returns it to **idle** → set cost back to idle value.

Because these transitions already trigger a reconcile (informer event +
`PoolNotifier` nudge), the pod patch rides along — no new control loop, no polling.
Use a **strategic‑merge/JSON patch of just the annotation** (not a full update) to
avoid clobbering other fields and to minimize API churn. Make it idempotent: only
patch when the current annotation value differs from the desired one.

### 5.3 RBAC (required change)

The operator currently has pods **`get;list;watch`** only
(`config/rbac/role.yaml`, kubebuilder markers in `warmpool_controller.go` and
`worker_discovery.go`). Setting the annotation requires adding **`patch`** (and/or
`update`) on `pods`:

```
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
```

Regenerate `config/rbac/role.yaml` (`make manifests`) and re‑apply it. This is the
one privilege expansion; scope stays namespaced to where workers run.

### 5.4 Reconciliation / drift

- On worker discovery reconcile, if a pod's current cost annotation doesn't match
  its KV state, patch it. This self‑heals pods created before the feature, pods
  that lost the annotation, and any races.
- New worker pods created by the Deployment start **without** the annotation
  (absent == default cost). The informer will stamp them idle on first Ready
  reconcile — acceptable, since a brand‑new pod is idle anyway.

## 6. Interaction with existing behavior

- **minIdle contraction** becomes safe(r): when `busy` drops and the pool shrinks,
  the idle workers (low cost) are the ones removed. The `effReplicas` math is
  unchanged.
- **HPA / manual `spec.replicas` scale‑down** benefits automatically — the
  ReplicaSet controller applies deletion cost regardless of *what* changed
  `replicas`.
- **No effect on scale‑out**, routing, resume, or teleport.

## 7. Testing / acceptance

1. **Unit:** given a worker's KV state, the reconciler computes the correct cost
   and issues a patch only on change (idempotent).
2. **Integration (envtest/kind):** a pool with N workers, M busy (KV), scaled from
   N to N‑k → the k deleted pods are idle; no busy pod is deleted while an idle one
   exists.
3. **Live (reference cluster):** drive several sessions onto `aio-pool`, then lower
   `replicas`; confirm via events that idle workers are the ones terminated and
   active sessions survive.
4. **Regression:** RBAC re‑applied; operator can patch pods (no `forbidden`).

Acceptance: with idle workers present, a scale‑in never deletes a busy worker.

## 8. Limitations & future work

- **Best‑effort, not a guarantee.** If a scale‑in removes more pods than there are
  idle workers, some busy workers *will* be deleted (there's no idle pod to prefer).
  `pod-deletion-cost` can't prevent that — it only orders the choice.
- **The real safety net is checkpoint‑on‑terminate** — specced in
  [PRD-checkpoint-on-terminate.md](PRD-checkpoint-on-terminate.md): a preStop hook /
  operator‑driven suspend that checkpoints the running session to S3 before the pod
  dies, so even an unavoidable busy‑worker deletion loses nothing and
  teleport‑resumes cleanly. `pod-deletion-cost` + checkpoint‑on‑terminate together
  give graceful scale‑in; this PRD is the first (cheap, high‑value) half.
- **PodDisruptionBudget** could additionally protect against voluntary disruptions
  (node drain), a separate axis from replica scale‑in.

## 9. Effort estimate

Small. One reconciler touch‑point (patch annotation on state change + drift
reconcile), one RBAC verb added and manifests regenerated, and tests. No API
change, no data‑path change, no new controller. Could ship in a single PR.

## 10. Open questions

| # | Question | Leaning |
|---|----------|---------|
| Q1 | Exact cost values (idle `0` vs negative; busy `100`)? | Simple constants with strict idle<busy ordering; document them. Negative for idle is fine. |
| Q2 | Rank busy workers by checkpoint recency (§5.1 refinement)? | Defer to a later iteration; v1 uses a single busy value. |
| Q3 | Patch from `WorkerDiscoveryReconciler` or a dedicated small controller? | Reuse WorkerDiscovery — it already watches the exact pods and observes the transitions. |
| Q4 | Ship checkpoint‑on‑terminate (§8) together or separately? | Separately — this PRD is the cheap half; checkpoint‑on‑terminate is its own PRD. |
