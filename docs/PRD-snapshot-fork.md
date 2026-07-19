# PRD — snapshot fork (branch many sessions from one common state)

Status: **Proposed** (analysis + plan; grounded in the shipped code on the
`checkpoint-restore` branch). Decision‑ready.

Related: [architecture-sandboxd.md](sandboxd/architecture-sandboxd.md),
[PRD-session-garbage-collection.md](PRD-session-garbage-collection.md),
[PRD-durable-assignment-state.md](PRD-durable-assignment-state.md),
[admin-guide-crds.md](sandboxd/admin-guide-crds.md).

## 1. Summary

Let a caller **fork a snapshot**: take one session to a desired "golden" state,
checkpoint it, then spawn **N independent sessions that all start from that identical
RAM+FS state** and diverge from there. Each fork is a first‑class session with its
own worker, its own future checkpoint lineage, and its own lifecycle — they share
only the read‑only base snapshot they were born from.

The motivating use case is **reinforcement learning / parallel rollouts**: drive an
environment (an agent, a browser, a tool sandbox, any workload) to a common starting
state once, then fan out K copies to explore K different action sequences / scenarios
from that exact state, comparing outcomes. It generalizes to any "branch from a known
good state" pattern: parallel test scenarios, tree‑search/beam rollouts,
what‑if/A‑B exploration, or a reproducible fixture that many jobs start from.

**The core mechanism already exists.** sandboxd's `/restore` reads a snapshot from an
arbitrary S3 prefix into a *new* sandbox id and never consumes or mutates the source
— which is exactly a fork. What's missing is control‑plane orchestration: a way to
mint N sessions pointing at one base snapshot, plus snapshot **pinning** so GC/suspend
don't reclaim a base while forks reference it.

## 2. Why this is mostly already possible (grounded in code)

- **Restore is a pure read of an arbitrary snapshot into a fresh sandbox.** The
  worker's `handleRestore` (`sandboxd/main.go:363`) takes `{sandboxId, image,
  snapshot, …}`, `downloadPrefix`s the snapshot from S3 (`s3.go` `GetObject` — read
  only), and `runsc restore`s a brand‑new gVisor sandbox (`runsc.go:352`). It rejects
  only a *locally* colliding `sandboxId` (`main.go:385`); it places **no lock** on the
  source snapshot and does not delete or move it. So pointing several restores at the
  **same** `snapshot` with **different** `sandboxId`s already yields several
  independent live sandboxes from one base — this is what teleport does today (restore
  onto a different worker), minus "don't delete the source, and do it more than once."
- **The resume path keys entirely off `SnapshotURI`.** In the operator's resume
  workflow (`internal/resume/resume.go`), a Suspended session with `cur.SnapshotURI`
  set drives `resumeFromSnapshot` → `/restore` with the session's own sid
  (`resume.go:267`, `:329`, `:393`). So "restore session X from snapshot S" is already
  the shape; a fork is just "**create session X′ whose `SnapshotURI` = S**" and let the
  existing restore path run.
- **Networking is rebuilt fresh per restore.** Each restored sandbox gets its own
  veth + nftables + fixed interior IP rebuilt on restore (`network.go`), so N copies on
  N workers don't collide on network identity. (One thing to validate live — §7.)
- **Checkpoint already produces a reusable, immutable‑by‑convention prefix.**
  `handleCheckpoint` (`main.go:294`) with `leaveRunning=true` writes
  `sandboxes/<sid>/snap-<ns>/…` and returns the prefix without tearing the sandbox
  down — a natural "golden snapshot" primitive that doesn't disturb the source
  session.

So the runtime needs **no change**. This is a control‑plane feature.

## 3. Where the current model blocks it

Today the assumption chain is **one user → one durable session → one snapshot lineage
→ one live sandbox**:

- The broker derives **one durable session id per principal** (`sess-<principal>-<hash>`),
  so identity maps to a single session; there is no first‑class "make another session
  seeded from this one."
- A session's `snapshotURI` is treated as *that session's own* checkpoint: idle‑suspend
  and checkpoint **overwrite** it (`main.go:349` sets `sb.Snapshot = prefix`), and
  teleport‑resume reads it back. Nothing models "session B restores from session A's
  snapshot, and A's snapshot must survive."
- **GC now reaps aggressively** (PRD‑session‑garbage‑collection): the orphan‑S3 pass
  deletes any `sandboxes/<sid>/` prefix **referenced by no session**, and TTL reaps a
  suspended session's snapshot after retention. A base snapshot that forks depend on
  must be **exempt** from both, or a fork's restore races a delete.

## 4. Goals / non‑goals

### Goals
- A caller can **create a base snapshot** from a session at a chosen state, without
  destroying that session (checkpoint‑leave‑running, or fork from an idle‑suspended
  session's existing snapshot).
- A caller can **fork N sessions** from a base snapshot in one operation; each is an
  independent session (own id, own worker, own subsequent checkpoints, own lifecycle),
  all starting from the identical restored state.
- **Snapshot pinning:** a base snapshot referenced by ≥1 fork (or explicitly pinned)
  is never GC‑reaped or overwritten while referenced.
- Forks are **isolated**: one fork's checkpoints/suspend/reset never affect the base
  or sibling forks.
- Fits the existing teleport/suspend/GC machinery — a fork is "just a session" after
  birth.

### Non‑goals
- **Not** copy‑on‑write S3 storage. v1 each fork that checkpoints writes its own full
  snapshot lineage (they share only the read‑only base at birth). CoW/dedup is a later
  optimization (§9).
- **Not** live process forking (à la `fork()`); this is checkpoint/restore‑based —
  copies start from a point‑in‑time image, not a shared running process.
- **Not** cross‑`runsc`‑version forking — same constraint as teleport (a snapshot
  restores only on a compatible `runsc`; `main.go:389` already version‑guards).
- **Not** the RL training loop / scheduler itself — this provides the *substrate*
  (branch + isolate + reclaim), not the policy that decides what to run.

## 5. Proposed design

### 5.1 Base snapshot: an explicit, pinned artifact

Introduce the notion of a **base snapshot** — a checkpoint intended to be forked from,
decoupled from any single session's mutable `snapshotURI` lineage.

- **Create it** two ways: (a) `checkpoint --leaveRunning` on a live session (worker
  already supports this) and record the returned prefix as a base; or (b) promote an
  idle‑suspended session's existing `snapshotURI` to a base.
- **Represent it.** Option A (leaning): a new `SnapshotFork`‑style CR — or a light
  `BaseSnapshot` CR — recording `{snapshotURI, image, runscVersion, ports, health,
  iamRoleArn, createdAt, pinned}`. It carries the restore identity (image + runsc + spec)
  so a fork needs no back‑reference to the origin session. Option B: no new CR — a fork
  request just carries a snapshot URI + restore spec inline. Option A gives GC a
  first‑class "this snapshot is pinned" object to honor (see 5.4) and a natural place
  for ref‑counting; recommend A.
- **Copy the snapshot to a fork‑stable prefix** (e.g. `bases/<baseId>/…`) so it lives
  outside the per‑session `sandboxes/<sid>/…` space the orphan‑S3 GC pass sweeps —
  belt‑and‑suspenders against accidental reap, and it makes the base's lifetime
  independent of the origin session. (Alternatively keep it under `sandboxes/` and rely
  purely on pinning; a separate prefix is cleaner.)

### 5.2 Fork operation

A control‑plane operation `Fork(baseId, count, [namePrefix]) → [sessionIds]`:

1. Resolve the base snapshot (URI + restore spec).
2. For each of `count`: mint a **new Session** (new id, e.g. `sess-fork-<base>-<n>` or
   caller‑named) with `status.snapshotURI = base.snapshotURI`, `image`, `ports`,
   `health`, `iamRoleArn` copied from the base, phase `Suspended` (so the *existing*
   resume path restores it on first contact) — or eagerly resume it.
3. Each fork then flows through the **unchanged** resume machinery: claim an idle
   worker, `/restore` from the base snapshot, reach Running. From that instant it is a
   normal session — its own checkpoints write its own lineage; it never touches the
   base.
4. Increment the base's **fork ref‑count** (5.4).

Exposure: since the broker is the session front door and holds identity, the natural
surface is a **broker tool / endpoint** (e.g. an MCP tool `fork_session` or a broker
`POST /fork`), authz‑gated (§6). An operator‑side API is the internal primitive; the
broker calls it. For non‑interactive RL harnesses, a direct control‑plane API (or
`kubectl apply` of N Session CRs referencing the base) is also viable.

### 5.3 Isolation

- Each fork has a distinct sandbox id → distinct worker binding, distinct interior
  netns/IP (rebuilt on restore), distinct future `sandboxes/<forkId>/…` snapshot
  lineage. No shared mutable state.
- Suspend/reset/checkpoint on a fork use its own sid — the CAS single‑writer model and
  the due‑indexes already isolate per‑session state. Nothing special needed.

### 5.4 Interaction with GC (the one hard requirement)

A base snapshot must survive as long as forks may restore from it. Two mechanisms,
use both:

- **Pin the base.** The `BaseSnapshot` CR (5.1) carries `pinned: true` (or a
  ref‑count > 0). Teach the GC orphan‑S3 pass to treat pinned base prefixes as
  never‑orphaned — it already has a "referenced by a session ⇒ keep" notion
  (`gc.go` `accounted`); extend that to "referenced by a live BaseSnapshot / positive
  ref‑count ⇒ keep." Because bases live under `bases/` (5.1), the simplest form is:
  the orphan‑S3 pass only sweeps `sandboxes/`, never `bases/`, and a **separate base
  reaper** deletes a `bases/<id>` only when its CR is deleted / ref‑count hits 0 and a
  grace elapses.
- **Ref‑count forks.** Increment on fork‑create, decrement when a fork is reset/deleted
  (reaped). A base with count 0 past a TTL (and not explicitly pinned) becomes eligible
  for reclaim. This composes with the existing session‑GC classes — a fork is reaped
  by the normal abandoned/TTL passes, and its teardown decrements the base.
- **Never overwrite a base.** Because a base lives at its own immutable prefix and no
  session treats it as its mutable `snapshotURI`, the "checkpoint overwrites snapshot"
  path (`main.go:349`) can't clobber it. Forks write elsewhere by construction.

### 5.5 Lifecycle summary

```
session ──checkpoint(leaveRunning)──► base snapshot (pinned, bases/<id>/…)
                                         │
                 Fork(base, N) ──────────┼──► sess-fork-…-1  ─ /restore from base ─► Running ─► own lineage
                                         ├──► sess-fork-…-2  ─ /restore from base ─► Running ─► own lineage
                                         └──► …N              (each independent from birth)
base reclaimed when: CR deleted OR (ref-count 0 AND past TTL AND not pinned)
```

## 6. Authorization

*Who* may fork, *how many* forks (fan‑out cap), and *from which* bases is a
subject→entitlement decision — the **same gate** deferred for BYOC, per‑session IAM,
and delegated access. Forking amplifies resource use (N workers per call), so a
per‑subject fan‑out quota is the key new control. This PRD assumes that gate; it adds
one parameter to it (max forks / concurrent forks per subject).

## 7. Risks / considerations

- **Restore determinism across copies (validate live).** N sandboxes restored from one
  image must not collide on anything a workload assumes is unique. The sandbox boundary
  (own netns, own IP) covers network identity; but a workload image that baked in, at
  checkpoint time, a value expected to be globally unique (a license nonce, a
  registered client id, an open outbound connection with server‑side state) will have N
  copies sharing it. This is inherent to snapshot‑forking, not a sandboxd bug —
  **document it**, and test with the target RL workload. gVisor restore of TCP
  connections in particular: a checkpoint with live external connections restored N
  times means N clients on one server‑side socket — fine if the base is checkpointed at
  a quiescent point (recommend: quiesce/close external I/O before taking a base).
- **Fan‑out cost / capacity.** K forks = K warm workers + K snapshot downloads (each
  the full image). Modest K is fine; large K is a pool‑scaling + scheduling question
  (and a thundering‑herd on S3 for the base — mitigate with the shared‑prefix read,
  possibly a cache, later). The fan‑out quota (§6) bounds it.
- **Base storage cost.** A pinned base persists indefinitely; plus each fork that
  checkpoints writes its own full lineage (no CoW in v1). Bound with base TTL +
  ref‑count reclaim (5.4) and the per‑fork session GC.
- **runsc version pinning.** A base is restorable only on a compatible `runsc`; forks
  must land on workers matching the base's `runscVersion` (the worker already 409s a
  mismatch — the fork scheduler should prefer/require matching‑image pools, same as
  teleport).

## 8. Testing / acceptance

1. **Fork correctness:** checkpoint a session at a marked state; fork N=3; each fork
   restores to Running on its own worker and observably starts from the marked state
   (e.g. a file/marker written before the base checkpoint is present in all three).
2. **Divergence/isolation:** mutate each fork differently; each fork's subsequent
   checkpoint writes its own lineage; the base snapshot is byte‑unchanged; siblings
   unaffected.
3. **Pinning vs GC:** with GC armed, a base with live forks is never reaped; after all
   forks are deleted and TTL elapses (and unpinned), the base is reclaimed.
4. **Fan‑out cap:** a subject over its fork quota is refused (once the authz gate
   exists).
5. **RL‑shaped smoke:** drive a workload to a common state, fork K, run K distinct
   action sequences, collect K outcomes — the end‑to‑end parallel‑rollout path.

Acceptance: from one checkpoint, N independent sessions start from the identical state,
diverge without interference, and the base is safely retained then reclaimed.

## 9. Later / out of scope (noted)

- **Copy‑on‑write / dedup S3 storage** so forks share unchanged pages instead of each
  writing a full lineage (big cost win at large K; needs a CoW‑aware snapshot format).
- **Snapshot‑read cache on workers** to avoid K full downloads of a hot base.
- **Nested forking / fork trees** (fork a fork) — falls out naturally if bases are
  first‑class, but the ref‑count/reclaim graph gets more involved.
- **Deterministic replay** guarantees for RL (seed capture, I/O virtualization) — a
  workload‑level concern beyond the substrate.

## 10. Effort estimate

Medium. No worker/runtime change (the restore primitive is done). Work is: a
`BaseSnapshot` representation + create/promote path; the `Fork` operation (mint N
Sessions seeded with the base snapshot — reusing the resume path); GC pinning +
base‑reaper + ref‑count; a broker/operator surface to invoke it; the fan‑out quota on
the shared authz gate. Pairs naturally with the session‑GC work already shipped.

## 11. Open questions

| # | Question | Leaning |
|---|----------|---------|
| Q1 | New `BaseSnapshot` CR, or inline snapshot URI on the fork request? | New CR — gives GC a first‑class pin/ref‑count object and a stable base identity. |
| Q2 | Base under a separate `bases/` prefix, or stay in `sandboxes/` + pin? | Separate `bases/` prefix — keeps it out of the orphan‑S3 sweep entirely; pin as defense in depth. |
| Q3 | Forks born `Suspended` (lazy restore on first contact) or eagerly resumed? | Born Suspended by default (reuses the resume path, no new code, pay restore on first use); offer eager resume for RL harnesses that want all K hot immediately. |
| Q4 | Fork surface: broker MCP tool, broker REST, operator API, or `kubectl apply` of Session CRs? | Broker tool/endpoint for interactive; direct control‑plane API (or declarative Session CRs referencing a base) for RL harnesses. Both call one operator primitive. |
| Q5 | Reclaim a base: explicit unpin/delete only, or ref‑count + TTL? | Both — ref‑count + TTL for automatic reclaim, explicit `pinned` to keep indefinitely. |
| Q6 | CoW storage in v1? | No — full lineage per fork in v1; CoW is a later optimization (§9). |

## 12. Status

**Proposed — nothing built.** The runtime primitive (`/restore` from an arbitrary
snapshot into a new sandbox) already exists and is exercised by teleport; this PRD is
the control‑plane feature (base snapshot + fork + pinning/GC) on top of it. See
[[sandboxd-pending-work]] (memory) for the cross‑session tracker.
