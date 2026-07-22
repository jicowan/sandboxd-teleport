# PRD — arbitrary‑image sessions ("bring your own container")

Status: **Proposed** (not scheduled). This is a decision‑ready product spec; we
implement only if demand materializes. Author‑of‑record: platform team. Grounded
in the existing design (`checkpoint-restore/docs/PRD-control-plane.md` §4.4 and
the O6a/O6b/O6c decision items) and the shipped code on the `checkpoint-restore`
branch.

## 1. Summary

Let an authorized caller run a **container image of their own choosing** as a
gVisor sandbox session, instead of only the operator‑authored image bound to a
WarmPool's `SandboxTemplate`. The session teleports, idle‑suspends, and restores
exactly like a template session — the only differences are *where the image comes
from* (the caller) and *the governance* required to allow it (authorization,
registry policy, quotas).

The worker already supports this: it bakes in no workload image and runs whatever
image `/run` names. The gap is entirely in the **control plane / front door** —
an authorization gate, a registry/image policy, a self‑service creation path, and
a pool model that protects the warm hit‑rate of the curated pools. This PRD
specifies that gap.

> **§13 (design addition)** develops a first‑class **generic pool** — a pool of
> workers that runs *any* image/template — including decoupling a Session's config
> source (`templateRef`) from its capacity source (`poolRef`), the `ForkSet` image
> source, and why **routing needs no change** (the router is session‑keyed and
> image‑blind).

## 2. Background — what exists today

- **Workers are generic.** A sandboxd worker is the agent + pinned `runsc`; the
  workload image is passed per‑session to `/run` (or `/restore`). Nothing about a
  worker is image‑specific.
- **Two modes already exist in the data model.** `SessionSpec` has both
  `poolRef` (template mode) and inline `image`/`cmd`/`env`/`ports`
  (arbitrary‑image mode). The resume workflow (`resume_glue.go` → `planFor`)
  already resolves either: if `spec.image` is set it runs that image directly;
  otherwise it resolves the pool's template image. `WarmPool.spec.arbitraryImage`
  exists as a marker for a dedicated arbitrary‑image pool.
- **What's missing.** The **broker never sets `spec.image`** — it forwards only a
  pool hint — so through the front door every session is template mode. There is
  **no authorization gate** deciding who may bring an image, **no registry/signature
  policy**, and **no self‑service creation path**. Today the only way to run an
  arbitrary image is for an admin to create a `Session` object by hand with
  `kubectl`.

So this capability is ~80% built at the mechanism layer and 0% built at the
governance/product layer. This PRD is about the governance/product layer.

## 3. Goals / non‑goals

### Goals

1. An authorized user can start a session from an image they specify, through the
   normal front door (no `kubectl`).
2. Running an arbitrary image is **safe by construction**: gVisor is the isolation
   boundary (unchanged), and the platform enforces *who* may do it and *which
   images* are permitted.
3. Arbitrary‑image sessions get the full session experience: teleport,
   idle‑suspend/restore, per‑session lifecycle, console/logs.
4. Arbitrary‑image workloads **do not degrade** the curated pools' warm hit‑rate
   or node image cache.
5. Clear, auditable policy: every arbitrary‑image launch records who, which image,
   and whether it passed policy.

### Non‑goals

- **Not** weakening the sandbox boundary or granting more privilege. Arbitrary
  images run in the same gVisor sandbox as template images.
- **Not** general multi‑tenant namespace isolation (that's a separate item).
- **Not** a container build service — callers bring an already‑published image
  reference; we don't build images.
- **Not** changing the worker. No `sandboxd` worker changes are required.
- **Not** guaranteeing warm (sub‑second) starts for never‑seen images — first pull
  is inherently colder.

## 4. Users & use cases

| Persona | Use case | Why template mode doesn't serve it |
|---------|----------|-----------------------------------|
| Platform power user / developer | Run a custom tool image (their own agent, a language runtime, a CLI bundle) as a durable sandbox. | The curated pool only offers the AIO image. |
| Data/ML user | Run a specific framework image (e.g. a pinned CUDA/py image) for a task. | Needs a specific image the admin hasn't templated. |
| Internal team onboarding a new workload | Try their image as a teleporting sandbox before asking for a curated pool/template. | Faster iteration than requesting a new template each time. |
| Platform admin | Offer "bring your own container" as a controlled, audited entitlement to select groups. | Wants the capability gated, not open. |

## 5. Product requirements

### 5.1 Authorization (O6a) — who may bring an image

- Arbitrary‑image mode is an **entitlement**, off by default. A caller has it only
  if their identity carries it.
- Model it as a Keycloak group, consistent with the existing tiering
  (`sandbox-users` / `sandbox-power`). Proposed: a new group, e.g.
  **`sandbox-byoc`** ("bring your own container"), or fold it into `sandbox-power`.
  Decision deferred to implementation; the requirement is that it is a distinct,
  assignable entitlement.
- Enforcement is at the **front door** (agentgateway policy and/or broker), the
  same trust tier that already gates tool visibility. The control plane
  additionally refuses an arbitrary‑image Session that wasn't created by an
  entitled path (defense in depth — see 5.5).
- A caller **without** the entitlement gets template mode only, exactly as today,
  with no visible change.

### 5.2 Registry & image policy (O6b) — which images are permitted

Configurable policy, enforced **before** `/run`:

- **Registry allow‑list.** Only images from approved registries/paths (e.g.
  `ghcr.io/ourorg/*`, the internal ECR). Default: deny‑all except an explicit
  list.
- **Signature / provenance (optional, recommended).** Require a valid signature
  (cosign/policy‑controller style) or attestation for arbitrary images. Configurable
  per deployment; off by default in the MVP, on for stricter environments.
- **Tag/digest discipline.** Optionally require digests (no floating tags) so a
  restored session's recorded image is reproducible.
- **Denylist / size / platform checks** as needed (e.g. reject `:latest`, cap
  layers).
- Policy violations are rejected with a clear, user‑visible error ("image
  `X` not permitted: registry not allow‑listed") and audited.

#### Where enforced — layered, controller‑authoritative

The image to run is **`Session.spec.image`, a string field on the Session CRD** —
the workload never appears in a Kubernetes pod spec (it's handed to the worker's
`/run` and executed inside gVisor via runsc). That shapes the enforcement options:

- **Controller gate (mandatory, fail‑closed) — the authoritative control.** The
  operator must resolve the image before `/run`, so it is the unavoidable
  chokepoint. The registry allow‑list, require‑digest, and entitlement checks live
  here so they **cannot be bypassed** by a disabled/absent admission policy, and
  this is the natural home for **signature/provenance verification** (a cosign
  verification step in the controller), because that machinery is tied to *image
  content*, not to a CRD field. Always on.

- **Native CEL admission (optional, declarative overlay).** Because the target is a
  field on our own CRD, the checks that *are* declarative — registry allow‑list,
  require‑digest, `arbitraryImage` pool match — can be expressed in **in‑tree CEL**,
  with no third‑party policy engine to install or operate:
  - **CRD validation rules** (`x-kubernetes-validations` on the Session schema) —
    e.g. reject a `spec.image` that isn't digest‑pinned or whose registry prefix
    isn't allow‑listed, enforced by the API server on write. Ships with the CRD.
  - **`ValidatingAdmissionPolicy`** (GA in‑tree CEL admission) — when a rule needs
    to be cluster‑configurable or reference the target pool, a VAP + binding
    against `Session` writes keeps policy declarative and updatable without an
    operator rebuild, still with no external controller.

  CEL handles the *string‑shape* rules on `spec.image` (allow‑list / digest /
  pool‑match). It **cannot** verify signatures — that's image‑content work with no
  CEL primitive — so **signature verification stays in the controller**. This layer
  *supplements*, never *replaces*, the controller gate.

Recommendation: ship the controller gate as the source of truth (works with zero
extra infrastructure and can't be turned off), and optionally add **native CEL**
(CRD validation rules, escalating to a `ValidatingAdmissionPolicy` only if a rule
must be cluster‑tunable) for early, declarative rejection. Deliberately **not** a
third‑party policy engine (Gatekeeper/Kyverno): the checks are simple field
predicates on our own CRD, so in‑tree CEL covers them without the extra dependency.
Whichever front door created the Session, the controller gate still applies.

### 5.3 Pool model (O6c) — protect the curated pools

- Arbitrary‑image sessions run in a **dedicated pool** marked
  `WarmPool.spec.arbitraryImage: true`, fed by a **generic base image** with a
  **looser warm guarantee** (lower `minIdle`, or scale‑from‑zero acceptable).
- Rationale: a never‑seen image isn't in the node's containerd cache, so the first
  `/run` pays a pull cost and pollutes that node's cache. Isolating these to their
  own pool keeps the curated pool's (e.g. AIO) warm hit‑rate and cache clean.
- Curated pools **reject** arbitrary‑image sessions (an arbitrary‑image Session
  must target an `arbitraryImage: true` pool for its worker).
- Optional later mitigation: a per‑node image pre‑puller for frequently‑used
  arbitrary images.

> **Refinement — see §13.** "Fed by a generic base image" above is loose wording.
> A worker bakes in *no* workload image, so an `arbitraryImage` pool isn't tied to
> any base image — it's simply **generic capacity** that runs whatever image each
> session names. §13 develops this "generic pool" model (capacity decoupled from
> image), the `templateRef`‑vs‑`poolRef` split on a Session, and the ForkSet
> implication, and explains why **routing needs no change**.

### 5.4 Self‑service creation path (front door)

The interesting product work. Two candidate mechanisms — pick during design:

- **(a) Broker‑mediated (MCP connect‑time).** The broker learns the caller's image
  intent **at connect time** and creates the `Session` with `spec.image`
  (+ `cmd`/`env`/`ports`) targeting the arbitrary‑image pool, then proceeds as
  normal. Concretely the broker reads the intent from an `initialize` parameter or
  request header (the same seam it already uses to fire `/_warm` on `initialize`),
  or exposes a small dedicated broker endpoint the client hits *before* the MCP
  session. Keeps the single OAuth front door.

  > **Not a hub‑published MCP tool.** A tempting framing is "add a `create_session`
  > tool to the broker," but that is circular: MCP tools are published by the
  > sandbox *inside* an already‑running session, so you'd need a session before you
  > could call the tool that creates one. The image must therefore be declared
  > **before/at session establishment** — an `initialize` parameter/header the
  > broker intercepts, or a pre‑session broker endpoint — not a tool exposed on the
  > in‑session MCP surface. (A hub tool could only ever *re‑image* or spawn a
  > *second* session from within a first one — a possible future nicety, not the
  > primary create path.)

- **(b) A thin self‑service API/CLI.** A small authenticated endpoint ("create a
  BYOC session with image X") that creates the Session, separate from the MCP data
  path entirely. The user then connects their MCP client to the resulting session
  id.

Either way, the caller does **not** touch Kubernetes, and neither relies on an
in‑session MCP tool to bootstrap the session. The output is a durable session id
they connect to like any other. Recommendation: start with (a) if the primary
consumers are MCP clients; (b) if BYOC is more of an ops/CLI workflow.

### 5.5 Control‑plane admission (defense in depth)

Server‑side enforcement on Session admission, applying to **every** Session
however created (broker, self‑service API, or hand‑written `kubectl`) — this
closes the "admin bypass is the only path" gap that exists today. The rules:

  - **Exactly one** of `poolRef` / `image` is set (already a documented rule).
  - An `image` Session references an `arbitraryImage: true` pool for its worker.
  - `spec.image` passes registry/require‑digest policy (§5.2).
  - The creator was entitled (e.g. Session carries an attestation the front door
    stamped, or creation is restricted by RBAC to the front‑door identity).

These can be enforced two (composable) ways, per the layering in §5.2:

- **In the controller** (mandatory, fail‑closed) — the operator validates on
  reconcile/admission and refuses to `/run` a non‑compliant Session. Works with no
  extra infrastructure and can't be switched off. Signature verification lives here.
- **Via native CEL** (optional) — CRD `x-kubernetes-validations` rules on the
  Session schema (and, if a rule must be cluster‑tunable, a
  `ValidatingAdmissionPolicy`) that reject non‑compliant `spec.image`/`poolRef`
  combinations at API admission, before the CR persists. In‑tree, no third‑party
  engine. Subject to the CRD‑field caveat in §5.2 (allow‑list / require‑digest /
  pool‑match only; signature verification is not expressible in CEL).

The controller gate is the source of truth; native CEL admission is
defense‑in‑depth.

### 5.6 Quotas & lifecycle

- Per‑subject limits on concurrent arbitrary‑image sessions and pool share, so one
  caller can't exhaust the arbitrary‑image pool (a front‑door/control‑plane concern
  that applies to template sessions too, but more acute here).
- Arbitrary‑image sessions honor the same idle‑suspend, TTL‑after‑suspend, and GC
  rules. `SessionLifecycle` overrides apply.

### 5.7 Observability & audit

- Every arbitrary‑image launch is audited: subject, image reference (+ digest),
  policy decision, pool, session id, timestamp.
- Metrics: arbitrary‑image session count, cold‑pull latency distribution,
  policy‑rejection count.
- `kubectl get sessions` and status already surface the resolved image; keep that.

## 6. How it works (end‑to‑end, once built)

```
entitled user
  │  MCP initialize / BYOC request naming image ghcr.io/ourorg/tool@sha256:…
  ▼
front door (agentgateway + broker)
  │  1. verify identity + arbitrary-image entitlement (sandbox-byoc)
  │  2. (optional) pre-check registry/signature policy
  │  3. create Session{ image, cmd, env, ports, poolRef: <arbitrary-image pool> }
  ▼
operator admission
  │  enforce: one-of(poolRef,image); pool is arbitraryImage; registry/sig policy; entitled creator
  ▼
resume workflow (unchanged)
  │  claim idle worker from the arbitrary-image pool → /run with the caller's image
  ▼
sandboxd worker  →  nested gVisor sandbox running the caller's image
  │  checkpoint/restore records the image → teleport/suspend/restore work unchanged
  ▼
S3 snapshot (image replayed on restore)
```

Teleport, suspend, and restore need **no new code** — the assignment entry already
records the session's image and replays it on `/restore`.

## 7. Security considerations

- **Isolation is unchanged.** gVisor is the boundary; running untrusted code in it
  is the platform's purpose. Arbitrary images do not weaken it.
- **The new surface is governance, not isolation:** entitlement (who), registry/
  signature policy (what), quotas (how much). All enforced outside the worker.
- **Privileged worker note.** Workers are privileged (nested gVisor = DinD). The
  arbitrary *workload* still runs inside gVisor, not as a peer of the worker —
  bringing an image does not grant host/worker privilege. This must be verified in
  design (confirm the image can't escape the sandbox to the privileged worker
  context) and stated explicitly in the threat model.
- **Supply chain.** Registry allow‑list + optional signature verification is the
  primary control against malicious/hijacked images, enforced in the controller
  (authoritative) and optionally mirrored by a native CEL admission rule on
  `Session.spec.image` (§5.2/§5.5).
- **Pairs with P1.5.** The planned mTLS + NetworkPolicy hardening (router↔worker,
  operator) and the "router trusts `X-Session-ID`" gap should be closed before/with
  BYOC, since BYOC widens who runs code on the fleet.

## 8. Tradeoffs accepted

- **Colder first start.** A never‑seen image isn't cached; first `/run` pays a pull
  (measured reference: cold `golang:1.23-alpine` ~5.7s end‑to‑end — still fast, but
  not warm). Mitigated by the dedicated pool and optional pre‑puller.
- **Cache churn** on arbitrary‑image nodes — contained to that pool by design.
- **Operational policy surface** — someone must own the registry allow‑list and
  entitlement grants. This is the cost of the capability.

## 9. Open questions / decisions to finalize

| # | Question | Leaning |
|---|----------|---------|
| Q1 | Entitlement as a new group (`sandbox-byoc`) or fold into `sandbox-power`? | New group — decouples "run exec tools" from "bring any image." |
| Q2 | Self‑service path: broker‑mediated at connect time (a) or separate API/CLI (b)? Note: not an in‑session MCP tool (circular — see 5.4). | (a) broker‑mediated (`initialize` param/header or pre‑session endpoint) if consumers are MCP clients; (b) if ops‑driven. |
| Q3 | Signature verification in MVP or fast‑follow? | Allow‑list in MVP; signatures configurable, on for strict envs. |
| Q4 | Policy enforcement home: controller only, or also add native CEL admission? | Controller is mandatory + authoritative (fail‑closed, hosts signature verification). Optionally add **in‑tree CEL** — CRD `x-kubernetes-validations`, escalating to a `ValidatingAdmissionPolicy` only if a rule must be cluster‑tunable — for declarative allow‑list/digest/pool‑match. Deliberately **not** a third‑party policy engine (Gatekeeper/Kyverno): simple field predicates on our own CRD don't warrant the dependency. See §5.2/§5.5. |
| Q5 | Require digests (no floating tags)? | Recommended for reproducible restore; make it a policy toggle. |
| Q6 | How does the front door prove entitlement to the operator (stamp/attestation vs. RBAC‑restricted creation)? | RBAC‑restrict Session creation to the front‑door identity + carry subject; revisit. |

## 10. Rollout plan (if scheduled)

1. **M1 — dedicated pool + admission.** Ship the `arbitraryImage` pool wiring and
   the control‑plane admission rules (one‑of, pool‑match, registry allow‑list).
   Arbitrary images creatable by an entitled front‑door identity; still no
   self‑service UX. Internal/admin‑operated.
2. **M2 — front‑door entitlement + self‑service path.** Add the `sandbox-byoc`
   entitlement and the chosen creation mechanism (5.4). Now genuinely self‑service
   for entitled users.
3. **M3 — signatures, quotas, pre‑puller, audit dashboards.** Harden: signature
   policy, per‑subject quotas, optional per‑node pre‑puller, audit/metrics.

Each milestone is independently useful and shippable.

## 11. Success metrics

- Entitled users can launch a BYOC session with **zero `kubectl`**.
- **Zero** measurable regression in curated‑pool warm hit‑rate after enabling BYOC.
- **100%** of BYOC launches audited with subject + image + policy decision.
- Policy rejections are clear and self‑explanatory (low support burden).

## 12. Appendix — running an arbitrary image *today* (admin‑only, pre‑PRD)

Until this PRD is implemented, an admin can run an arbitrary image by creating the
Session directly (no authz/registry gate — admin‑operated only):

```yaml
apiVersion: core.sandboxd.io/v1alpha1
kind: Session
metadata:
  name: sess-alice-mytool        # matches the X-Session-ID routed to it
  namespace: default
spec:
  image: ghcr.io/alice/my-tool:1.4
  cmd: ["/bin/my-tool", "--serve"]
  ports: [{ container: 8080, host: 8080 }]
  poolRef: { name: aio-pool }    # borrows a worker; NOTE: pollutes this pool's cache
  subject: alice
```

Then route with `X-Session-ID: sess-alice-mytool`. This is exactly the
ungoverned path this PRD exists to replace with a safe, self‑service one.

## 13. Design addition — the generic pool model

This section develops §5.3's pool model into a first‑class **generic pool** (a pool
of workers that can run *any* image or template), and works through the two
consequences that fall out of it: decoupling a Session's **config source
(`templateRef`)** from its **capacity source (`poolRef`)**, and adding an image
source to **ForkSet**. It also resolves the routing question that motivated the
design ("how would the router know where to send a request for an arbitrary
image?").

### 13.1 Why a generic pool is mostly a *relaxation*, not new machinery

A sandboxd worker is image‑agnostic by construction: it is the agent + pinned
`runsc`, bakes in **no** workload image, and pulls whatever image `/run`/`/restore`
names from the node's containerd cache. `ClaimIdleWorker(pool, sid)` picks *any*
idle worker in a pool — it never consults the image. The pool→template binding is
therefore a pure **control‑plane convention**, not a worker or scheduling fact.

Today that convention is a 1:1 rule: `WarmPool.templateRef → SandboxTemplate →
image`, and a Session's `poolRef` transitively pins its image. A **generic pool**
simply relaxes that: the pool provides *capacity*, and each session names *what to
run*. Concretely:

- A generic pool is `WarmPool.spec.arbitraryImage: true` (the marker already exists,
  §5.3) with **no serving `templateRef`** (or a `templateRef` that provides only a
  worker‑sizing/scheduling default, not the workload image). `templateForPool`
  becomes **optional** for this pool flavor rather than required.
- Its workers are ordinary warm workers. They cold‑pull an un‑cached image on first
  use and warm‑hit thereafter (the node cache is per‑node, shared across that node's
  sessions).

Nothing about teleport, suspend, restore, GC, or reclaim changes — those are already
image‑agnostic and session‑keyed.

### 13.2 Routing needs **no change** — the router is image‑blind

The concern "how does the router route a request for an arbitrary image?" dissolves
once you trace what the router keys on. **The router routes by *session*, never by
image or pool.** Per request (`internal/router/router.go`):

1. Resolve `Identity` from headers — `X‑Session‑ID` (+ subject, + pool hint). The
   image is **not** in the request.
2. `GetSession(sid)` → the `session:<sid>` KV entry. If `Running` on a live worker,
   proxy to `e.WorkerPodIP:port`.
3. On miss/stale → `Resume(sid, subject, poolHint)`; the operator does the work,
   writes the worker IP into `session:<sid>`, and the router proxies to it.

The router only ever answers *"which worker holds session Y?"* — a session is bound
to exactly one worker (the one running its sandbox), and the router follows that
binding. There is no "find a worker that has image X" step to get wrong.

This is why **resume‑from‑snapshot — the common case — is the easy case.** The
restore path reads the image straight from the session record (`cur.Image` in
`resume.go`), **not** from a pool template, so a suspended session restores onto any
idle worker in its pool regardless of what template (if any) the pool has. The image
travels *with the session* (in the KV entry, mirrored to `Session.status` in etcd).
The cold‑pull cost (§8) is a one‑time, first‑use event on a never‑seen image; every
subsequent resume replays the cached image.

The **only** image‑aware step in the entire system is the operator's *cold‑start*
branch in `planFor` (`resume_glue.go`). That is the single place the generic‑pool
work touches (§13.3).

### 13.3 Decouple config source from capacity source: `templateRef` on the Session

`Session.spec.poolRef` today conflates two things a generic pool must separate:

- **Capacity** — which pool's workers to claim.
- **Config** — the workload image plus its `cmd`/`env`/`ports`/`health`/`idle`/`iam`
  defaults (today these come from the pool's `SandboxTemplate`).

For a generic pool, a session needs to name a template for *config* while claiming a
*generic* pool for *capacity*. Proposed additive field:

```
SessionSpec:
  poolRef:     *LocalRef   # capacity: which pool to claim a worker from
  templateRef: *LocalRef   # NEW: config source (image + cmd/env/ports/health/idle/iam)
  image:       string      # OR inline arbitrary image (existing, §2)
  # ... cmd/env/ports as today (inline overrides)
```

Resolution precedence in `planFor` / the resume cold‑start branch, in order:

1. **`spec.image`** set → run it directly (existing arbitrary‑image mode).
2. else **`spec.templateRef`** set → resolve *that* template for the image + config,
   independent of the pool. (New branch; mirrors the existing pool→template lookup
   but keyed off the session, not the pool.)
3. else **`spec.poolRef` → pool's `templateRef`** → today's behavior (curated pool
   whose template supplies the image). Unchanged.

Capacity always comes from `poolRef` (or the generic pool the front door assigns).
This is the smallest change that lets *many* curated templates run on *one* generic
pool without standing up a dedicated warm pool per template — the ergonomic win over
raw `spec.image` (you keep templated `cmd`/`env`/`ports`/`health`/`idle`/`iam`
without hand‑copying them into every Session).

Admission (extend §5.5): the one‑of rule becomes **at most one of
`{image, templateRef}`**, and **`poolRef` (capacity) is always required** — a
session with `templateRef` but no `poolRef` has no worker to run on. A `templateRef`
that names a *curated* pool's template is fine; the distinction is only that a
generic pool doesn't force a single one.

> **Compatibility.** `templateRef` is additive and optional. Existing curated
> Sessions (only `poolRef`) and existing arbitrary‑image Sessions (only `image`) are
> unchanged; the new branch is inert unless `templateRef` is set. No CRD‑breaking
> change.

### 13.4 ForkSet implication — add an image/template source

`ForkSet` has two sources today (`forkset_controller.go`):

- **Snapshot source** (`spec.baseRef` → a `BaseSnapshot`): children `/restore` from
  the base's S3 snapshot. This is **already generic‑pool‑ready** — a `BaseSnapshot`
  carries its **own** `status.image`, and children restore from S3 regardless of the
  pool's template. No change needed; arguably this is the *ideal* "fork any image"
  path (promote a golden checkpoint once, fan out from it).

- **Image source** (`spec.baseRef` unset): children are created as plain
  `poolRef`‑backed Sessions and cold‑start from **the pool's template image**. On a
  generic pool that has *no* single template image, this is under‑specified — there
  is nothing to inherit.

So an image‑source ForkSet on a generic pool needs to carry its own source. Proposed
additive fields on `ForkSetSpec`, matching §13.3's precedence exactly:

```
ForkSetSpec:
  pool:        string      # capacity (existing)
  baseRef:     *LocalRef   # snapshot source (existing) — carries its own image
  image:       string      # NEW: image-source fork of an arbitrary image
  templateRef: *LocalRef   # NEW: image-source fork of a named template's config
  # cmd/env/ports optional inline (with spec.image), as on a Session
```

The image‑source branch of `createForkSessions` then stamps each child Session with
`spec.image`/`spec.templateRef` (whichever the ForkSet set) alongside its
`poolRef: <pool>`, instead of relying on the pool→template lookup. Resolution in the
child is then exactly §13.3. Precedence and admission mirror the Session rules:
snapshot (`baseRef`) XOR image XOR template; `pool` always required.

**Fan‑out amplifies the cold‑pull tradeoff.** A `count: N` image‑source ForkSet of a
never‑seen image is up to **N simultaneous cold pulls** across workers (bounded by
distinct nodes; same‑node children share the cache after the first). This is the
§8 tradeoff at N×. Mitigations, all optional and layered — none are prerequisites:

- Prefer the **snapshot source** for large fan‑outs (pull/boot once, promote, then
  every child is an S3 restore of an already‑materialized image — no per‑child pull).
- The optional **pre‑puller** (§5.3, M3) warms the fork image on the pool's nodes
  before fan‑out.
- Later, **image‑affinity‑aware claiming** — prefer an idle worker whose node already
  has the image cached. This is a new scheduling dimension (the assignment table
  would track per‑worker/per‑node cached images); explicitly **out of scope** for the
  MVP, which claims image‑blind and accepts first‑use cold pulls.

### 13.5 What this adds to the rollout (§10)

The generic pool model slots into the existing milestones rather than adding a new
track:

- **M1** additionally: `arbitraryImage` pools may omit a serving `templateRef`
  (generic capacity); add the `Session.spec.templateRef` config‑source branch to
  `planFor`; extend admission to `at‑most‑one{image, templateRef}` + `poolRef`
  required.
- **M2** additionally: the front door may set `templateRef` (not just `image`) when
  assigning a session to a generic pool.
- **M3** additionally: `ForkSetSpec.image`/`templateRef`; document the fan‑out
  cold‑pull tradeoff and the snapshot‑source recommendation; optional pre‑puller and
  (later) image‑affinity claiming.

### 13.6 Summary of the delta

| Change | Where | Kind |
|--------|-------|------|
| Generic pool = capacity, no forced serving template | `WarmPool` `arbitraryImage`; `templateForPool` optional for it | control‑plane relaxation |
| `Session.spec.templateRef` (config source, decoupled from `poolRef`) | `SessionSpec` + `planFor` precedence | additive CRD field + one resolver branch |
| `ForkSetSpec.image` / `.templateRef` (image‑source fork of arbitrary image/template) | `ForkSetSpec` + `createForkSessions` | additive CRD fields + one branch |
| Routing | — | **no change** (router is session‑keyed, image‑blind) |
| Worker | — | **no change** (already image‑agnostic) |
| Teleport / suspend / restore / GC | — | **no change** (session‑keyed, image travels with the session) |

The net: a "run any image/template" pool is a **control‑plane relaxation plus two
additive CRD fields**, with the hard part being the *governance* layer this PRD
already specifies (§5.1–5.5), not the routing or the runtime.
