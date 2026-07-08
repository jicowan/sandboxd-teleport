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

Where enforced: the **control plane** (operator admission/validation of the
Session), so it holds regardless of which front door created the Session. The
front door may additionally pre‑check for a better UX.

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

- A validating webhook (or operator admission) enforces, server‑side:
  - **Exactly one** of `poolRef` / `image` is set (already a documented rule).
  - An `image` Session references an `arbitraryImage: true` pool for its worker.
  - The image passes registry/signature policy (5.2).
  - The creator was entitled (e.g. Session carries an attestation the front door
    stamped, or creation is restricted by RBAC to the front‑door identity).
- This makes hand‑created `kubectl` Sessions subject to the same policy, closing
  the "admin bypass is the only path" gap that exists today.

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
  primary control against malicious/hijacked images.
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
| Q4 | Enforce policy in a validating webhook vs. operator admission code? | Webhook if we want it declarative and reusable; operator code is faster to ship. |
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
