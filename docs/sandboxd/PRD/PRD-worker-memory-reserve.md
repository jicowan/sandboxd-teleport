# PRD — worker-side memory reserve (OOM-isolate the sandboxd agent)

Status: **Proposed / ready to implement.** Written 2026-07-26. Grounded in a read of the
live worker (`checkpoint-restore/sandboxd/{main,bundle,spec}.go`, `runsc.go`,
`network.go`) — the mechanism below cites what exists today.

Related: [PRD-multi-sandbox-per-worker.md](./PRD-multi-sandbox-per-worker.md) (this
supersedes its "Phase 0"; see §7), [architecture-sandboxd.md](../architecture-sandboxd.md)
(worker-vs-sandbox model).

## 1. Problem

A sandboxd **worker pod** runs three things inside one Kubernetes pod cgroup:

1. the **sandboxd Go agent** (the HTTP control server: `/run`, `/restore`, `/suspend`, …),
2. **runsc** — the gVisor **sentry** + **gofer** processes, and
3. the **sandbox** itself (the guest workload).

In gVisor the guest's RAM is not a separate VM allocation — it lives **inside the sentry
(`runsc`) process**. So "the sandbox using memory" shows up on the host as the sentry's
RSS growing.

Today the worker sets **no per-sandbox memory limit** (`ociSpec` in `spec.go` emits
`linux.namespaces` but no `linux.resources`). A runaway guest therefore grows the sentry
until the **pod** cgroup hits its limit, at which point the **kernel OOM killer** chooses
a victim **anywhere in the pod** — and it can pick the **sandboxd agent** (or the gofer)
rather than the offending sandbox. Killing the agent takes down the whole worker and every
future control operation on it; the blast radius of one greedy guest is the entire worker.

## 2. Goal / non-goals

**Goal:** make a memory-hungry sandbox trip **its own** cgroup's OOM kill *before* it can
push the pod cgroup to the edge — so the kernel kills inside the sandbox's subtree and the
**sandboxd agent survives**. Pure robustness/hardening.

**Non-goals:**

- **Not** a user-facing resource feature. **Zero** CRD / API / KV / wire surface. No
  `AppTemplate.spec.resources`, no `SessionEntry.Resources`, no `RunRequest` change. (That
  was the dropped "Phase 0"; see §7 for why it added nothing at 1:1.)
- **Not** density / multi-sandbox-per-worker (Phases 1–3 of the other PRD — shelved).
- **Not** CPU. CPU starvation only makes the agent *slow* and is self-correcting; OOM
  *kills* it. Memory only. (A future `cpu.weight` favoring the agent is possible if
  starvation is ever observed — out of scope here.)

## 3. Why this is clean (mechanism)

runsc **already** runs each sandbox's sentry+gofer in its **own per-container cgroup v2
subtree** — `runsc.go`'s teardown does a per-container `cgroup.kill` on exactly that
subtree. We are **not** creating cgroups; we only add a **limit** to the one runsc already
makes, via the standard OCI field `linux.resources.memory.limit`. runsc honors standard
OCI `LinuxResources`. So a runaway guest hits `memory.max` on the sentry cgroup → the
OOM killer fires **inside that subtree** → the agent, in a sibling part of the pod cgroup,
is spared.

## 4. Design

Entirely **worker-internal**. The worker derives the limit from **its own pod cgroup** at
bundle-build time — nothing upstream (operator/router/CRD) is involved.

1. **Read the pod's memory limit.** cgroup v2: `/sys/fs/cgroup/memory.max` — the
   **worker POD's own** cgroup limit. Value is either an integer (bytes) or the literal
   `max` (no limit). **Anchor on the pod cgroup, NEVER the node** (`/proc/meminfo`
   `MemTotal` / node allocatable): the pod limit is set by
   `SandboxTemplate.spec.resources` and is **constant across the pool** regardless of
   which instance type Karpenter provisioned or how many worker pods share the node (see
   §5.1). Node memory varies by instance type and is carved up by density, so reserving
   against it would over-estimate available memory and risk node-level OOM/eviction — the
   exact whole-pod kill we're preventing.
   - If `max` (the SandboxTemplate sets no memory **limit**) → **skip** (emit no
     `linux.resources` → today's exact behavior) **and log a WARN recommending a memory
     limit on the SandboxTemplate to enable agent OOM-protection.** Under Karpenter a
     limit-less pod can burst into shared node memory that density makes unpredictable, so
     this is the config where the protection matters most — but we will not guess against
     node memory; the feature engages only when given a pod limit to anchor on.
   - If unreadable (e.g. cgroup v1, see §5) → skip silently (today's behavior).
2. **Compute the sandbox limit:**
   `reserve = max(reserveFloor, reservePct × podLimit)`;
   `sandboxLimit = podLimit − reserve`.
   Defaults: `reserveFloor = 256Mi`, `reservePct = 12%`. (Percent scales the headroom on
   large workers; the floor protects small ones.)
3. **Floor guard.** If `sandboxLimit <= 0` or below a small sanity minimum (e.g. the
   reserve would consume most of the pod), **skip + log a WARN** rather than emit an
   absurd limit. A too-small pod just runs uncapped (as today).
4. **Write it into the OCI spec.** In `spec.go`'s `ociSpec`, add
   `linux.resources.memory.limit = sandboxLimit` (the spec is a `map[string]any`, so this
   is a couple of lines — no new module import). Applies on **`/run`**.
5. **Restore path.** `/restore` today reuses the **saved `config.json`** from the
   checkpoint verbatim (`moveConfigJSON`, `main.go`), so the limit is **carried in the
   snapshot automatically** — no extra code needed. A session only ever teleports within
   **its own pool**, and every worker in a pool is stamped from the same
   `SandboxTemplate.spec.resources` → same pod size, so the baked-in limit always matches
   the restoring worker's pod limit. Nothing to recompute on restore.

**Config (env vars, matching worker convention — the worker reads `SANDBOXD_*` via
`os.Getenv`, it has no flags):**

- `SANDBOXD_AGENT_MEMORY_RESERVE` — reserve floor, e.g. `256Mi` / bytes. Default `256Mi`.
- `SANDBOXD_AGENT_MEMORY_RESERVE_PCT` — integer percent, default `12`.
- Setting the floor to `0` **and** pct to `0` disables the feature (explicit opt-out;
  equivalent to today).

**Touch list (small, localized):**

- `checkpoint-restore/sandboxd/spec.go` — `ociSpec` gains an optional `memLimitBytes`
  arg (0 = omit) → writes `linux.resources.memory.limit`.
- `checkpoint-restore/sandboxd/bundle.go` — `writeOCISpec` reads pod limit + computes the
  reserve, passes `memLimitBytes` to `ociSpec`. (A small helper `podMemoryLimit()` +
  `sandboxMemLimit(podLimit)` — unit-testable pure function.)
- `checkpoint-restore/sandboxd/main.go` — read the two env vars once at startup into the
  server config; no behavior change if unset (defaults apply).
- `checkpoint-restore/sandboxd/{reaper.go,runsc.go}` — **OOM visibility (in scope).** When
  a sandbox exits, read its cgroup's `memory.events` `oom_kill` counter (the per-container
  cgroup runsc already manages, which `runsc.go` `cgroup.kill`s on teardown) and log it, so
  a sandbox killed by *its own* limit is diagnosable as an OOM rather than a mysterious
  workload exit. This is what makes the §8 success criterion observable; without it the
  feature's effect is invisible.
- Tests: table-test `sandboxMemLimit` (max→0, small pod→0 with WARN, normal→pod−reserve,
  pct vs floor crossover) and a `config.json` assertion that `linux.resources.memory.limit`
  is present/absent as expected.

## 5. Backward compatibility

- Pods with **no** memory limit (`memory.max == max`) → no `linux.resources` emitted →
  **byte-for-byte** today's spec (plus the WARN from §4 step 1). This is the default for
  any worker whose `SandboxTemplate.spec.resources` sets no memory limit.
- Existing checkpoints (saved `config.json` with no `linux.resources`) restore exactly as
  before.
- cgroup v1 nodes: `/sys/fs/cgroup/memory.max` won't exist → read fails → skip. (We target
  cgroup v2, which runsc already assumes for its per-container `cgroup.kill`. Worth a note,
  not a blocker.)

### 5.1 Karpenter / heterogeneous instances

sandboxd worker nodes are commonly provisioned by **Karpenter**, whose NodePools can span
many instance types (a `min` CPU/RAM requirement, not a fixed shape) and pack a variable
number of worker pods per node by size. This design is **correct by construction** under
that model, precisely because it anchors on the **pod** cgroup, not the node:

- **Per-pod limit is pool-constant, instance-independent.** The worker pod's
  `memory.max` comes from `SandboxTemplate.spec.resources` (the WarmPool stamps every pod
  from the same spec). It is the same value whether Karpenter placed the pod on a small or
  a large instance — so `sandboxLimit = podLimit − reserve` is identical for every worker
  in the pool, and identical across a session's whole same-pool teleport lifetime. Karpenter
  instance choice does **not** perturb the number.
- **Density-independent.** However many worker pods Karpenter bin-packs onto one large
  instance, each pod's *own* cgroup limit is unchanged. Because we reserve against the pod
  (not node allocatable), co-tenant density on the node cannot make our reserve wrong.
- **This is exactly why we must not read node memory.** Node `MemTotal`/allocatable varies
  by instance type *and* is divided among co-resident pods; a reserve computed from it would
  over-estimate and could drive **node-level** OOM/eviction (which kills the entire worker
  pod — agent included — the opposite of the goal). Pod-cgroup anchoring sidesteps this.
- **Recommendation for operators:** set a memory **limit** (not just a request) on
  `SandboxTemplate.spec.resources`, especially with Karpenter NodePools defined only by a
  `min` size. Without a limit the pod is `Burstable`/`BestEffort` and can grow into shared
  node memory that density makes unpredictable — and the agent-OOM protection stays off
  (the §4-step-1 WARN fires). A limit both binds the pod and enables this feature.

## 6. Caveats / open considerations

1. **Reserve tuning.** `256Mi` / `12%` are starting points; the agent+gofer footprint
   under load should be measured on the live fleet and the defaults revised. Over-reserving
   wastes guest memory; under-reserving remits the original risk. Configurable by design.
2. **Guest-visible memory.** Setting `memory.limit` may change what the guest *sees* as
   available RAM (runsc can report the cgroup limit to the guest). Confirm the guest sees a
   sane total; ensure we don't accidentally shrink the guest's view below the workload's
   needs when a pod limit is generous but pct-reserve is large.

## 7. Relationship to the dropped "per-sandbox resources" idea

The earlier plan ("Phase 0" of [PRD-multi-sandbox-per-worker.md](./PRD-multi-sandbox-per-worker.md))
was to expose per-sandbox CPU/mem as a **user-facing** `resources` field
(`AppTemplate.spec.resources` → `RunRequest` → OCI), threaded through the KV
`SessionEntry` so teleport replays it. **Dropped**, because at strict **1:1
sandbox-per-worker**:

- the **worker pod already has a cgroup limit** (its k8s pod spec, from
  `SandboxTemplate.spec.resources`) and the sandbox is the pod's **only** tenant — so a
  per-sandbox cap ≤ pod size is redundant and > pod size is impossible. **The worker's
  resources are the limit.**
- there is **no resource-aware scheduler** — the router is a dumb 1:1 session→worker proxy
  and placement is the **pool hint**, not bin-packing — so nothing would even *read* a
  per-sandbox resource value.

A user-facing per-sandbox `resources` knob only earns its complexity once 1:1 is broken
(multiple sandboxes sharing one pod cgroup budget = Phases 1–3, **shelved**). This PRD
keeps the *one* benefit that exists even at 1:1 — **OOM-isolating the agent** — and does
it with **no API surface**, purely inside the worker.

## 8. Success criteria

- A guest that mallocs past its computed limit is OOM-killed **in its own cgroup**; the
  sandboxd agent stays up and the worker keeps serving other control ops. (Demonstrate on
  the live fleet with a memory-bomb workload; observe the sandbox die and `/sandboxes`
  still respond.)
- Workers with no pod memory limit are byte-for-byte unchanged (spec diff = empty).
- Restore of a pre-existing checkpoint is unchanged.
