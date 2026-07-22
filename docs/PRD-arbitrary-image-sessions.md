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

> **§13 (as‑designed, 2026‑07‑22)** develops **generic‑by‑default pools** + a new
> scheduling‑free **`AppTemplate`** CRD: a pool's `SandboxTemplate` carries only
> worker‑shape (scheduling/resources) and its `image` is optional — empty ⇒ a generic
> pool that runs any app a session brings via `Session.spec.appRef`; set ⇒ a
> dedicated single‑image pool (today's behavior). Includes the `ForkSet.appRef`
> implication and why **routing needs no change** (the router is session‑keyed and
> image‑blind). This supersedes §5.3's sketch and the interim Stage 1 `templateRef`.

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

## 13. Design addition — generic pools + the AppTemplate CRD

This section supersedes §5.3's sketch with the **as‑designed** model (settled
2026‑07‑22). It splits the two things today's `SandboxTemplate` conflates —
**worker‑shape** (scheduling/resources) and **workload** (image/ports/health/…) —
into two CRDs, makes **generic pools the preferred default**, and keeps a
**dedicated single‑image pool** as the exception. It also resolves the routing
question that motivated the design ("how would the router know where to send a
request for an arbitrary image?"): it doesn't need to — the router is session‑keyed
and image‑blind (§13.5). We drop the interim `Session.spec.templateRef` from the
earlier Stage 1 in favor of `appRef → AppTemplate`.

### 13.1 The load‑bearing split: worker‑shape vs. workload

A `SandboxTemplate` today carries fields consumed at two different times:

| Bundle | Fields | Consumed | Belongs to |
|--------|--------|----------|------------|
| **Worker‑shape** | `scheduling` (nodeSelector/tolerations/affinity/topologySpread), `resources`, `workerImage`, `streamConsole` | **pool‑creation** — baked into the worker pod spec (`warmpool_controller.go`) | the **pool** |
| **Workload / app** | `image`, `cmd`, `env`, `ports`, `health`, `idle`, `checkpointIntervalSeconds`, `iam` | **per‑session** — passed to `/run`, travels with the session | the **app** |

This split is **structural, not stylistic**: the resume/session path never reads
`scheduling`/`resources`, and it *can't* — a worker is a real pod placed by
kube‑scheduler, and the sandbox runs nested inside whatever worker it lands on. A
session cannot retroactively change the placement of its assigned pod. So placement
is inherently a pool property and the workload is inherently a session property.

**Design rule (user, 2026‑07‑22): the app must NOT be able to specify scheduling.**
We make that a *type* guarantee, not a runtime check, by giving the app its own CRD
with no scheduling fields.

### 13.2 Two CRDs

- **`SandboxTemplate`** — the **pool's** blueprint: worker‑shape (scheduling,
  resources, workerImage, streamConsole). Its `image` becomes **optional**:
  - **image set** → a **dedicated pool** that runs *only* that one image (exactly
    today's `aio`/`redis`/`memcached` pools — unchanged).
  - **image empty** → a **generic pool**: worker‑shape only; it runs whatever app a
    session brings.
- **`AppTemplate`** (NEW) — the **workload**: `image` (required), `cmd`, `env`,
  `ports`, `health`, `idle`, `checkpointIntervalSeconds`, `iam`. **No scheduling,
  no resources, no workerImage** — by construction an app cannot express placement.
  Reusable: one `AppTemplate` runs on any generic pool (GPU pool, AZ‑pinned pool,
  standard pool) with zero duplication.

```
WarmPool ──templateRef──▶ SandboxTemplate     (worker-shape: scheduling, resources,
   (capacity + placement)                       workerImage; GPU/AZ spread live HERE)
      │
      │  image set  → DEDICATED pool (runs only that image; poolRef-only sessions)
      │  image empty → GENERIC  pool (runs any app a session brings)
      ▼
Session ──appRef──▶ AppTemplate               (workload: image, cmd, env, ports, health,
   (runs on the pool's workers)                idle, checkpoint, iam — NO scheduling)
```

### 13.3 Generic‑first; dedicated is the exception

Operational guidance (user, 2026‑07‑22): **reach for a generic pool first.** Stand
up a **dedicated** single‑image pool only when one image earns its own warm fleet —
a heavy/slow‑booting image whose cold‑pull you want to amortize, or one that needs a
distinct capacity class. What makes a pool "dedicated" is simply **its
`SandboxTemplate` pinning an image**; that single‑image‑ness is *itself* the cache /
warm‑hit isolation (a dedicated pool has nothing to run a foreign app *as*, so it
refuses them for free — no opt‑in/opt‑out flag needed).

Session → pool matrix:

| Session | Generic pool (template image empty) | Dedicated pool (template image set) |
|---------|-------------------------------------|--------------------------------------|
| `poolRef` only | **error** — nothing to run (no app, no pinned image) | runs the pinned image (**today's behavior, unchanged**) |
| `poolRef` + `appRef` | runs the AppTemplate's workload on the pool's workers | **rejected** — a dedicated pool runs only its image |
| `poolRef` + `image` (inline) | admin/`kubectl` escape hatch (ungoverned, §12); front door does not expose it | rejected |

Capacity is **always** `poolRef`. The workload comes from (precedence)
**`image` (escape hatch) > `appRef` > the pool's own pinned image**.

### 13.4 Why this needs almost no new machinery

A sandboxd worker is image‑agnostic by construction — the agent + pinned `runsc`,
baking in **no** workload image, pulling whatever `/run`/`/restore` names from the
node containerd cache. `ClaimIdleWorker(pool, sid)` picks *any* idle worker in a
pool; it never consults the image. So "run any app on this pool" is a **control‑plane
resolution change**, not a worker/scheduler change. Teleport, suspend, restore, GC,
and reclaim are already image‑agnostic and session‑keyed — untouched.

The Stage 1 work already built and live‑proved the config‑decoupling machinery: the
resume `planFor`, the suspend `policyFor`, and the checkpoint `policyFor` all resolve
workload config through one shared precedence helper. Stage 2 only re‑points that
helper at `AppTemplate` (via `appRef`) instead of the interim `templateRef`.

### 13.5 Routing needs **no change** — the router is image‑blind

The router routes by **session**, never by image or pool. Per request
(`internal/router/router.go`): resolve `Identity` from headers (`X‑Session‑ID` +
subject + pool hint — the image is *not* in the request) → `GetSession(sid)` → if
`Running` on a live worker, proxy to its IP; on miss/stale → `Resume(...)`, the
operator writes the worker IP into `session:<sid>`, proxy. It only ever answers
*"which worker holds session Y?"*. A session is bound to exactly one worker; the
router follows that binding. There is no "find a worker with image X" step.

Resume‑from‑snapshot — the common case — is the easy case: `/restore` reads the image
straight from the session record (`cur.Image`), not from any template, so a suspended
session restores onto any idle worker in its pool. The image travels *with the
session* (KV entry, mirrored to `Session.status`). Cold‑pull is a one‑time first‑use
cost; every later resume replays the cached image.

### 13.6 ForkSet implication — add an `appRef` source

`ForkSet` sources today:

- **Snapshot** (`spec.baseRef → BaseSnapshot`): already generic‑ready — a
  `BaseSnapshot` carries its **own** `status.image`, children `/restore` from S3
  regardless of pool. No change; the *ideal* large‑fan‑out path (promote once, fan
  out from S3 — no per‑child pull).
- **Image** (`baseRef` unset): children are `poolRef`‑backed and cold‑start from the
  **pool's** template image — undefined on a generic pool (no pinned image).

So add **`ForkSetSpec.appRef → AppTemplate`**: the image‑source branch of
`createForkSessions` stamps each child Session with `appRef` alongside `poolRef`,
resolving exactly as §13.3. Sources are mutually exclusive: `baseRef` XOR `appRef`
(XOR nothing = cold‑start a *dedicated* pool's image, today's behavior); `pool`
always required.

**Fan‑out amplifies the cold‑pull tradeoff** (N× on a never‑seen image). Mitigations,
all optional: prefer the **snapshot source** for large fan‑outs; optional pre‑puller;
later, image‑affinity‑aware claiming (out of MVP scope — claim image‑blind, accept
first‑use pulls).

### 13.7 Governance shrinks to a template ACL

Because a generic pool runs **admin‑authored `AppTemplate`s** (not caller‑supplied
image strings), the heavy governance surface of §5.1–5.5 (registry allow‑list,
cosign/signature verification, image admission) mostly **evaporates** for the
front‑door path: the admin already vetted each AppTemplate's image/ports/iam. What
remains is a small ACL — *"which subjects may reference which AppTemplates"* — a name
allowlist, not a supply‑chain policy engine. Raw `spec.image` (§12) stays as an
admin/`kubectl` escape hatch only; the front door exposes `appRef`, not `image`, so
the §5.2 registry/signature machinery is needed **only** if/when raw arbitrary images
are ever exposed through the front door (a separate, later decision).

### 13.8 Regression safety

Almost entirely additive; the only edit to an existing type is making
`SandboxTemplate.image` **optional** (backward‑compatible — every existing template
sets it, so dedicated pools are byte‑identical). `AppTemplate` + `Session.appRef` +
`ForkSetSpec.appRef` are new/optional. Existing pools, `poolRef`‑only sessions, and
snapshot/image ForkSets are unchanged. The interim Stage 1 `Session.spec.templateRef`
is dropped (it was new, optional, and never used in production).

### 13.9 Staged rollout (regression‑first; each stage live‑tested)

- **Stage 1 (done, being superseded):** `Session.spec.templateRef → SandboxTemplate`
  + shared config‑resolution precedence across resume/suspend/checkpoint. Live‑proved
  the decoupling machinery. Its field is renamed/retyped to `appRef` in Stage 2a.
- **Stage 2a:** add the **`AppTemplate` CRD**; replace `Session.spec.templateRef` with
  `appRef → AppTemplate`; point the three resolvers at it; make
  `SandboxTemplate.image` optional. *Live test: an `appRef` session (e.g. redis) runs
  on the aio‑pool with the AppTemplate's image/ports/idle; classic aio `poolRef`
  session unchanged; suspend + teleport intact.*
- **Stage 2b (done, live‑verified — operator v36):** stand up a true **generic pool**
  (image‑less `SandboxTemplate` with its own scheduling) + admission enforced at the
  operator's authoritative `planFor` chokepoint (`resolveWorkloadSource`): `appRef`
  requires a generic pool; `poolRef`‑only on a generic pool is rejected ("nothing to
  run"); `appRef`/inline‑image on a dedicated pool is rejected. Verified live: the
  accept/reject matrix (4 cases) behaved exactly, rejects returned the admission
  reason (e.g. *"pool X is generic … needs an appRef"*), an `appRef=redis` session ran
  on a generic‑pool worker, and the curated aio‑pool was unaffected.
  - **Gotcha (worker‑shape):** a generic pool's `SandboxTemplate` still needs a
    resolvable **worker image** — either `spec.workerImage` on the template or the
    operator's global `--worker-image`/`SANDBOXD_WORKER_IMAGE`. Only the *workload*
    image is optional on a generic template; the *worker* image is worker‑shape and
    must exist, else the WarmPool reconcile refuses to render the Deployment (logs
    "no worker image configured" and requeues). The reference fleet sets `workerImage`
    per template (no global default), so a generic template must set it too.
- **Stage 3:** `ForkSetSpec.appRef` — fan an AppTemplate onto a generic pool;
  per‑fork routing verified.

### 13.10 Summary of the delta

| Change | Where | Kind |
|--------|-------|------|
| `SandboxTemplate.image` optional (empty ⇒ generic pool; set ⇒ dedicated single‑image) | `SandboxTemplateSpec` | backward‑compatible relaxation |
| **`AppTemplate` CRD** (workload only; no scheduling — type‑level guarantee) | new `api/v1alpha1/apptemplate_types.go` | additive CRD |
| `Session.spec.appRef → AppTemplate` (replaces interim `templateRef`) | `SessionSpec` + shared resolver precedence | additive field + retarget resolver |
| Admission: `appRef` ⇒ generic pool; `poolRef`‑only on generic ⇒ error; `appRef` on dedicated ⇒ reject | controller (+ optional CEL) | new admission rules |
| `ForkSetSpec.appRef` | `ForkSetSpec` + `createForkSessions` | additive field + one branch |
| Routing / worker / teleport / suspend / restore / GC | — | **no change** |

Net: generic‑by‑default pools + a scheduling‑free `AppTemplate`, achieved with one
new CRD, one new Session field, one relaxed field, and admission rules — no router,
worker, or teleport change. The workload/placement boundary is enforced by the type
system, not by runtime checks.
