# PRD — checkpoint‑on‑terminate (don't lose a session when a worker dies)

Status: **Implemented** (2026‑07‑08, operator `v15` + worker `v43`). Companion to
[PRD-graceful-scale-in.md](PRD-graceful-scale-in.md): scale‑in ordering picks
*which* worker dies; this makes a worker's death *lossless* when it holds a
session. Related: [architecture-sandboxd.md](sandboxd/architecture-sandboxd.md).

> **As built** (Design A, with two refinements to §5.3/§5):
> - **No `preStop` hook.** The worker image is distroless (no shell), and an
>   `httpGet` preStop only fires once (can't poll‑until‑drained). Instead the
>   **worker's own SIGTERM handler drain‑waits** (`sandboxd/main.go`): on SIGTERM it
>   keeps the HTTP server serving while it still holds a sandbox, so the operator's
>   `/suspend` can land, and exits once the sandbox is gone or `SANDBOXD_DRAIN_DEADLINE`
>   (default 100s, < the 120s grace period) elapses. Same effect as a preStop, no
>   shell needed.
> - **Terminate path `RemoveWorker`, not `ReleaseWorker`.** A new
>   `Suspender.SuspendForTerminate(sid, pod, ip, pool)` mirrors idle‑suspend
>   (Suspending → `/suspend` → Suspended+snapshotURI) but **removes** the worker
>   entry instead of returning it to the idle pool — a dying worker must never be
>   handed a new session. It's CAS‑idempotent (no‑ops if idle‑suspend already moved
>   the session) and bounded by `SuspendDeadline`.
> - **Trigger:** `WorkerDiscoveryReconciler` already sees a pod enter Terminating;
>   if KV says the worker is busy it calls `SuspendForTerminate`. `WorkerImage`‑style
>   `WorkerTerminationGracePeriodSeconds` var (default 120) sets the pod grace
>   period on the operator‑generated Deployment (and `worker-deploy.yaml`). RBAC
>   unchanged (operator already had what it needed). Verified live: deleting a busy
>   aio‑pool worker → session Suspended with a fresh snapshot in ~5s → reconnect
>   restored the pre‑delete marker file on a different worker.

## 1. Summary

When a worker pod is terminating (scale‑in, node drain, rollout, eviction) **while
it holds a running session**, the worker should **checkpoint that session to S3
before it exits**, so the session teleport‑resumes cleanly on its next request with
no lost state. Today termination just drains HTTP and exits — the running gVisor
sandbox is destroyed and everything since the last checkpoint is lost.

This is the "real safety net" deferred from the scale‑in PRD. Together,
`pod-deletion-cost` (prefer deleting idle workers) + checkpoint‑on‑terminate
(make deleting a busy worker lossless) give **graceful scale‑in**.

## 2. Problem

The worker's shutdown handler (`sandboxd/main.go`) on SIGTERM/SIGINT only drains
the HTTP server and returns — it deliberately does **not** checkpoint (the code
comments that checkpoint‑on‑shutdown is "the control plane's decision"). So when
kubelet sends SIGTERM to a worker pod that is busy:

- The running sandbox dies with the pod. State since the **last** checkpoint
  (periodic checkpoints are opt‑in and off by default; idle‑suspend hasn't fired
  because the session is active) is **lost**.
- The session's next request teleport‑resumes from the *stale* snapshot (if any)
  or cold‑starts — silently losing the user's in‑flight work.

Termination is routine, not exceptional: WarmPool scale‑in, Karpenter node
consolidation, worker image rollouts, and evictions all SIGTERM busy workers.

Compounding constraints observed in the code:

- **No `terminationGracePeriodSeconds` or `preStop`** is set on worker pods (neither
  `sandboxd/worker-deploy.yaml` nor the operator‑generated Deployment), so the grace
  period is the default **30s** — possibly too short for a large checkpoint+upload
  (browser‑class sessions measured ~7–8s for checkpoint+S3 today, but bigger/slower
  cases could approach or exceed 30s once kubelet overhead is included).
- **The worker holds no Valkey credentials.** The operator is the *sole* KV writer
  (assignment table). So the worker can checkpoint to S3 but **cannot** record the
  new `snapshotURI` or mark the session `Suspended` itself — that coordination is
  the hard part of this PRD.

## 3. Goals / non‑goals

### Goals

1. A terminating worker that holds a running session **checkpoints it to S3** before
   the container exits.
2. The assignment table ends in a correct state: the session becomes `Suspended`
   with the **new** `snapshotURI`, and the worker is released — so the next request
   teleport‑resumes from the fresh checkpoint, losing nothing.
3. Bounded and safe: never hang termination indefinitely; if the checkpoint can't
   finish in the grace window, fail cleanly (fall back to today's behavior) rather
   than corrupt state.
4. Works for all termination causes (scale‑in, drain, rollout, eviction), not just
   WarmPool scale‑in.

### Non‑goals

- **Not** preserving live TCP/MCP connections. Sockets never survive teleport; the
  client reconnects (`initialize`). This is about **state**, not connections.
- **Not** guaranteeing success under a hard `SIGKILL` / lost node / grace‑period
  overrun. Best‑effort within the grace window; §7 covers the failure mode.
- **Not** changing the resume/restore path — it already restores from a
  `snapshotURI`; we just need a *fresh* one recorded at terminate time.
- **Not** the deletion‑ordering concern — that's [PRD-graceful-scale-in.md](PRD-graceful-scale-in.md).

## 4. Background — how suspend works today (the flow to mimic)

The operator's `Suspender` (`internal/resume/suspend.go`) already does exactly the
state transition we need, for the *idle* case:

1. CAS the session to `Suspending`.
2. Call the worker's `POST /suspend` → the worker checkpoints to S3, deletes the
   sandbox, frees itself, and returns `{ snapshot, … }`.
3. CAS the session to `Suspended` with `snapshotURI = resp.Snapshot`.
4. `ReleaseWorker(pod, pool)` in KV.

The worker already implements `/suspend` (checkpoint → S3 → delete → cleanup). The
gap is that on **pod termination** nothing invokes this flow, and the worker can't
complete steps 1/3/4 (KV writes) on its own.

## 5. Proposed design

The termination signal originates at the pod (kubelet SIGTERM), but the KV writes
must come from the operator. Two candidate designs; recommend **A**.

### Design A (recommended): operator‑driven, informer‑triggered

Leverage the fact that the operator **already watches worker pods**
(`WorkerDiscoveryReconciler`) and sees a pod enter `Terminating`
(`DeletionTimestamp != nil`) — it currently just removes the worker from the idle
set on that event.

Change: when a pod that is **busy** (per KV) enters Terminating, the operator runs
the **existing suspend flow against that worker** before the pod is gone:

- WorkerDiscovery reconcile sees `DeletionTimestamp != nil` + KV state `busy` →
  enqueue an urgent suspend of that session (reuse `Suspender.suspendOne` /
  `POST /suspend` on the worker's IP).
- The worker checkpoints to S3 and returns the snapshot; the operator CAS‑marks the
  session `Suspended` with the new URI and releases the worker — identical to the
  idle‑suspend path, just triggered by termination instead of idleness.

To give the checkpoint time to finish before the container is killed, add a worker
**`preStop` hook + a longer `terminationGracePeriodSeconds`** (see §5.3). The
preStop hook holds the container open (sleep/poll) while the operator drives the
suspend; when the session is `Suspended`/released, the pod can exit.

**Why A:** reuses the proven suspend flow and the sole‑KV‑writer invariant intact
(operator still owns all KV writes). No new worker→KV trust. The operator already
has the informer event.

**Risk to handle:** the race between the operator noticing Terminating and the pod
actually dying. The `preStop` hook + grace period is what buys the window; if the
window closes first, we fall back to today's behavior (lossy) — see §7.

### Design B (alternative): worker self‑checkpoints, operator reconciles KV after

The worker's own SIGTERM/`preStop` handler checkpoints to S3 (it can do S3 — it has
Pod Identity) and writes the resulting snapshot URI somewhere the operator can read
(e.g. a well‑known S3 marker, or the worker's `/status`), then exits. The operator,
on the pod‑gone event, reads that URI and CAS‑marks the session `Suspended`.

**Downside:** splits the checkpoint action (worker) from the KV truth (operator)
across a dying process, with a fragile handoff (the worker may die before the
operator reads the URI). More moving parts than A. Keep as fallback only.

### 5.3 Grace period + preStop (both designs)

- Set `terminationGracePeriodSeconds` on the worker pod (operator‑generated
  Deployment + `worker-deploy.yaml`) to a value comfortably above the P99
  checkpoint+upload time — proposal **120s** (tunable; the existing resume/warm
  deadline is already 90s for large images, so 120s is consistent).
- Add a `preStop` hook that blocks pod shutdown until the session is checkpointed
  (Design A: poll until KV says `Suspended`/released or a deadline; Design B: run
  the checkpoint synchronously). preStop runs *before* SIGTERM, extending the
  effective window within the grace period.
- Make the grace period a **per‑pool/template knob** (or a global operator default)
  so slow images get more time.

### 5.4 Idempotency & fencing

- If the operator's idle‑suspend sweeper and the terminate‑suspend fire for the same
  session, the existing CAS‑on‑version protocol serializes them — the second sees
  `Suspending`/`Suspended` and no‑ops. Reuse it; no new locking.
- If a periodic checkpoint (opt‑in) just ran, the terminate checkpoint is still
  worth taking (it captures the newest state) but may be skippable if within a
  freshness window — optional optimization, not required.

## 6. Interaction with other work

- **Graceful scale‑in ([PRD-graceful-scale-in.md](PRD-graceful-scale-in.md)):** that
  PRD makes scale‑in *prefer* idle workers; this one makes the *unavoidable* busy
  deletions lossless. They compose and are independently shippable — but this PRD is
  what turns "best‑effort ordering" into "safe scale‑in."
- **Periodic background checkpoints (existing, opt‑in):** reduce worst‑case loss
  even without this feature, but at steady S3 cost. Checkpoint‑on‑terminate gives
  the same protection *only when needed*, so it can stay off by default.
- **Node drain / Karpenter consolidation:** benefits automatically — any SIGTERM to
  a busy worker triggers the checkpoint.

## 7. Failure mode (be honest)

If the checkpoint can't complete within the grace period (huge session, slow S3,
`SIGKILL`, node loss), the pod dies anyway and the session falls back to **today's
behavior**: resume from the last snapshot or cold‑start. The feature is a
best‑effort improvement, not a durability guarantee. It must **fail safe** — never
block a node drain indefinitely, never leave the session wedged in `Suspending`
(a reconcile/timeout must recover it to a resumable state).

## 8. Testing / acceptance

1. **Integration:** a busy worker gets SIGTERM'd (delete the pod); assert the
   session ends `Suspended` with a **newer** `snapshotURI` than before, worker
   released, and a subsequent request restores that snapshot with state intact.
2. **Scale‑in:** scale a pool below `busy`; assert evicted busy workers checkpointed
   first (session state survives), pairing with the deletion‑cost PRD.
3. **Grace overrun:** artificially slow the checkpoint past the grace period; assert
   the pod still terminates and the session recovers to a resumable state (no wedge).
4. **Idempotency:** concurrent idle‑suspend + terminate‑suspend → one wins via CAS,
   no double‑release.

Acceptance: deleting a busy worker pod loses no committed session state within the
grace window; outside it, the system degrades to today's behavior without wedging.

## 9. Effort estimate

Medium. Reuses the suspend flow and CAS protocol (no new checkpoint code), but adds:
the Terminating‑pod trigger in WorkerDiscovery, a `preStop` hook + configurable
`terminationGracePeriodSeconds` on worker pods, the wedge‑recovery timeout, and
tests for the race/overrun paths. Larger than the deletion‑cost PRD, smaller than a
new subsystem. Likely 1–2 PRs.

## 10. Open questions

| # | Question | Leaning |
|---|----------|---------|
| Q1 | Design A (operator‑driven via informer + preStop) or B (worker self‑checkpoints, operator reconciles)? | **RESOLVED → A** (operator sole KV writer, reuses suspend flow). |
| Q2 | `terminationGracePeriodSeconds` value / where configured? | **RESOLVED → 120s** via `WorkerTerminationGracePeriodSeconds` var on the operator‑generated Deployment + `worker-deploy.yaml`. Per‑pool override deferred. |
| Q3 | preStop implementation — poll KV state vs. call a worker endpoint that blocks until suspended? | **RESOLVED → neither; no preStop.** Distroless image has no shell and `httpGet` preStop fires once. The worker's SIGTERM handler drain‑waits until its sandbox is gone (or `SANDBOXD_DRAIN_DEADLINE`). |
| Q4 | Recovery for a session stuck in `Suspending` after a grace overrun? | A reconcile/timeout returns it to a resumable state (treat as needs‑resume). |
| Q5 | Skip the terminate checkpoint if a periodic checkpoint is very recent? | Optional optimization; default to always checkpointing on terminate. |
