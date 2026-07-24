# Design: reclaim orphaned worker bindings (stuck-busy workers)

**Status:** IMPLEMENTED + deployed (operator v33, 2026-07-21). Reclaim sweep in
`WorkerDiscoveryReconciler` with 300s grace (default), two-strike + version-stable
gate; strict rule (orphan / Suspended / rebound only); swallowed suspend-sweeper
error now logged. Metric `sandboxd_worker_reclaimed_total{reason}`. Flag
`--worker-reclaim-grace-seconds` (env `SANDBOXD_WORKER_RECLAIM_GRACE_SECONDS`,
default 300, 0=off).

> **Scope note learned in deployment (2026-07-21).** The strict rule intentionally
> does NOT cover a session whose KV state is still `Running`/`Resuming` and bound to
> THIS pod but whose actual sandbox has died (Gap-B). In KV that is indistinguishable
> from a healthy session, so auto-reclaiming it would risk racing a live session.
> The strict sweep therefore auto-heals only: (a) busy with no session id, (b) session
> entry gone (orphan), (c) session `Suspended`, (d) session rebound to another pod.
> Gap-B stale-`Running` entries are handled by: the now-visible suspend-sweeper error
> log (operator surfaces the failing `/suspend`), and operator/manual correction of the
> session state (which then makes reclaim/GC act). This matched the live fleet: of 6
> stuck workers, 2 were true orphans (auto-reclaimed) and 4 were Gap-B stale-`Running`
> residue requiring the manual KV cleanup in §8.

**Status (original):** proposed (design-first, pending review)
**Problem owner:** control-plane / assignment table
**Related:** PRD-control-plane-scalability (pool counts), PRD-session-garbage-collection (session footprint reap), `worker_discovery.go` (existing `PruneStaleWorkers`), `suspend.go` (`suspendOne`, sweeper)

---

## 1. Symptom

The `aio-pool` WarmPool ran **8/8** replicas with `spec.replicas: 4`, `minIdle: 2`.

The reconciler sizes a pool as:

```
replicas = max(spec.Replicas, busy + minIdle)      // warmpool_controller.go
busy     = SCARD(pool:<p>:all) − SCARD(pool:<p>:idle)   // CountWorkers, O(1)
```

Observed: `all=8`, `idle=2` ⇒ `busy=6` ⇒ `max(4, 6+2)=8`. The pool is pinned at 8
because six workers are marked **busy** in KV but their sessions are gone and their
sandboxes are dead (probed `/v1/health` → 404).

## 2. Root cause

A worker's `busy` binding (`worker:<pod>` = `{state:busy, sid:X}`, plus membership in
`pool:all` but not `pool:idle`) is created by `ClaimIdleWorker` and released by exactly
one place on the happy path: `suspendOne` → `ReleaseWorker` (idle-suspend or `/reset`),
or `RemoveWorker` when the pod dies. **Nothing reconciles a `busy` binding whose bound
session no longer exists or is no longer live.** Two independent ways it leaks:

- **A — orphan binding (no session entry at all).** If a `session:<sid>` key is deleted
  without releasing the worker (manual `DEL`, an aborted fork cleanup, a partial GC/reset
  where `DeleteSession` ran but `ReleaseWorker` didn't — note `assign.go:180 DeleteSession`
  only removes the session key + due-indexes, never the worker binding), the `worker:*`
  entry stays `busy` forever. GC can't see it: GC iterates `ListSessions`, and there is no
  session to iterate.

- **B — dead session shielded from GC.** For a `Running` session whose sandbox has died,
  GC's `abandoned()` check is gated by `workerHolds()` — if the `worker:*` entry is still
  `busy` + bound to that sid, GC treats the session as "held" and does **not** reap it.
  Meanwhile the idle **suspend-sweeper** picks the session up (it's overdue in
  `idx:suspend:due`), calls `suspendOne`, and the worker `/suspend` fails (sandbox is a
  404). That error is swallowed (`suspend.go` increments an error metric and `continue`s),
  so the session never reaches `Suspended`, never frees its worker, and the pair wedges
  each other indefinitely.

Both are **production-reachable**, not just test residue: any `suspendOne` failure after
`Suspending` but before `ReleaseWorker`, or any crash between session-delete and
worker-release, lands in state A or B.

## 3. The subtle constraint: the claim→bind window is *legitimately* "busy with no session"

`ClaimIdleWorker` marks a worker `busy`+`sid` **before** the session KV entry exists.
`startAndBind` then writes `Resuming`, drives `/run|/restore`, waits ready, writes
`Running`. So during a normal cold-start/resume there is a short window (sub-second to a
few seconds, bounded by `ResumeDeadline`) where `worker:<pod>` is `busy`+`sid=X` and
`session:X` does not yet exist (or is `Resuming`). **A reclaimer must not free a worker in
this window**, or it will yank a worker out from under an in-flight resume.

This is why "busy worker with no live session" cannot be reclaimed *immediately* — it must
be **grace-gated**: only reclaim a binding that has been anomalous for longer than any
legitimate resume could take.

## 4. Proposed fix

Add a **level-triggered reclaim sweep** that returns a stuck-busy worker to the idle pool
when its binding is provably orphaned and has aged past a grace period. Put it next to the
existing `PruneStaleWorkers` sweep in `WorkerDiscoveryReconciler` — same package, same
loop cadence, same "level-triggered self-heal of KV worker state" charter. The worker
discovery reconciler is already the **sole writer** of worker/idle state, so keeping
reclaim here preserves that invariant (no new writer racing it).

### 4.1 Reclaim rule (per busy worker, evaluated against live truth)

For each `worker:<pod>` with `state == busy`:

```
1. Pod must exist + be Ready.           // else PruneStaleWorkers / remove() already handles it
2. Look up session := GetSession(w.SID).
3. Decide:
   a. session NOT FOUND               -> ORPHAN  (state A)
   b. session.State == Suspended       -> STALE   (suspend completed but ReleaseWorker didn't;
                                                    a Suspended session holds no worker)
   c. session bound to a DIFFERENT pod  -> STALE   (session.WorkerPod != pod: rebind happened,
       (session.WorkerPod != "" &&                  this pod's binding is a leftover)
        session.WorkerPod != pod)
   d. otherwise (Running/Resuming/Suspending on THIS pod, or bound here) -> KEEP
4. If ORPHAN or STALE: require the anomaly to have persisted >= ReclaimGrace
   (default 5m, comfortably > ResumeDeadline) before acting — see 4.2 for how we age it
   without a schema change.
5. Reclaim = ReleaseWorker(pod, pool) [busy->idle, clears sid, re-adds to pool:idle,
   bumps version] + PoolChanged(pool) so the WarmPool status refreshes immediately.
```

Case (b) also means we can **stop `suspendOne` from wedging**: once the session is
`Suspended`, reclaim frees the worker even if the original `ReleaseWorker` was lost.

### 4.2 Aging without a schema change (avoiding the false-positive race)

`WorkerEntry` has no timestamp, and I'd rather not add hot-path clock writes. Two options:

- **Option 1 — two-strike confirmation (recommended).** Keep an in-memory
  `map[pod]firstSeenAnomalous` in the reconciler. A binding is reclaimed only if it was
  observed anomalous on a previous sweep too, i.e. anomalous for ≥ one full sweep interval,
  and total elapsed ≥ `ReclaimGrace`. Cheap, no KV/schema change, no clock writes. Cleared
  when the pod goes healthy/idle or disappears. Downside: state resets on operator restart
  (first post-restart sweep just re-arms; correctness preserved, at worst one extra grace
  interval of delay). This is the same "confirm across sweeps" pattern the codebase already
  tolerates for eventual reconciliation.

- **Option 2 — version-stability check.** Record `(pod → observed WorkerEntry.Version)` and
  only reclaim if the version is unchanged across the grace window (a real claim/resume
  bumps the version). Strictly stronger against the claim race, but essentially the same
  bookkeeping as Option 1 with an extra field. Fold into Option 1: store
  `{firstSeen, version}` and require both aged **and** version-stable.

**Chosen:** Option 1 augmented with the version check from Option 2 — reclaim iff
`anomalous this sweep AND anomalous last sweep AND version unchanged since first-seen AND
elapsed ≥ ReclaimGrace`. This makes a false reclaim during the claim→bind window
essentially impossible (that window bumps the version and is far shorter than the grace).

### 4.3 Where it runs

- New method `ReclaimOrphanBindings(ctx) (int, error)` on `WorkerDiscoveryReconciler`.
- Driven by the **same** `StartPruneLoop` (rename intent to "reconcile loop") — after
  `PruneStaleWorkers` each tick, or a sibling loop on its own interval. Leader-gated already
  (the whole reconciler runs under manager leader election).
- Needs read access to sessions: `WorkerDiscoveryReconciler` gets the `assign.Client`
  already (`r.KV`), which has `GetSession`. No new dependency.

### 4.4 Config / flags

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `--worker-reclaim-grace-seconds` | `SANDBOXD_WORKER_RECLAIM_GRACE_SECONDS` | `300` | How long a binding must stay anomalous before reclaim. Must exceed `ResumeDeadline`. `0` disables reclaim (parity with GC's opt-in ethos). |

Reuse the existing prune-loop interval for the sweep cadence (default 30s), so grace is
enforced by 4.2's multi-sweep confirmation, not a second ticker.

### 4.5 Observability

- Log each reclaim: `pod`, `pool`, `sid`, `reason` (orphan|suspended|rebound), `agedSec`.
- Metric: extend the existing GC/worker metrics — e.g. `sandboxd_worker_reclaimed_total{reason}`.
- Also fix the **swallowed suspend error** (independent, small): `SweepOnce` should surface a
  per-session failure count so a wedged `/suspend` is visible (today it's silent). At minimum
  `log.Error` on the first failure per sid, not just a metric increment.

## 5. Why not put it in GC?

GC is session-centric (`ListSessions` → classify → reap the session's S3+KV+CR footprint).
The orphan case (A) has *no session* to classify, and the shielding case (B) is precisely GC
being blocked by the stuck binding. Worker-binding health is the discovery reconciler's
domain (it already owns idle/busy writes and the `PruneStaleWorkers` self-heal). Splitting it
keeps each sweep's invariant clean: GC owns *session footprint*, discovery owns *worker
state*. They compose: once reclaim frees the worker, GC's `abandoned()` is no longer shielded
and can reap the dead session normally on the next pass.

## 6. Blast radius / safety

- Only ever moves a worker **busy → idle** (via the existing `ReleaseWorker` script, which
  bumps version). Never deletes a worker, never touches a session, never touches S3.
- Grace + version-stability makes a false reclaim during a legitimate resume effectively
  impossible; even if one occurred, the effect is "an in-flight resume's worker returns to
  idle" → the resume's next CAS/`/run` fails and the router retries a fresh claim — degraded,
  not corrupting.
- Idempotent: reclaiming an already-idle worker is a no-op.
- Leader-gated (single writer). No change to the claim/suspend hot paths.

## 7. Test plan

Envtest / unit (fake KV) in `worker_discovery_test.go`:

1. **Orphan (A):** seed `worker:p1{busy,sid=X}`, pod Ready, no `session:X`. First sweep: no
   reclaim (arming). Advance clock past grace + second sweep: reclaimed → idle, `pool:idle`
   contains p1, `busy` count drops.
2. **Suspended-shield (B):** seed `worker:p2{busy,sid=Y}` + `session:Y{Suspended}`. Aged →
   reclaimed. Then assert GC's `abandoned`/TTL can now act on Y.
3. **Claim→bind window (negative):** seed `worker:p3{busy,sid=Z}`, no session yet, then
   before grace elapses create `session:Z{Resuming}` and bump worker version. Assert **not**
   reclaimed (version changed / became live).
4. **Rebound (c):** `worker:p4{busy,sid=W}` but `session:W.WorkerPod=p9`. Aged → reclaimed.
5. **Live (KEEP):** `worker:p5{busy,sid=V}` + `session:V{Running,WorkerPod=p5}`. Never
   reclaimed regardless of age.

## 8. One-off cleanup of the current live leak (separate, after this lands or before)

The six stuck bindings on the live cluster are my own test residue (dead sandboxes). Manual
remediation, independent of the code fix:

```
# for each stuck pod: return to idle (or just delete residue sessions and let reclaim run)
ReleaseWorker(pod, aio-pool)          # busy->idle, re-add to pool:idle
DEL session:sess-mtls-2 / -3 / -test  # orphan session keys (dead sandboxes)
DEL session:sess-e2e                  # CR already deleted; sandbox dead
```

Expected: `busy 6→0`, `replicas → max(4, 0+2) = 4`, pool scales in to 4/4. With the reclaim
sweep deployed, this cleanup becomes automatic (aged orphans self-heal).

**Done 2026-07-21 (operator v33 live):** the reclaim sweep auto-reclaimed the 2 true
orphans (`sess-fork-e2e-0/1`, `reason=orphan`, `agedSec=300`) → pool 8→6. The other 4
were Gap-B stale-`Running` residue (no CR, sandbox 404); deleting their KV `session:*`
entries converted them to orphans, which reclaim then freed → **pool 4/4, busy=0**.
`sess-e2e`'s shielded S3 snapshot was removed. 5 legitimate sessions + their S3 dirs
untouched. Verified: `busy=0 idle=4 replicas=4`, all 4 worker bindings idle.

## 9. Open questions for review

1. **Grace default** — 300s ok? It must exceed `ResumeDeadline` (currently
   `--resume-deadline-seconds`); confirm that value on the live operator and set grace to
   `max(300, 2×ResumeDeadline)`.
2. **Reset action** — `suspendOne` `/reset` deletes the session and releases the worker; if
   `/reset` fails mid-way it can also orphan. Same reclaim covers it (session gone → orphan).
   No extra handling needed — confirm.
3. **Do we also want reclaim to fire on `Absent`/`Suspending` stuck states**, or keep the
   rule to {no-session, Suspended, rebound}? I lean strict (listed cases only) to minimize
   surface; `Suspending` that never completes is caught once it either finishes (→Suspended,
   case b) or the session is GC'd.
4. Should the swallowed-suspend-error fix (4.5) be part of this change or a separate small PR?
