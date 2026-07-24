# PRD — session garbage collection (reap the full session footprint)

Status: **Implemented + verified live** (2026‑07‑09, operator `v22`). Related:
[PRD-control-plane-scalability.md](PRD-control-plane-scalability.md),
[PRD-durable-assignment-state.md](PRD-durable-assignment-state.md),
[architecture-sandboxd.md](../architecture-sandboxd.md),
[admin-guide-crds.md](../admin-guide-crds.md).

> **As built (2026‑07‑09, operator v22).** The `gc.Collector`
> (`internal/gc/gc.go`) now reaps the whole session footprint across four passes:
> **TTL** (Suspended past retention — per‑session `ttlAfterSuspendSeconds` else
> `--default-ttl-after-suspend-seconds`; now also deletes KV + CR, not just S3),
> **abandoned** (non‑Suspended entry whose worker is gone / no longer holds it, idle
> past `--abandoned-grace-seconds` default 1h → KV + CR), **orphan‑S3** (unchanged),
> and **orphan‑CR** (operator‑owned CR, dead phase, no KV → delete). CR deletion is a
> new `controller.SessionReaper` that deletes **only** operator‑owned CRs (label
> `sandboxd.io/created-by=operator`, stamped by `planFor` on lazy create) and
> tombstones user‑declared CRs to `Absent`; the delete is guarded by
> UID+resourceVersion. `--gc-dry-run` **defaults true** (classify + record via
> `sandboxd_gc_candidates` / `sandboxd_gc_reaped_total`, mutate nothing). Migration:
> `hack/backfill-created-by-label.sh`. **Verified live:** in dry‑run the classifier
> matched §3 exactly against 16 real CRs (abandoned:6, orphanCR:8, ttlExpired:0);
> then armed (`SANDBOXD_GC_DRY_RUN=0`, `--default-ttl-after-suspend-seconds=604800`)
> and the fleet dropped 16 → 6, reaping the 14 dead sessions' CRs + KV entries and
> leaving the 5 Suspended + 1 live session untouched. The design/analysis below is
> preserved as written.

## 1. Summary

A session now lives in **four stores**: the S3 checkpoint (`sandboxes/<sid>/`), the
Valkey assignment entry (`session:<sid>` + the `idx:suspend:due` / `idx:checkpoint:due`
ZSETs), the `Session` custom resource in etcd, and — while active — a worker binding.
Today's garbage collector (`internal/gc/gc.go`, the `Collector`) reaps only the first
two, and only for one narrow class of session. As a result, **every non‑live session
class leaks** in at least one store. This PRD characterizes the leak from the live
test env, then proposes a unified session‑lifecycle GC that reaps the *whole*
footprint (S3 + KV + indexes + Session CR) across *all* the ways a session goes dead.

The goal is a control plane that doesn't accumulate dead state indefinitely — closing
the gap the durability work (etcd mirror) and the scalability work (idx ZSETs)
widened by adding two more stores a session can leak into.

## 2. What exists today

`Collector.SweepOnce` (`internal/gc/gc.go`), enabled live via `SANDBOXD_ENABLE_GC=1`
(the `--enable-checkpoint-gc` flag reads that env; **it is ON in the live operator** —
correcting an earlier note that said it was off), running every 5 min under a scoped
least‑privilege S3 identity (list+delete on `sandboxes/*` only), does exactly two
things:

1. **TTL reap.** For each `session:*` entry that is `Suspended` **and** has a
   `SnapshotURI`, if `now − max(lastCheckpointAt, lastActiveAt) > ttlAfterSuspendSeconds`
   (and `ttl > 0`): delete the S3 prefix, then `kv.DeleteSession` (which removes the
   `session:<sid>` key **and** ZREMs it from both due‑indexes).
2. **Orphan reap.** Any `sandboxes/<sid>/` prefix in S3 referenced by **no** KV
   session entry → delete the S3 objects.

What it never touches:
- **The `Session` CR.** `gc.DeleteSession` is KV‑only. `SessionMirror.Delete`
  (reset path) sets `status.phase = Absent` but explicitly leaves the CR — its own
  comment says *"GC of the CR itself is a separate concern"* (`session_durability.go:73‑76`).
  A repo‑wide grep confirms **no code path deletes a `Session`‑typed object.**
- **Any session that isn't `Suspended`‑with‑a‑snapshot.** The TTL path `continue`s on
  everything else (`gc.go:91`); the orphan path only looks at S3 keys, so a dead
  session with no snapshot is invisible to both.

## 3. The leak, measured live (2026‑07‑09)

16 `Session` CRs, 12 `session:*` KV entries, 5 S3 snapshot dirs. Cross‑referencing the
four stores gives a clean taxonomy of drift:

| Class | Example sessions | S3 | KV | CR | idx | Reaped today? |
|-------|------------------|----|----|----|----|---------------|
| **A. Suspended, TTL unset** | jicowan, redis, memcached, valkey, mmeckes (5) | ✓ | ✓ | ✓ | — | **No** — `ttlAfterSuspendSeconds` is empty (`0` = keep forever); TTL reap never fires |
| **B. Zombie‑Running, KV+CR, no snapshot** | alice, bob (2) | ✗ | ✓ | ✓ | ✗ | **No** — not `Suspended`, no snapshot to orphan‑reap |
| **C. Zombie‑Running, KV‑only (no CR)** | caddy, echo, httpd, nginx (4) | ✗ | ✓ | ✗ | ✗ | **No** — invisible: never `Suspended`, no CR to show |
| **D. CR‑only orphan (empty phase, no KV)** | iam3/4/5, iamtest/2, rename1, termtest, consoletest‑narwhal (8) | ✗ | ✗ | ✓ | — | **No** — GC reads KV, never enumerates CRs |
| **E. Live Running** | idxtest (1) | ✗ | ✓ | ✓ | ✓ | correctly retained |

Only class E is healthy. **Every other class leaks in at least one store**, and none
is reaped by today's GC. Root causes, in order of impact:

1. **No TTL is ever set (Gap 1).** Nothing defaults `ttlAfterSuspendSeconds`; all 5
   Suspended sessions have it empty. GC is *running* but its one working reaper is
   structurally inert — the S3 snapshots that back suspended sessions live forever.
2. **The `Session` CR is never reaped (Gap 2).** Even a successful TTL reap (once a
   TTL exists) deletes S3 + KV but leaves the CR in etcd forever. Classes A, B, D all
   keep a CR nothing will ever remove.
3. **Dead non‑suspended sessions are ignored entirely (Gap 3).** Classes B, C, D were
   created lazily (`resume_glue.go planFor`), used briefly, then their worker died
   (scale‑in, pod churn, tests). They're neither `Suspended` (TTL path skips) nor have
   an S3 snapshot (orphan path skips). Nothing reaps them, in any store. This is the
   largest class (14 of 16 CRs).

## 4. Why this matters (and why it didn't before)

- **etcd pressure compounding the scalability work.** The durability mirror
  (PRD‑durable‑assignment‑state) made every durable transition an etcd write; now the
  CRs those writes create are never cleaned up. Dead `Session` objects sit in etcd —
  shared cluster infra — indefinitely. At the fleet scale the scalability PRD is
  designing for, an unbounded set of dead CRs is a real etcd object‑count liability.
- **S3 cost/retention.** Suspended checkpoints (RAM + FS images, not small) accumulate
  with no expiry. A privacy/retention story ("we delete your session N days after you
  stop using it") is currently unenforceable.
- **KV drift.** Zombie `session:*` entries (classes B, C) point at dead workers. The
  router fast‑path‑liveness and resume fall‑through already tolerate this (they
  fence on `workerHolds`/`workerLive` and re‑resume), so it's not a *correctness* bug
  — but it's unbounded growth of the table the sweepers and rebuild iterate.
- **Operator legibility.** `kubectl get sessions` is dominated by dead entries, half
  with empty phase. Hard to see the live fleet.

None of this bites at today's dozens‑of‑sessions scale — same disposition as the
scalability PRD. But it's the natural completion of that work: we made the hot paths
O(due), then we need to make the *table itself* bounded.

## 5. Goals / non‑goals

### Goals
- Reap the **entire** footprint of a dead session — S3 snapshot, `session:*` KV entry,
  both due‑indexes, **and** the `Session` CR — as one atomic-enough unit.
- Cover **all** dead‑session classes (A–D), not just Suspended‑with‑snapshot.
- Make retention **configurable and actually enforced** (a working default TTL, plus
  per‑session/per‑template override).
- Preserve every correctness property: never reap a live/Running session with a live
  worker, never reap a session mid‑transition, keep the single‑writer (operator/leader)
  and durable‑recovery guarantees intact.
- Distinguish **operator‑created (lazy)** CRs from **user‑declared** CRs — GC may
  delete the former; the latter it should return to `Absent`, not delete (the user
  owns the object).

### Non‑goals
- Not changing the checkpoint/restore or teleport mechanics.
- Not building the subject→entitlement authz gate (separate, shared‑keystone work).
- Not S3 lifecycle policies as the *primary* mechanism (they can't reap the CR/KV and
  don't know session semantics) — though a bucket lifecycle rule is a reasonable
  belt‑and‑suspenders backstop (§7).

## 6. Proposed design

### 6.1 A default retention TTL that actually fires (fixes Gap 1)

- Add an operator flag `--default-ttl-after-suspend-seconds` (env
  `SANDBOXD_DEFAULT_TTL_AFTER_SUSPEND_SECONDS`), default e.g. `604800` (7 days).
- `gc_glue.go ttlFor` returns `Session.spec.lifecycle.ttlAfterSuspendSeconds` when set
  (>0), else the operator default, else 0 (never). This makes the existing TTL reap
  live without forcing every Session author to set a field. `0` on the flag preserves
  today's keep‑forever behavior for anyone who wants it.
- Alternatively/additionally default the field at lazy‑create time in
  `resume_glue.go planFor` — but resolving at `ttlFor` is less invasive and lets the
  default be re‑tuned without touching existing CRs.

### 6.2 Reap the Session CR as part of the reap (fixes Gap 2)

Extend the reap unit so deleting a session removes **all four** stores. Introduce a
`SessionReaper` interface the `Collector` calls after a successful S3 + KV delete:

```
type SessionReaper interface {
    // Reap removes the durable Session CR for sid IF it is operator-owned
    // (lazy-created). User-declared CRs are reset to Absent, not deleted.
    Reap(ctx context.Context, sid string) error
}
```

Implemented in the controller package (has the k8s client), symmetric with
`SessionMirror`. Ownership signal: a label/annotation the operator stamps on
lazy‑created CRs (e.g. `sandboxd.io/created-by: operator`) in `planFor`. GC deletes
only labeled CRs; unlabeled (user‑declared) CRs get `status.phase = Absent` and keep
the object. This mirrors the existing `SessionMirror.Delete` ownership caution.

Ordering: delete S3 → `kv.DeleteSession` (KV + indexes) → `Reaper.Reap` (CR). Each
best‑effort/idempotent so a partial failure self‑heals next sweep (the orphan/abandoned
passes below re‑detect any store left behind).

### 6.3 Reap abandoned non‑suspended sessions (fixes Gap 3 — the big one)

Add a third pass to `SweepOnce` alongside TTL and orphan reaping: an **abandoned‑session
reap**. A KV `session:*` entry is *abandoned* when it is **not** in a healthy live
state — i.e. its bound worker no longer exists / no longer holds it — for longer than a
grace period. Concretely, for each non‑`Suspended` entry:

- Resolve worker liveness the same way resume/router already do (`workerHolds` /
  `GetWorker`: the `worker:<pod>` entry exists, is `busy`, and its `sid` matches).
- If the worker is **gone or no longer holds this sid**, and `now − lastActiveAt >
  abandonedGraceSeconds` (new flag, default e.g. 1h; falls back to the entry's
  creation/first‑seen time when `lastActiveAt` is 0), the session is abandoned:
  `DeleteSession` (KV + indexes) + `Reaper.Reap` (CR). No S3 to delete (these classes
  have no snapshot; if one somehow exists it's caught by the orphan pass).

This directly reaps classes B and C. It must be conservative: the grace period + the
"worker doesn't hold it" check ensure we never reap a session that's simply
mid‑resume (Resuming has a bound worker) or briefly between requests.

### 6.4 Reap CR‑only orphans (fixes class D)

The abandoned pass above is KV‑driven, so it won't see class D (CR exists, no KV). Add
a **CR reconciliation pass** (leader‑only, low frequency — e.g. every GC interval or a
multiple of it): list `Session` CRs, and for each **operator‑owned** CR whose phase is
`Absent`/empty **and** that has no `session:*` KV entry **and** whose worker (if any in
status) is gone, past a grace: delete the CR. This is the mirror image of the orphan‑S3
pass — an orphan‑CR pass. Bounded by CR count; runs off the hot path.

> Design note: 6.3 + 6.4 together are "reconcile the four stores toward the union of
> what's actually alive." An alternative framing is a single reconciler that, per
> sweep, builds the set of live sessions (from live workers + valid snapshots) and
> reaps anything in *any* store not in that set, past grace. Cleaner conceptually;
> larger change. Recommend implementing as the three explicit passes first (TTL,
> abandoned‑KV, orphan‑CR + existing orphan‑S3), then consider unifying if the passes
> start duplicating logic.

### 6.5 Interaction with the durability mirror and idx ZSETs

- `kv.DeleteSession` already ZREMs both `idx:suspend:due` and `idx:checkpoint:due`
  (`assign.go:178‑185`) — no extra index work needed; the reap is index‑clean by
  construction.
- The etcd mirror fires `Delete`→`Absent` on reset today. The new `Reaper.Reap`
  supersedes that for GC‑driven removal (it deletes the CR outright for operator‑owned
  sessions rather than leaving an `Absent` tombstone). Reset semantics unchanged.
- The `SessionRebuilder` (startup KV rebuild from CRs) must not resurrect a
  just‑reaped session: since the reap deletes the CR (operator‑owned) there's nothing
  to rebuild from, which is correct. For user‑declared CRs left as `Absent`, rebuild
  already skips `Absent`/empty, so no resurrection.

## 7. Safety / backstops

- **Everything gated on grace periods + liveness checks**, never on a single sweep
  observation. A session must look dead for a configured grace before any store is
  touched.
- **Idempotent, best‑effort, self‑healing:** a partial multi‑store delete is re‑detected
  and completed next sweep.
- **Least privilege preserved:** S3 deletes stay under the scoped GC identity; CR
  deletes use the operator's existing RBAC — the manager ClusterRole already grants
  `delete` on `core.sandboxd.io/sessions` (kubebuilder marker at
  `warmpool_controller.go:113`), so no RBAC change is needed.
- **Optional S3 lifecycle rule** as an independent backstop (expire `sandboxes/*`
  objects after M days) so even a totally wedged GC can't leak S3 forever. Belt‑and‑
  suspenders; not the primary path (can't reap CR/KV).
- **Dry‑run / observability first:** add `sandboxd_gc_reaped_total{store,class}` and a
  `--gc-dry-run` mode that logs what it *would* reap without deleting, so we validate
  the abandoned/orphan‑CR classification against the live env before arming it.

## 8. Rollout

1. Land observability + `--gc-dry-run` (default dry). CR‑delete RBAC already exists
   (see §7), so no RBAC change is needed.
2. Run dry against the live env; confirm the taxonomy in §3 is classified correctly
   (the 14 dead CRs land in B/C/D, idxtest stays live).
3. Set the default TTL (§6.1) and arm CR reaping (§6.2) — reaps classes A, and B/C/D
   CRs once abandoned passes arm.
4. Arm the abandoned‑KV (§6.3) and orphan‑CR (§6.4) passes.
5. Backfill the existing live drift: a one‑time script (like
   `hack/backfill-session-status.sh`) can clear the current 14 dead sessions, or just
   let the armed GC reap them on its next sweeps.

## 9. Acceptance

- With default TTL set, a Suspended session past its retention has its S3 snapshot, KV
  entry, both indexes, and (operator‑owned) CR all gone within one GC interval.
- A session whose worker dies and never resumes is fully reaped after the abandoned
  grace period — no leftover KV entry or CR.
- No live/Running/Resuming session with a live holding worker is ever reaped.
- `kubectl get sessions` reflects (roughly) the live + recently‑suspended fleet, not an
  ever‑growing pile of dead entries.
- User‑declared Session CRs are never deleted by GC (reset to `Absent` only).

## 10. Open questions

| # | Question | Leaning |
|---|----------|---------|
| Q1 | Default TTL value? | 7 days (`604800`); tune later. `0` flag preserves keep‑forever. |
| Q2 | Delete operator‑owned CRs, or always just tombstone to `Absent`? | Delete operator‑owned (they're pure control‑plane bookkeeping); tombstone only user‑declared. |
| Q3 | Abandoned grace period? | ~1h default (`--abandoned-grace-seconds`); long enough to never race a slow resume. |
| Q4 | Three explicit passes vs. one unified "reconcile to live set"? | Three passes first (smaller, matches existing orphan‑S3 pass); unify later if they converge. |
| Q5 | Add S3 lifecycle backstop now? | Yes — cheap, independent, bounds the worst case regardless of GC health. |
| Q6 | Ownership signal for lazy‑created CRs? | Label `sandboxd.io/created-by=operator` stamped in `planFor`; GC keys CR‑deletion off it. |

## 11. Status

**Proposed — nothing built.** Live grounding captured 2026‑07‑09 (see §3). See
[[sandboxd-next-investigations]] and [[sandboxd-pending-work]] (memory) for the
cross‑session tracker.
