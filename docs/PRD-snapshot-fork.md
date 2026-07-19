# PRD — ForkSet (fan out N sessions from a common source: snapshot or image)

Status: **Proposed** (analysis + plan; grounded in the shipped code on the
`checkpoint-restore` branch). Decision‑ready.

Related: [architecture-sandboxd.md](sandboxd/architecture-sandboxd.md),
[PRD-session-garbage-collection.md](PRD-session-garbage-collection.md),
[PRD-durable-assignment-state.md](PRD-durable-assignment-state.md),
[admin-guide-crds.md](sandboxd/admin-guide-crds.md).

## 1. Summary

Let a caller fan out **N independent sessions from one common source** in a single
declarative operation (a `ForkSet` CR). The source is one of two things:

- **A snapshot** (fork‑from‑snapshot): take one session to a desired "golden" state,
  checkpoint it, then spawn N sessions that all start from that **identical RAM+FS
  state** and diverge. Amortizes an expensive setup — you paid once to reach the state,
  and every fork replays it for free.
- **An image** (fork‑from‑image): spawn N sessions that each **cold‑start from a pool's
  template image**. No shared frozen moment — each does its own boot, so the N trials
  are *independently* initialized.

Both mint N first‑class `Session` children (own id, own worker, own future checkpoint
lineage, own lifecycle) through the **same** fan‑out parent and the **same** resume
machinery; they differ only in whether each child is born with a `snapshotURI` (→
`/restore`) or without (→ `/run`). `baseRef` on the `ForkSet` is what selects snapshot
vs image, and it is **optional** — omit it for image fan‑out.

The motivating use case is **reinforcement learning / parallel rollouts**: drive an
environment (an agent, a browser, a tool sandbox, any workload) to a common starting
state once, then fan out K copies to explore K different action sequences / scenarios,
comparing outcomes. It generalizes to any "branch from a common start" pattern:
parallel test scenarios, tree‑search/beam rollouts, what‑if/A‑B exploration, or a
reproducible fixture many jobs start from.

**The core mechanism already exists — for both sources.** sandboxd's `/restore` reads
a snapshot from an arbitrary S3 prefix into a *new* sandbox id and never consumes or
mutates the source (that's the snapshot fork); `/run` cold‑starts from an image (that's
the image fork). The resume workflow already chooses `/restore` vs `/run` by whether a
`snapshotURI` exists. What's missing is control‑plane orchestration: a way to mint N
sessions from one source, plus — for the snapshot case only — snapshot **pinning** so
GC doesn't reclaim a base while forks still need it.

### 1.1 When to use which source

| | **Fork‑from‑snapshot** (`baseRef` set) | **Fork‑from‑image** (`baseRef` omitted) |
|---|---|---|
| Use when | The common state is **expensive to reach** (warm‑up, loaded model, primed cache, navigated UI) or the branch point **is a specific reached state** ("agent is 20 steps in, try 8 next actions"). | The common start is just "**freshly booted image**" — no meaningful warm‑up — or you *want* N **independently** initialized trials. |
| Start latency | One restore (seconds) regardless of setup cost. | N cold starts (e.g. a browser‑class image is ~40–45s each). |
| Initial state | **Bit‑identical** across forks (the point — and the source of the determinism caveat, §7). | **Independent** per fork (own boot, seeds, timestamps, connections) — no shared‑state caveat. |
| Extra machinery | BaseSnapshot + copy‑on‑promote + pinning + GC exemption (§5.1/§5.4). | **None** — cold start is the shipped P1 path; no base, no pinning. |

Fork‑from‑image is the cheaper, simpler configuration (available essentially for free);
fork‑from‑snapshot adds the base‑management apparatus to buy state amortization and
reproducibility. They are complementary, not competing — the `ForkSet` expresses both.

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
- **Cold start from an image is the shipped P1 path (fork‑from‑image needs nothing
  new).** `/run` (`handleRun`) starts a fresh sandbox from a template image, and the
  resume workflow already branches `/run` vs `/restore` on whether a `snapshotURI`
  exists (cold‑start branch vs `resume.go:267`). So an *image* fork is even simpler than
  a snapshot fork — N children with **no** `snapshotURI`, each cold‑starting from the
  pool's template image. None of the base/pinning/GC apparatus applies.

So the runtime needs **no change** for either source. This is a control‑plane feature.

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
  must be **exempt** from both, or a fork's restore races a delete — solved in §5.1/§5.4
  by copy‑on‑promote to a `bases/` prefix the orphan‑S3 pass never sweeps, plus a
  conservative CR‑gated base reaper.

## 4. Goals / non‑goals

### Goals
- A caller can **fan out N sessions from one source** in one **declarative** operation
  (a `ForkSet` CR), where the source is **either** a snapshot (`baseRef` set) **or** a
  pool's image (`baseRef` omitted). Each child is an independent session (own id, own
  worker, own subsequent checkpoints, own lifecycle).
- For the snapshot source: a caller can **create a base snapshot** from a session at a
  chosen state without destroying it (checkpoint‑leave‑running, or promote an
  idle‑suspended session's existing snapshot), and all forks start from that identical
  restored state.
- For the image source: N forks **cold‑start independently** from the pool's template
  image (no base, no shared frozen state) — the cheap, no‑extra‑machinery path.
- Each fork carries an explicit **lifecycle policy** — **ephemeral** (`reset`‑on‑idle:
  a finished/abandoned rollout is torn down leaving no snapshot behind — the RL default)
  or **durable** (`suspend`‑on‑idle: keep the fork's state, optionally re‑promote it to
  a new base) — chosen per ForkSet, not hard‑wired.
- **Base protection (snapshot source):** a base referenced by in‑flight forks (or
  explicitly pinned) is never GC‑reaped or overwritten; protection is *structural* (the
  base lives as its own artifact), with ref‑count/TTL only governing eventual reclaim.
- Forks are **isolated**: one fork's checkpoints/suspend/reset never affect the base
  or sibling forks.
- Fits the existing teleport/suspend/GC machinery — a fork is "just a session" after
  birth, and routing/quotas/GC treat it as one.

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

This adds **one new fan‑out CRD (`ForkSet`)** plus a **`BaseSnapshot`** CR (only needed
for the snapshot source), and **two small additive fields** on the existing
`Session`/`SessionLifecycle` — the forked sessions themselves ride the unchanged
Session machinery. Fork‑from‑image uses `ForkSet` alone (no `BaseSnapshot`).

### 5.1 `BaseSnapshot`: an explicit, protected artifact (copy‑on‑promote) — snapshot source only

A **base snapshot** is a checkpoint intended to be forked from, decoupled from any
single session's mutable `snapshotURI` lineage. Represent it as a `BaseSnapshot` CR
(short name `basesnap`) recording the full restore identity so a fork needs no
back‑reference to the origin session:

```yaml
apiVersion: core.sandboxd.io/v1alpha1
kind: BaseSnapshot
metadata: { name: golden-cartpole-v3, namespace: default }
spec:
  image: ghcr.io/agent-infra/sandbox:latest   # restore identity (image + runsc + spec)
  runscVersion: release-20260622.0
  ports:  [{ container: 8080, host: 8080 }]
  health: { probe: http, probePort: 8080, probePath: /v1/health }
  iamRoleArn: ""                               # optional default for forks
  pinned: true                                 # explicit keep — never auto-reclaimed while true
status:
  snapshotURI: bases/golden-cartpole-v3/snap-…  # fork-stable prefix (see below)
  refCount: 3                                   # forks not-yet-materialized + explicit holds
  ready: true                                   # copy-on-promote complete
```

**Create it** two ways, both via the worker's *existing* checkpoint primitive:
(a) `checkpoint --leaveRunning` on a live session (`main.go:294`, already supported) at
the chosen state, or (b) promote an idle‑suspended session's existing `snapshotURI`.

**Copy‑on‑promote — the primary protection (structural, not bookkeeping).** Promoting
does an **S3 server‑side copy** of the source checkpoint to a **fork‑stable prefix
`bases/<baseId>/…`**, distinct from the per‑session `sandboxes/<sid>/…` space. This is
deliberate and load‑bearing:

- The base's survival is **no longer hostage to the origin session.** If the origin
  session re‑checkpoints (overwriting *its* `sandboxes/<originSid>/` lineage), is reset,
  or is GC‑reaped, the base under `bases/` is untouched.
- The **orphan‑S3 GC pass only ever sweeps `sandboxes/`** — it never walks `bases/`. So
  a base cannot be reaped as an "orphan" by construction, independent of any ref‑count
  correctness. Reclaim of a base is a *separate, conservative* path (5.4).
- Checkpoint always mints a **fresh** `snap-<unixnano>` and never writes into an
  existing prefix (`main.go:340`), so "overwrite the base" is not a possible failure
  mode — the only threat to a base is *deletion*, which `bases/`‑exclusion + 5.4 handle.

### 5.2 `ForkSet`: the fan‑out CR (what the user passes)

A fork is inherently **one → N**, so it gets its own declarative parent — a `ForkSet`
CR is to forked `Session`s what `WarmPool` is to worker pods: a controller creates and
owns N `Session` children (ownerRefs) and reports their readiness in `.status`. This is
also the single seam where the **fan‑out quota** (§6) is enforced, rather than trusting
a client to apply N Sessions itself.

**The source is `baseRef` (optional):**
- **`baseRef` set → fork‑from‑snapshot.** Children are seeded with the base's
  `snapshotURI` + restore identity and `/restore` from it.
- **`baseRef` omitted → fork‑from‑image.** Children carry no `snapshotURI` and
  cold‑start (`/run`) from the pool's template image. No `BaseSnapshot`, no pinning.

```yaml
apiVersion: core.sandboxd.io/v1alpha1
kind: ForkSet
metadata: { name: rollout-batch-7, namespace: default }
spec:
  baseRef: { name: golden-cartpole-v3 }   # OPTIONAL: set → snapshot source; omit → image source (cold start from pool)
  count: 16                               # N forks
  namePrefix: rollout-7                    # children sess-fork-rollout-7-{0..15}; else generated suffix
  pool: aio-pool                           # placement + (image source) the template image; must be runsc-compatible with a base
  activation: Eager                        # Eager = resume/run all N now (RL default) | Lazy = born Suspended/Absent, materialize on first contact
  lifecycle:                               # per-fork lifecycle policy (see 5.3)
    idleAction: reset                      #   reset = ephemeral rollout | suspend = durable branch | none
    idleTimeoutSeconds: 300
    ttlAfterSuspendSeconds: 3600
  subject: alice                           # owner → authz + fan-out quota key
  iam: { roleArn: "" }                     # optional; overrides the base's / template's iam
status:
  desired: 16
  ready: 12
  forks: [sess-fork-rollout-7-0, sess-fork-rollout-7-1, …]  # created session ids (the harness reads these back)
  phase: Progressing                        # Progressing | Ready | Failed
  conditions: [...]
```

| Field | Meaning |
|-------|---------|
| `baseRef` | **Optional.** Set → snapshot source (the `BaseSnapshot` to fork from — resolves snapshot URI + restore identity; gives pinning / ref‑count / quota a handle). Omit → image source (each child cold‑starts from `pool`'s template image). |
| `count` | N forks to create. |
| `namePrefix` | deterministic child naming so a harness can address `rollout-7-<k>`; optional. |
| `pool` | pool that places the forks; for the image source it also supplies the template image. Must be `runsc`‑compatible with the base (snapshot source). |
| `activation` | `Eager` (materialize all N now — RL default) vs `Lazy` (born Suspended (snapshot) / Absent (image), materialize on first contact). |
| `lifecycle.idleAction` | **ephemeral (`reset`) vs durable (`suspend`)** — the key policy knob (5.3). |
| `lifecycle.idleTimeoutSeconds` / `ttlAfterSuspendSeconds` | per‑fork idle + retention. |
| `subject` | owner for the fan‑out quota + attribution. |

**Validation:** `baseRef` present ⟹ the referenced `BaseSnapshot` must exist and be
`ready`; the `pool`'s `runscVersion` must match the base's. `baseRef` absent ⟹ `pool`
is required (it supplies the image). A CEL admission rule can enforce both.

**Controller flow (per fork child):** mint a new `Session` (id `sess-fork-<prefix>-<n>`)
with `spec.forkFrom = {baseRef, snapshotURI}` (5.5, empty for the image source) and the
resolved `pool`/`iam`, then:
- **Snapshot source** (`baseRef` set): seed `status.snapshotURI = base.status.snapshotURI`
  + restore identity, phase `Suspended`. `Eager` resumes it now (→ `/restore` from the
  base); `Lazy` leaves it `Suspended` for the existing resume path to restore on first
  contact.
- **Image source** (`baseRef` omitted): no `snapshotURI`, phase `Absent`. `Eager` warms
  it now (→ `/run` cold start from the pool image); `Lazy` leaves it `Absent` for the
  normal cold‑start‑on‑first‑contact path.

Either way, once a fork reaches Running it is a normal session — its own checkpoints
write its own `sandboxes/<forkId>/` lineage; the snapshot‑source fork never touches the
base again (5.4). Both sources use the **already‑existing** `/run`‑vs‑`/restore` branch
in the resume workflow — the controller just sets the child's initial state so the
right branch fires.

**Exposure.** The CR is the substrate. For interactive callers the broker offers sugar
— an MCP tool `fork_session` / `POST /fork` that creates a `ForkSet` and returns
`.status.forks` — authz‑gated at the identity seam (§6). RL harnesses skip the broker
and `kubectl apply` a `ForkSet` directly, then watch `.status.ready == desired`.

### 5.3 Fork lifecycle policy: ephemeral vs durable (do forks checkpoint?)

Forks checkpoint through the **unchanged per‑session machinery**, isolated by sid for
free: the checkpoint S3 prefix is `sandboxes/<sandboxID>/snap-<ns>` (`main.go:341`) and
`sandboxID` *is* the fork's own session id, so a fork's suspend / periodic‑checkpoint /
checkpoint‑on‑terminate writes its **own** lineage under `sandboxes/<forkId>/` — it can
never touch the base or a sibling. No new mechanism.

The *policy* question — whether a fork should checkpoint at all — is surfaced as
`lifecycle.idleAction`, chosen per batch:

- **Ephemeral (`idleAction: reset` — the RL default).** A rollout runs a scenario; you
  read the outcome and discard it. `reset`‑on‑idle (or explicit delete) tears the fork
  down **without leaving a snapshot behind**, so K rollouts don't litter S3 with K
  abandoned lineages. This is why forks need a per‑session `idleAction` override (5.5) —
  today the idle *action* lives only on the `SandboxTemplate`, so without the override
  an ephemeral batch would need its own pool.
- **Durable (`idleAction: suspend`).** A fork that reaches an interesting state is
  suspended (checkpoint → its own `sandboxes/<forkId>/` snapshot) and can teleport‑resume
  later — and that snapshot can itself be **promoted to a new `BaseSnapshot`**, so
  nested forking / fork trees fall out naturally (a base is just a pinned snapshot).

Isolation otherwise needs nothing special: each fork has a distinct sandbox id →
distinct worker binding, distinct interior netns/IP (rebuilt on restore), distinct
snapshot lineage; the CAS single‑writer model + due‑indexes already isolate per‑session
state.

### 5.4 Protecting the base while forks are in use (the crux)

Three layered claims, from strongest to softest:

**(1) Structural: the base is a standalone artifact under `bases/`, which GC never
sweeps.** By 5.1 the orphan‑S3 pass only walks `sandboxes/`, so a base cannot be reaped
as an orphan regardless of ref‑count bookkeeping. This is the primary guarantee and it
does not depend on counting anything correctly.

**(2) CR‑existence is the retention guarantee; ref‑count only *offers* reclaim.** A base
is retained **as long as its `BaseSnapshot` CR exists.** Deliberately, a ref‑count is
**never** allowed to delete a base out from under a live fork: a missed increment or a
double‑decrement must be *safe*, so the reclaim path errs toward over‑retention. A base
becomes *eligible* for reclaim only when **all** hold — `spec.pinned == false` **and**
`status.refCount == 0` **and** a grace has elapsed — and even then a dedicated base
reaper (not the orphan‑S3 pass) deletes `bases/<id>` + the CR. Losing track just keeps a
base longer than necessary; it never frees one early.

**(3) The dependency window is short: a fork needs the base only until its first
restore.** A fork reads the base **exactly once** — its initial `/restore`. Once it
reaches Running, its state is in RAM + local rootfs, and every *future* resume/teleport
uses its **own** `sandboxes/<forkId>/` lineage, never the base. So `refCount` really
counts **forks that have not yet completed their first restore, plus explicit holds
(`pinned`)** — not "live forks." Consequences:

- The correctness‑critical window is just *fork‑create → each fork's first successful
  restore* — short and easy to hold the base across.
- A base whose forks are all already Running (and not `pinned`) is immediately safe to
  reclaim — nothing will read it again unless you fork *more* from it (which is exactly
  what `pinned: true` expresses: "keep this base forkable indefinitely").

Implementation: `ForkSet` reconcile increments `refCount` per child at create and
decrements when a child records its first `Running` transition (first restore done); an
explicit `pinned` hold is a separate, caller‑controlled retention that survives
refCount 0. (Image‑source ForkSets have no `baseRef`, so there's no base and no
ref‑counting at all — this whole section applies only to the snapshot source.) The
existing session‑GC classes reap the fork *sessions* normally; base reclaim is the
separate conservative path above.

### 5.5 CRD changes summary

Two **new** CRs and **two small additive fields** on existing types — the forked
sessions ride the unchanged Session machinery:

- **`ForkSet`** (new, §5.2) — the one→N fan‑out parent; owns N `Session` children.
  Source is `baseRef` (optional): set → snapshot, omitted → image. This is the only CR
  the **image** source needs.
- **`BaseSnapshot`** (new, §5.1) — a pinned, forkable checkpoint artifact. **Snapshot
  source only** (not created for image fan‑out).
- **`Session.spec.forkFrom { baseRef, snapshotURI }`** (additive) — provenance on each
  fork child: makes a child self‑describing (a rebuild knows its base) and is the
  ref‑count decrement key for base reclaim (§5.4). Set by the fork controller for the
  snapshot source; empty for image forks and normal sessions.
- **`SessionLifecycle.idleAction`** (additive) — a per‑session idle‑action override
  (`suspend`|`reset`|`none`). Today the idle *action* lives only on
  `SandboxTemplate.spec.idle.action`; `SessionLifecycle` has only `idleTimeoutSeconds` +
  `ttlAfterSuspendSeconds`. This override lets an **ephemeral fork choose `reset` without
  a dedicated pool** (§5.3), and is independently useful beyond forking.

No worker/runtime change; no change to the router (§5.7).

### 5.6 Lifecycle summary

Snapshot source (`baseRef` set):
```
session ──checkpoint(leaveRunning)──► BaseSnapshot (copy-on-promote → bases/<id>/…, pinned)
                                         │
        ForkSet{baseRef,count:N} ────────┼──► sess-fork-…-0  ─ /restore from base ─► Running ─► own sandboxes/<forkId>/ lineage
                                         ├──► sess-fork-…-1  ─ /restore from base ─► Running ─► own lineage
                                         └──► …N-1            (each independent after first restore)
base reclaimed when: CR deleted OR (pinned==false AND refCount==0 AND past grace)
  where refCount = forks-not-yet-restored + explicit pins  (a fork needs the base only for its FIRST restore)
```

Image source (`baseRef` omitted):
```
        ForkSet{pool,count:N} ───────────┬──► sess-fork-…-0  ─ /run (cold start from pool image) ─► Running ─► own lineage
                                         ├──► sess-fork-…-1  ─ /run ─► Running ─► own lineage
                                         └──► …N-1            (each INDEPENDENTLY booted — no shared state, no base)
no base, no pinning, no ref-count; forks are reaped by the normal session GC.
```

### 5.7 Routing to forks (no router change)

Forks route **exactly like any other session** — the router needs no change, because a
fork *is* a session with its own id. Recall the data path: the front door addresses a
session by `X-Session-ID`; the router resolves that id → the bound worker via the KV
table and stream‑proxies to it (`router.go` fast path: `GetSession` → `workerLive`
fence → proxy; miss → `/resume`). Each fork has a **distinct session id** and, once
restored, a **distinct worker binding** in KV, so:

- **Addressing a specific fork** is just sending its `X-Session-ID` (e.g.
  `sess-fork-rollout-7-3`). The router looks it up and proxies to *that* fork's worker —
  the same resolution used for any session. The `.status.forks` list on the `ForkSet`
  is how a caller learns the N ids to address. This is identical for both sources
  (snapshot and image) — routing doesn't care how a fork was materialized.
- **No fan‑out/broadcast in the router.** The router stays a 1:1 session→worker proxy;
  it does not multiplex one request to N forks. Driving N forks in parallel is the
  *client/harness's* job — it holds the N session ids and opens N logical streams (N
  `X-Session-ID`s), which the router independently resolves. This keeps the data plane
  the same O(1)‑per‑request proxy it is today.
- **Teleport/liveness still apply per fork.** If a fork was `Lazy` or idle‑suspended, its
  first request resolves through the normal path — cold‑start (`/run`, image source) or
  restore (`/restore` from the base for the first materialization, then its own lineage
  thereafter) — transparent to the caller, identical to any session's
  cold‑start/restore‑on‑connect.
- **The broker seam.** Interactive callers reach sessions via the broker, which derives
  a durable per‑principal id — that path assumes one session per user and isn't the
  fork addressing surface. Fork batches are addressed by their explicit session ids
  (RL harness → router directly, or a broker extension that can target a chosen fork id
  rather than the principal‑derived one). Either way the **router is unchanged**; only
  *which* `X-Session-ID` is presented differs.

## 6. Authorization

*Who* may fork, *how many* forks (fan‑out cap), and *from which* bases is a
subject→entitlement decision — the **same gate** deferred for BYOC, per‑session IAM,
and delegated access. Forking amplifies resource use (N workers per call), so a
per‑subject fan‑out quota is the key new control. This PRD assumes that gate; it adds
one parameter to it (max forks / concurrent forks per subject).

## 7. Risks / considerations

- **Restore determinism across copies — SNAPSHOT SOURCE ONLY (validate live).** N
  sandboxes restored from one snapshot must not collide on anything a workload assumes
  is unique. The sandbox boundary (own netns, own IP) covers network identity; but a
  workload that baked in, at checkpoint time, a value expected to be globally unique (a
  license nonce, a registered client id, an open outbound connection with server‑side
  state) will have N copies sharing it. This is inherent to snapshot‑forking, not a
  sandboxd bug — **document it**, and test with the target workload. gVisor restore of
  TCP connections in particular: a checkpoint with live external connections restored N
  times means N clients on one server‑side socket — fine if the base is checkpointed at
  a quiescent point (recommend: quiesce/close external I/O before taking a base).
  **The image source is exempt** — each fork boots independently, so there is no shared
  frozen value; choosing the image source is itself the mitigation when independence
  matters more than amortization (§1.1).
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

Grouped by concern; each case names the scenario discussed. Unit/envtest where the
logic is pure (controller reconcile, GC, admission); live (EKS) for the restore/boot
determinism and RL end‑to‑end. "unit" = miniredis/envtest, no gVisor.

### 8.1 Snapshot source — correctness & isolation
1. **Fork correctness (snapshot):** checkpoint a session at a marked state (write a
   sentinel file + in‑RAM marker before `checkpoint --leaveRunning`); create a `ForkSet`
   with `baseRef` + `count: 3`; assert all 3 children reach `Running` on **distinct
   workers** and each observably starts from the marked state (sentinel present, RAM
   marker intact). *live*
2. **Divergence / isolation:** mutate each of the 3 forks differently; assert each
   fork's subsequent checkpoint writes its **own** `sandboxes/<forkId>/` lineage, the
   base prefix under `bases/` is **byte‑unchanged**, and no sibling sees another's
   mutation. *live*
3. **Base immutability under re‑checkpoint of the origin:** after promoting a base,
   re‑checkpoint (or reset) the **origin** session; assert the base under `bases/` is
   untouched and forks still restore from it. (Copy‑on‑promote decoupling, §5.1.) *live*

### 8.2 Image source — correctness & independence
4. **Fork correctness (image):** create a `ForkSet` with **no** `baseRef`, `pool` set,
   `count: 3`; assert all 3 children cold‑start (`/run`) from the pool image, reach
   `Running` on distinct workers, and **no `BaseSnapshot`** is created / no `bases/`
   object written. *live*
5. **Independent initialization:** assert the 3 image forks are **not** bit‑identical —
   e.g. distinct per‑boot values (own random seed / generated id / start timestamp) —
   confirming image forks sidestep the shared‑state determinism caveat (§7). *live*
6. **No base machinery engaged (image):** unit — reconcile an image‑source `ForkSet` and
   assert `spec.forkFrom` is empty on children, no ref‑count is taken, and children are
   born `Absent` (not `Suspended`). *unit*

### 8.3 Source selection & validation
7. **`baseRef` optional discriminator:** unit — `baseRef` set ⟹ children seeded
   `Suspended` with `forkFrom.snapshotURI` = base; `baseRef` omitted ⟹ children `Absent`,
   no snapshotURI. *unit*
8. **Admission validation:** `baseRef` referencing a missing/not‑`ready` `BaseSnapshot`
   is rejected; `baseRef` omitted **and** `pool` omitted is rejected; a `pool` whose
   `runscVersion` ≠ the base's is rejected (CEL rule, §5.2). *unit/envtest*

### 8.4 Activation
9. **Eager:** `activation: Eager` materializes all N immediately (Running before any
   client request) — both sources. *live*
10. **Lazy:** `activation: Lazy` leaves children `Suspended` (snapshot) / `Absent`
    (image); the first request to a child materializes it via the normal resume path.
    *live*

### 8.5 Fork lifecycle policy (do forks checkpoint?)
11. **Ephemeral (`idleAction: reset`):** an idle/finished fork is torn down leaving **no**
    `sandboxes/<forkId>/` snapshot behind and its KV entry + CR reaped — K rollouts leave
    no litter. *live + unit (idleAction override honored over the template)*
12. **Durable (`idleAction: suspend`):** an idle fork is checkpointed to its own lineage
    and teleport‑resumes later; and that fork's snapshot can be **promoted to a new
    `BaseSnapshot`** and forked again (nested fork). *live*
13. **`SessionLifecycle.idleAction` override:** unit — a session with
    `lifecycle.idleAction: reset` under a template whose `idle.action: suspend` resets on
    idle (override wins, no dedicated pool needed).

### 8.6 Base protection & GC (snapshot source)
14. **Structural exemption:** with GC armed, the orphan‑S3 pass never lists/deletes under
    `bases/` (only `sandboxes/`); a base with **zero** refs but a live CR is retained.
    *unit (GC pass) + live*
15. **Dependency‑ends‑at‑first‑restore:** `refCount` counts only not‑yet‑restored forks +
    pins; after all forks reach `Running`, an **unpinned** base's refCount is 0 and it
    becomes reclaim‑eligible; a `pinned` base is retained regardless. *unit*
16. **CR‑is‑the‑guarantee (safety):** a spurious extra decrement (refCount would go
    negative / to 0 early) must **not** delete a base whose CR still exists and whose
    forks haven't restored — reclaim requires pinned==false AND refCount==0 AND grace,
    via the base reaper, never the orphan‑S3 pass. *unit*
17. **Reclaim path:** delete all forks + unpin + elapse grace ⟹ base reaper deletes
    `bases/<id>` and the `BaseSnapshot` CR; a still‑pinned base is never reclaimed. *unit
    + live*
18. **No‑race restore vs reclaim:** a fork mid‑first‑restore holds refCount > 0, so the
    base cannot be reclaimed out from under it. *unit*

### 8.7 Routing
19. **Per‑fork addressing (no router change):** requests carrying each child's
    `X-Session-ID` route to that child's worker; no broadcast; the router resolves each
    id independently — identical for snapshot and image forks. *live (drive 3 forks over
    3 concurrent streams, assert responses come from the right worker)*

### 8.8 Fan‑out quota (once the authz gate exists)
20. **Quota enforcement:** a `ForkSet` whose `count` (or the subject's concurrent forks)
    exceeds the per‑subject fan‑out cap is refused at admission; within cap succeeds.
    *envtest*

### 8.9 RL end‑to‑end
21. **Parallel‑rollout smoke (snapshot):** drive a workload to a common state, fork K,
    run K distinct action sequences, collect K outcomes — the amortized‑state path. *live*
22. **Parallel‑trial smoke (image):** fork K from an image, run K independent trials —
    the independent‑init path. *live*

### Acceptance
From one source, N independent sessions materialize (restore for snapshot, cold‑start
for image) on distinct workers, diverge without interference, route individually, and
are reaped by the normal session GC; for the snapshot source the base is safely retained
while any fork still needs it and reclaimed only when unpinned + unreferenced + past
grace; over‑quota fan‑outs are refused.

## 9. Later / out of scope (noted)

- **Copy‑on‑write / dedup S3 storage** so forks share unchanged pages instead of each
  writing a full lineage (big cost win at large K; needs a CoW‑aware snapshot format).
- **Snapshot‑read cache on workers** to avoid K full downloads of a hot base.
- **Nested forking / fork trees** (fork a fork) — falls out naturally if bases are
  first‑class, but the ref‑count/reclaim graph gets more involved.
- **Deterministic replay** guarantees for RL (seed capture, I/O virtualization) — a
  workload‑level concern beyond the substrate.

## 10. Effort estimate

Medium — and **fork‑from‑image is a small subset** (the `ForkSet` CR + reconcile alone,
no base machinery). No worker/runtime change (both `/run` and `/restore` are done) and
**no router change** (§5.7). Work is: the `ForkSet` CR + its reconcile (mint N Session
children — seeded from the base for the snapshot source, plain cold‑start for the image
source — reusing the resume path, roll up `.status`); the two additive Session fields
(`spec.forkFrom`, `SessionLifecycle.idleAction`); **for the snapshot source only** the
`BaseSnapshot` CR + create/promote path (incl. the S3 copy‑on‑promote to `bases/`) and
the base reaper + ref‑count in GC; a broker `fork_session` surface (sugar over
`ForkSet`); and the fan‑out quota on the shared authz gate. Pairs naturally with the
session‑GC work already shipped (the reaper + `bases/`‑exclusion extend the existing
Collector). A reasonable phasing: **image fork‑set first** (cheap, no base), then the
snapshot source (base + pinning + GC).

## 11. Open questions

| # | Question | Leaning |
|---|----------|---------|
| Q0 | Fork source: snapshot only, or also image? | **Both**, via optional `baseRef` on one `ForkSet` CR — set → snapshot (amortized/reproducible state), omit → image (cheap, independent per‑boot init). Complementary; see §1.1. |
| Q1 | New `BaseSnapshot` CR, or inline snapshot URI on the fork request? | New CR (snapshot source only) — gives GC a first‑class pin/ref‑count object and a stable base identity. |
| Q2 | Base under a separate `bases/` prefix, or stay in `sandboxes/` + pin? | Separate `bases/` prefix — keeps it out of the orphan‑S3 sweep entirely; pin as defense in depth. |
| Q3 | Forks born `Suspended`/`Absent` (lazy) or eagerly materialized? | `ForkSet.spec.activation`: `Lazy` (reuses the resume/cold‑start path, pay on first use) vs `Eager` (materialize all N now — the RL default). Caller chooses per batch. |
| Q4 | Fork surface: broker MCP tool, broker REST, operator API, or CRs? | The **`ForkSet` CR is the substrate** — RL harnesses `kubectl apply` it and watch `.status.forks`. A broker `fork_session` tool is sugar that creates a `ForkSet` for interactive callers. |
| Q7 | Should fork *addressing* go through the broker or direct to the router? | Direct by `X-Session-ID` for RL harnesses (broker's principal‑derived id assumes one session/user); router is unchanged either way (§5.7). A broker extension could target a chosen fork id if interactive fork‑driving is needed. |
| Q8 | `refCount` counts live forks, or only not‑yet‑restored forks + pins? | Only **not‑yet‑restored forks + explicit pins** (§5.4.3) — a fork needs the base solely for its first restore, so a base with all forks Running (unpinned) is immediately reclaimable. CR‑existence, not the count, is the retention guarantee. |
| Q5 | Reclaim a base: explicit unpin/delete only, or ref‑count + TTL? | Both — ref‑count + TTL for automatic reclaim, explicit `pinned` to keep indefinitely. |
| Q6 | CoW storage in v1? | No — full lineage per fork in v1; CoW is a later optimization (§9). |

## 12. Status

**Proposed — nothing built.** Both runtime primitives already exist — `/restore` from an
arbitrary snapshot (exercised by teleport) and `/run` cold‑start from an image (the P1
path); this PRD is the control‑plane fan‑out (`ForkSet`, + `BaseSnapshot`/pinning/GC for
the snapshot source) on top of them. See [[sandboxd-pending-work]] (memory) for the
cross‑session tracker.

> **Naming note:** the file is `PRD-snapshot-fork.md` for link stability, but the
> feature is **ForkSet** (fan out N from a snapshot *or* an image). `baseRef` optional
> is the source discriminator.
