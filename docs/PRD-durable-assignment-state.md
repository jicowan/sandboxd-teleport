# PRD — durable assignment state (Kubernetes as truth, Valkey as cache)

Status: **Implemented + verified live** (2026‑07‑08, operator `v17`). Related:
[architecture-sandboxd.md](sandboxd/architecture-sandboxd.md).

> **Update (2026‑07‑08, operator v19):** the mirror was narrowed to **durability‑
> critical transitions only** to relieve etcd write pressure
> (PRD-control-plane-scalability §5.4). It now fires on **Suspended** (idle‑suspend
> + checkpoint‑on‑terminate) and **periodic‑checkpoint advances**, plus **Delete**
> on reset — NOT on Resuming/Running/Suspending. Rationale: on a Valkey wipe a
> Running session is unrecoverable to its live RAM regardless (its worker binding is
> wiped too) and recovery falls back to the last snapshot, so mirroring Running
> bought no recovery — only etcd writes. Resume now does **zero** etcd writes;
> `kubectl get sessions` shows the last *durable* state (Suspended/Absent), not live
> Running. Verified live. Original design below.

> **As built:** the operator mirrors each authoritative session transition into
> `Session.status` (via a `SessionMirror` fired at the `casSession` /
> `PutSessionCAS` / `DeleteSession` choke points) and rebuilds the Valkey session
> cache from the `Session` CRs on startup (`SessionRebuilder`, a manager Runnable,
> leader‑only). `SessionStatus` gained the lossless‑mirror fields (`pool`,
> `workerPod`, `ports`, `health`, `iamRoleArn`; `lastActiveAt` mirrored coarsely on
> transitions, not per router stamp). Worker/idle entries are NOT persisted (they
> self‑heal via the pod informer + prune loop). A one‑time backfill of pre‑existing
> **Suspended** sessions was done via `hack/backfill-session-status.sh` (script, not
> startup code — single test env, one‑off). Verified live: deleted 5 suspended
> sessions from Valkey → restarted operator → rebuild restored all 5 from the CRs →
> a rebuilt session teleport‑resumed from its recovered snapshot. The optional
> Valkey PVC/AOF (§7) was NOT added.

## 1. Summary

The Valkey assignment table is today the **only** durable copy of session state,
and the Valkey pod has **no persistence** — no PV/PVC, `--save "" --appendonly no`.
If it restarts or reschedules, the entire session index is lost: the S3
checkpoints survive but become **orphaned** (nothing knows which snapshot belongs
to which session, or that a suspended session exists at all).

Fix: make **Kubernetes (the `Session` CR in etcd) the durable source of truth** and
treat **Valkey as a rebuildable cache**. The operator mirrors each authoritative
session transition into `Session.status` (which it does *not* write today), and on
startup **rebuilds the Valkey table** from the `Session` CRs plus a light S3
reconcile. A Valkey wipe then becomes a non‑event.

## 2. Problem — the current failure mode

- **Valkey is in‑memory only.** Confirmed: the Valkey Deployment has no volumes and
  runs `--save "" --appendonly no` (persistence disabled). `strategy: Recreate`, one
  replica. A pod restart / node drain / rollout loses 100% of the table.
- **The table is the sole index.** Keys `session:<sid>`, `worker:<pod>`,
  `pool:<pool>:idle`. Sessions carry `{state, pool, workerPod, workerPodIP, image,
  snapshotURI, ports, health, lastActiveAt}`.
- **The `Session` CR is a hollow spec.** The operator writes `Session.status`
  **nowhere** today (verified — zero status writes). The CRD *has* a status
  subresource with exactly the right fields (`phase`, `workerPodIP`, `snapshotURI`,
  `image`, `lastActiveAt`), but they're never populated. So there is no durable
  mirror to recover from.
- **S3 checkpoints are safe but orphaned on loss.** They live at
  `sandboxes/<sid>/<snap-…>/` independent of Valkey — durable, but unreferenced
  without the index.

Net: a single stateless pod restart can strand every suspended user's session
(they'd cold‑start and silently lose all checkpointed state), and orphan S3 objects
that GC may or may not reclaim.

## 3. Why "Kubernetes as truth" over "persist Valkey to S3"

The literal ask was "persist the KV to S3 and restore it." We choose the stronger
design instead:

- **etcd is already durable + backed up** as part of the cluster; no new S3 plumbing
  for the index, no second consistency problem, no RDB‑flush staleness window.
- It's the **idiomatic operator pattern**: the CR is the desired/observed state; a
  cache is rebuildable. Valkey becomes genuinely disposable, not "disposable but
  remember to back it up."
- The `Session` CR *should* reflect reality anyway (so `kubectl get sessions` is
  truthful and other controllers can observe it) — this PRD makes that real as a
  side benefit.

Persisting Valkey to S3 (the alternative) is kept only as a **fallback** if
mirroring to etcd proves too write‑heavy (§8).

## 4. What must be durable (and what must not)

- **Session state — YES.** The authoritative per‑session record. Mirrored to
  `Session.status`.
- **Worker entries (`worker:<pod>`) — NO.** They're inherently ephemeral: the
  pod‑label informer rebuilds them from live pods on startup, and the prune loop
  reaps stale ones. Persisting them would just be stale on recovery. Leave them
  Valkey‑only; they self‑heal.
- **Idle set (`pool:<pool>:idle`) — NO.** Derived from worker entries; rebuilt by
  discovery.

So only **session** records need durability. That keeps the change small.

## 5. Proposed design

### 5.1 Mirror authoritative session writes to `Session.status`

Every authoritative session write funnels through a small number of choke points:
`Workflow.casSession` (resume.go) and `Suspender.casSession` (suspend.go), plus
`PutSessionCAS` / `DeleteSession`. After a successful KV write, also update the
corresponding `Session.status` (subresource) with the mirrored fields:

| KV `SessionEntry` | → `Session.status` |
|---|---|
| `State` | `phase` |
| `WorkerPodIP` | `workerPodIP` |
| `SnapshotURI` | `snapshotURI` |
| `Image` | `image` |
| `LastActiveAt` (unix ms) | `lastActiveAt` (metav1.Time) |
| `Pool`, `WorkerPod`, `Ports`, `Health`, `IAMRoleARN` | (see 5.2) |

Notes:
- **KV stays the write‑hot path**; the etcd mirror is a best‑effort follow‑write, so
  a transient etcd hiccup never blocks a resume/suspend (it reconciles later).
- **Not every field fits `status` cleanly.** `pool` is on `Session.spec` (poolRef);
  `image`/`ports`/`iamRoleArn` for a broker‑created (lazy) session may only exist in
  KV. Decision (Q1): extend `SessionStatus` with the few missing durable fields
  (`pool`, `workerPod`, `ports`, `health`, `iamRoleArn`) so a rebuild is lossless,
  OR resolve them from `spec` + the pool template on rebuild. Lean: add the missing
  fields to `status` — a rebuild must not depend on re‑resolving a template that may
  have changed.
- **`lastActiveAt` is stamped per request by the router** (high frequency). Do NOT
  mirror every stamp to etcd (write amplification). Mirror it lazily / coarsely
  (e.g. only on state transitions, or throttled) — idle detection tolerates a stale
  `lastActiveAt` on recovery (worst case: a slightly early/late suspend).

### 5.2 Rebuild Valkey from etcd (+ S3 reconcile) on startup

Add an operator startup step (a Runnable, before serving `/resume`) that repopulates
the session cache when Valkey is empty/stale:

1. **List `Session` CRs** in the resume namespace. For each with a non‑empty
   `status.phase`, write the `session:<sid>` KV entry from the mirrored status
   fields (skip `Absent`).
2. **Reconcile against S3** (light): list `sandboxes/*` prefixes. A session whose
   status says `Suspended` but whose `snapshotURI` object is missing → mark
   needs‑attention (don't fabricate). A snapshot prefix with **no** owning Session →
   an orphan (leave for GC; log it).
3. **Worker entries rebuild themselves** via the existing pod informer + prune loop
   — no action needed.

After rebuild, normal operation resumes: a user reconnecting to a `Suspended`
session teleport‑restores from its (now‑known) `snapshotURI`.

### 5.3 When to rebuild

- **Always on operator startup** is simplest and safe (idempotent: writing the KV
  entry from status is a no‑op if it already matches). It also covers "Valkey
  restarted but operator didn't."
- Optionally, a **periodic reconcile** (like the worker prune loop) to catch drift
  between etcd and Valkey during normal operation — low frequency.

### 5.4 Consistency model

- **KV remains authoritative during normal operation** (CAS‑on‑version, single
  writer). etcd `status` is a **downstream mirror**.
- **On recovery, etcd is authoritative** (KV is empty/untrusted) — rebuild from it.
- The window of loss shrinks from "everything since the last Valkey start" (today:
  total) to "any transition not yet mirrored to etcd" (a follow‑write behind by
  milliseconds, and self‑correcting on the next transition or periodic reconcile).

## 6. Interaction with existing behavior

- **GC** already reasons about TTL + orphans; the S3 reconcile (5.2) complements it
  (an orphan snapshot with no Session is exactly what GC reaps).
- **Teleport / resume** is unchanged — it reads the (now‑durable) `snapshotURI`.
- **HA operator:** the mirror write and rebuild must be safe under leader election
  (only the leader rebuilds; mirror writes are idempotent CAS‑style on the CR's
  resourceVersion).
- **Session CR truthfulness:** `kubectl get sessions` starts showing real
  phase/worker — a usability win, and other controllers can now observe session
  state.

## 7. Optionally: give Valkey a PV anyway (defense in depth, cheap)

Independent of the above, enabling Valkey AOF on a small PVC (and dropping
`strategy: Recreate` risk) would make the *common* case (pod restart on the same
node) recover instantly from disk without the etcd rebuild. This is a **cheap
complementary** change (a PVC + `--appendonly yes`), not a substitute — etcd remains
the disaster‑recovery truth (PVC loss, node loss). Recommend doing both: PV for fast
local restart, etcd mirror for durability. (Q3.)

## 8. Fallback (if etcd mirroring is too write‑heavy)

If the `status` write rate proves problematic (it shouldn't — transitions are
infrequent and `lastActiveAt` is throttled), fall back to **periodic Valkey RDB →
S3** snapshots + restore‑on‑cold‑start. Simpler write path, but a staleness window
and a second source of truth. Kept as a documented alternative only.

## 9. Testing / acceptance

1. **Mirror:** a resume/suspend transition writes both KV and `Session.status`;
   fields match the mapping (5.1).
2. **Rebuild:** wipe Valkey (delete the pod / flush), restart the operator → the
   session cache is repopulated from `Session` CRs; a `Suspended` session then
   teleport‑restores from its `snapshotURI` with state intact.
3. **Orphan detection:** an S3 snapshot with no owning Session is logged/left for GC;
   a Session whose snapshot is missing is flagged, not fabricated.
4. **Write‑path resilience:** an etcd write failure during a transition does not
   fail the resume/suspend; the mirror self‑corrects on the next transition/reconcile.
5. **Worker rebuild:** after a Valkey wipe, worker entries repopulate from the pod
   informer (no persistence needed).

Acceptance: deleting the Valkey pod loses no session recoverability — every
suspended session still resumes from its checkpoint after the operator rebuilds.

## 10. Effort estimate

Medium. Mostly operator work: a `Session.status` writer hooked into the two
`casSession` choke points + `PutSessionCAS`/`DeleteSession`; a startup rebuild
Runnable (list Sessions → repopulate KV; light S3 reconcile); a few added
`SessionStatus` fields + regenerated CRD/deepcopy; RBAC already allows
`sessions/status` (verify). Optional PVC/AOF is a one‑line manifest change. No data‑
path change; no new component.

## 11. Open questions

| # | Question | Leaning |
|---|----------|---------|
| Q1 | Add missing durable fields (`pool`,`workerPod`,`ports`,`health`,`iamRoleArn`) to `SessionStatus`, or re‑resolve from spec+template on rebuild? | Add to `status` — a rebuild must be lossless and not depend on a possibly‑changed template. |
| Q2 | Mirror `lastActiveAt` how often? | Throttled / on transitions only — never per router stamp (write amplification); idle detection tolerates staleness. |
| Q3 | Also give Valkey a PVC + AOF? | Yes — cheap, fast local‑restart path; etcd mirror remains the DR truth. |
| Q4 | Rebuild only on startup, or also periodic reconcile? | Startup always; add a low‑freq periodic reconcile if drift is observed. |
| Q5 | HA: who rebuilds under leader election? | Leader only; mirror writes idempotent on CR resourceVersion. |
| Q6 | Keep Valkey→S3 as a real fallback or drop it? | Document as fallback only (§8); don't build unless mirroring proves too heavy. |
