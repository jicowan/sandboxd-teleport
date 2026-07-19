# PRD — delegated agent access (protocol‑native user‑identity propagation to sandboxes)

Status: **Proposed** (not scheduled). Decision‑ready spec; grounded in the shipped
code on the `checkpoint-restore` branch. Related:
[architecture-sandboxd.md](sandboxd/architecture-sandboxd.md),
[architecture-broker.md](sandboxd/architecture-broker.md),
[PRD-sandbox-iam-credentials.md](PRD-sandbox-iam-credentials.md) (the AWS analog).

> **Update (2026‑07‑09):** investigated "can the router pass an optional JWT to the
> sandbox generically?" Answer: **yes, and the router + worker need zero changes** —
> the broker is the only seam (see §5.2, verified against the code). Added **Phase 0
> — raw passthrough** (§5.5) as the near‑zero‑code starting point for trusted pools.
>
> **Update 2 (2026‑07‑09) — the key reframe:** identity should ride each protocol's
> **native auth channel**, not a bespoke `X-Delegated-Authorization` header a workload
> must be specially coded to read. MCP and A2A already define *where identity goes and
> how the receiver validates it* — MCP servers are OAuth **resource servers** (token on
> the transport `Authorization`); A2A propagates per the agent card's declared security
> scheme. So the broker stays **protocol‑aware** (that's a feature, not AIO coupling),
> and "generic" means two narrower things: **(a) de‑couple from AIO specifically**
> (names/defaults + AIO's cold‑start/warm quirk → per‑pool config), and **(b) become
> multi‑protocol via pluggable adapters** (MCP today, A2A next), where each adapter
> knows the protocol's lifecycle *and* its identity‑propagation mechanism. This is now
> §5.6, and it flips Q1 (use the protocol‑native `Authorization`, not a custom header).

## 1. Summary

Let an agent or MCP server running **inside a sandbox act on behalf of the calling
user** against downstream app APIs and other MCP servers — using the *user's*
delegated authority, not the platform's or the sandbox's service identity. Two
identity flows:

- **Inbound:** a sandboxed MCP server learns *who the caller is* (today the broker
  strips identity to `X-Session-ID`, so the workload has zero knowledge of the user).
- **Outbound:** a sandboxed agent presents the user's delegated authority when
  calling further downstream.

Both need the same missing capability: **the caller's identity, in a downstream‑
usable form, propagated past the broker into the sandbox.** Phase 1 does this
**per request** (interactive: the user drives the agent live), which sidesteps the
token‑lifetime problem. Autonomous/offline delegation is a harder Phase 2 (§9).

This is the application‑layer JWT‑delegation pattern (nested `act`/`obo` claims,
RFC 8693 token exchange) — *not* AWS IAM. It complements the AWS path
(`PRD-sandbox-iam-credentials.md`); use that for AWS resources, this for app/MCP.

> **Design principle — propagate identity through the protocol's native channel.**
> The broker is **protocol‑aware on purpose.** MCP and A2A already specify *how a
> receiver takes and validates identity* — an MCP server is an OAuth **resource
> server** that validates a bearer token on the transport `Authorization`; an A2A
> agent declares its accepted **security scheme** in its agent card. So the broker
> injects identity into *that* native slot, which means the in‑sandbox workload
> validates it with **standard, off‑the‑shelf protocol machinery** — no custom header,
> no "the workload must be specially coded to read our header." A generic HTTP proxy
> couldn't do this (it would have to invent a side‑channel an arbitrary app has no
> reason to understand); a protocol‑aware broker can. See §5.6 for the multi‑protocol
> adapter model and what "generic broker" therefore means.

## 2. Background — where identity dies today

The user's Keycloak JWT flows `MCP client → agentgateway (verify) → broker (verify)`.
Then `broker_sandboxd.py._forward()` sends only `X-Session-ID` + `X-Session-Pool`
to the router; the **user identity is dropped at the broker**. Router, operator,
worker, and the sandbox workload never see the user. So a sandboxed MCP server
can't authenticate its caller, and a sandboxed agent has no user credential to
present downstream — it has only whatever service identity it carries.

The broker is the right seam: it already **holds the validated JWT + claims**
(`preferred_username`, `groups`, `azp`, …) and is a confidential OAuth client, so
it can perform token exchange. The data path broker→router→workload already
exists (it proxies `/mcp`); this adds a header instead of stripping identity.

## 3. Goals / non‑goals

### Goals

1. A sandboxed MCP server can **authenticate its caller** (inbound): it receives a
   verifiable token identifying the user for the current request.
2. A sandboxed agent can **act on behalf of the user** (outbound) against downstream
   services that understand the delegated token, scoped to what the user permits.
3. **Attribution:** downstream can tell it was *this user* via *this agent* (the
   delegation relationship is explicit in the token — `act`/`obo`).
4. **Scoped, least‑privilege:** the delegated token carries only the scopes the
   agent needs ∩ the user has, not the user's full authority.
5. Phase 1 requires **no durable storage of user tokens** and doesn't fight the
   durable‑session / teleport model (per‑request lifetime).

### Non‑goals

- **Not** AWS IAM/STS delegation — that's `PRD-sandbox-iam-credentials.md`
  (`AssumeRoleWithWebIdentity` / session tags). This PRD is app/MCP‑layer.
- **Not** making external downstreams understand our token — they must speak the
  same delegation format (§8). Fully in our control only where the downstream is
  another MCP server *we* host.
- **Not** autonomous/offline delegation in Phase 1 (agent acting when the user
  isn't connected) — that's Phase 2 (§9), with an unsolved durable‑grant core.
- **Not** a new sandbox runtime capability — the in‑sandbox server consumes identity
  with **standard protocol machinery** (MCP resource‑server validation of the transport
  `Authorization`; A2A's declared scheme), not sandboxd‑specific code. (This is weaker
  than the pre‑reframe "must read our custom header" — see Update 2 / §5.6.)

## 4. The interactive insight (why Phase 1 is tractable)

The user drives the agent **interactively** through their MCP client. So on **every
inbound MCP request** the broker already has a **fresh, valid user JWT**. Delegation
can therefore be **per request**:

```
per inbound MCP request:
  broker validates user JWT (already does)
    → RFC 8693 token exchange at Keycloak → scoped delegated token (act/obo)
    → inject as a header on the proxied /mcp request into the sandbox
  agent / MCP server in the sandbox uses that token for downstream calls
    made DURING this request
```

**Token lifetime = request lifetime.** Nothing is stored in the session; the
delegated token rides each request. This dodges the crux that makes offline
delegation hard (our sessions are long‑lived and teleport; user access tokens are
~1h and we don't hold the user's refresh token — the MCP client does).

## 5. Proposed design (Phase 1 — interactive, per‑request)

### 5.1 Broker: token exchange + inject

In `broker_sandboxd.py`, on each forwarded MCP request (`_forward`):

- After validating the inbound user JWT, perform **RFC 8693 token exchange** against
  Keycloak: `subject_token` = the user JWT, requesting a delegated token for the
  agent audience with **downscoped scopes** (agent‑needs ∩ user‑has). Keycloak
  supports standard token exchange; the broker is a confidential client.
- The resulting token carries the delegation relationship: `sub` = agent, `act.sub`
  = user (or `obo`), plus the filtered scopes.
- **Inject it into the sandbox via the protocol's native auth channel** (§5.6). For
  MCP that is the transport **`Authorization: Bearer <delegated-token>`** the
  in‑sandbox MCP resource server already validates — *not* a bespoke
  `X-Delegated-Authorization` (that was the pre‑reframe design; see Update 2). For A2A,
  per the agent card's declared security scheme. Optionally also pass
  `X-On-Behalf-Of: <subject>` for audit/convenience.
- **Audience must match the receiver.** Because an MCP resource server validates
  `aud`, the exchanged token's audience must be the **in‑sandbox MCP server's**
  audience — not `sandbox-router`. This is precisely why the MCP‑native path needs
  RFC 8693 **exchange** (re‑mint with the right `aud` + downscope), and why raw
  passthrough (§5.5) only works on a pool whose MCP server is deliberately configured
  to accept the router audience.
- **Opt‑in per pool/session** (see 5.4). When off, behavior is unchanged (no
  identity forwarded) — safe default.

Token exchange failures are non‑fatal to the request unless the pool requires
delegation (config): fail closed only when the workload can't function without it.

### 5.2 Router / worker: pass‑through — verified to need **zero code changes**

Investigated 2026‑07‑09 against the shipped code. The propagation path from the
broker into the sandbox already carries an arbitrary header end‑to‑end, so **neither
the router nor the worker needs any change** to deliver a token to the workload:

- **Router (`internal/router/router.go`):** it proxies via `r.Clone(ctx)` →
  `httputil.ReverseProxy`. Go's `ReverseProxy` copies **all** request headers to the
  upstream and strips only the hop‑by‑hop ones (`Connection`, `Keep‑Alive`,
  `Transfer‑Encoding`, …). It never touches `Authorization` or any `X‑*` header. The
  router's identity code (`session.go`) only *reads* `X‑Session‑ID`/`X‑Session‑Pool`
  and *writes* none — a grep confirms no `Header.Del`/`Header.Set` on the proxied
  request. So any header the broker adds is forwarded to the worker unchanged, for
  free. The router stays protocol‑agnostic — it doesn't parse or depend on the token.
- **Worker (`sandboxd/network.go`):** the worker delivers inbound traffic to the
  sandbox via **nftables DNAT** (`podIP:hostPort → interiorIP:containerPort`) — pure
  kernel networking, no userspace proxy in the path. It is completely transparent to
  HTTP headers, so the token reaches the sandboxed workload's listener untouched.

Net: this is purely data‑plane header propagation originating at the broker — no KV,
no operator involvement, no new endpoint, unlike the AWS credential vendor (which
needed a control‑plane role + a link‑local vending endpoint). **The entire feature is
"the broker adds one header."**

> Trust note: the router forwarding a bearer token into the sandbox is only safe
> because the sandbox is the intended audience and (for Phase 1) the token is
> downscoped + short‑lived. This interacts with the deferred **P1.5** hardening
> (router trusting headers, mTLS worker↔router) — token forwarding should ship with
> or after P1.5 so the propagation channel is authenticated. This matters *more* for
> Phase 0 raw passthrough (§5.5), where the forwarded token is the user's full JWT.

### 5.3 Sandbox workload: validates via standard protocol machinery

Because identity rides the protocol's native channel (§5.6), the in‑sandbox workload
consumes it with **off‑the‑shelf protocol machinery**, not custom code for our header:
- **Inbound MCP server:** it is an OAuth **resource server** — it already validates
  the transport `Authorization` bearer (JWKS from Keycloak, `aud` = its own audience)
  and enforces per‑user authz on its tools/resources. Standard MCP auth; nothing
  sandboxd‑specific to implement.
- **Outbound agent:** attaches the delegated token as `Authorization` on downstream
  calls to services that accept the delegation format (another hosted MCP server, an
  A2A peer).

This is a far weaker "cooperation" requirement than the pre‑reframe design: instead of
"the workload must be coded to read `X-Delegated-Authorization`," it is "the workload
speaks its own protocol's standard auth" — which a compliant MCP/A2A server already
does. (Where the downstream is another MCP/A2A endpoint we host, both ends are just
speaking the protocol.)

### 5.4 Configuration / opt‑in

- Per‑pool via `SandboxTemplate` (e.g. `spec.delegation.enabled` + the target
  audience/scopes the pool's workload needs), mirroring how `iam.roleArn` is per‑pool.
  Off by default.
- The set of scopes that may be requested is **operator‑declared** (not chosen by
  the untrusted workload); the broker filters to `requested ∩ user‑has` at exchange
  time.

### 5.5 Phase 0 — raw JWT passthrough (simplest starting point; opt‑in)

Because §5.2 proved the propagation path needs **no** router/worker changes, the
smallest possible increment — ahead of the RFC 8693 exchange in §5.1 — is for the
broker to forward the **user's own validated JWT** into the sandbox unchanged, as an
opt‑in per‑pool header. No token exchange, no Keycloak config beyond what the broker
already does to validate the inbound token.

- **What it delivers:** the sandboxed MCP server receives a verifiable token
  identifying the calling user for the current request, in the slot it already
  validates.
- **Mechanism:** in `broker_sandboxd.py`'s forward path, when the pool opts in, set the
  proxied transport **`Authorization: Bearer <the inbound user JWT>`** (optionally
  `X‑On‑Behalf‑Of: <subject>` for audit). The broker already holds the validated
  token. The router forwards it and the worker DNATs it through, both unchanged (§5.2).
- **Only works if the in‑sandbox server accepts the router audience.** Since the raw
  user token has `aud=sandbox-router`, a Phase‑0 MCP server must be configured to
  accept that audience — fine for a first‑party/dev pool you control, not for arbitrary
  workloads. That audience mismatch is the concrete reason Phase 1's exchange (re‑mint
  to the server's own `aud`) is the real path.
- **Opt‑in, off by default:** gated by a `SandboxTemplate` field (§5.4). When off,
  behavior is unchanged (no identity forwarded) — the safe default.

**Why Phase 0 is not the safe end state (the tradeoff, stated plainly):** the raw
user JWT is *not* downscoped or audience‑bound to the sandbox. It carries the user's
full scopes/groups, has the router audience, and is valid for its full lifetime
(~1h). Handing it to an **untrusted, multi‑tenant sandbox** means a compromised or
malicious workload can **replay the user's full authority** anywhere that token is
accepted, for the rest of its lifetime. That is exactly the confused‑deputy /
token‑theft risk §7 calls out, and it is why the real Phase 1 does RFC 8693
downscoping (agent‑needs ∩ user‑has, audience‑bound, short‑lived) before injection.

**So Phase 0 is appropriate only for trusted pools** — e.g. a first‑party workload
image the operator controls, or a dev/test pool — **and must ship with or after
P1.5** (the router→worker hop is unauthenticated today; forwarding the user's full
bearer token over it is the worst case for interception). It is a stepping stone that
exercises the propagation path and the per‑pool opt‑in with near‑zero code, not a
substitute for the downscoped exchange for untrusted or arbitrary‑image pools.

### 5.6 Generic broker: protocol‑aware core + pluggable adapters

"Generic broker" does **not** mean protocol‑agnostic — being MCP‑aware is what lets us
propagate identity natively (§ Design principle). It means two things:

**(a) De‑couple from AIO *specifically*.** The broker's AIO ties are shallow and
config‑shaped:
- **Names/defaults** — `SANDBOXD_POOL=aio-pool`, `EXPECTED_AUDIENCE=sandbox-router`,
  `GATEWAY_AZP=aio-sandbox-client`, `REQUIRED_GROUP=sandbox-users` are already
  env‑driven; only the *defaults* are AIO. Re‑default / document them.
- **Cold‑start/warm is AIO‑specific.** The instant‑`initialize` + background `_warm`
  machinery exists to hide AIO's ~45s boot. Another MCP workload has a different (or
  no) cold‑start profile. Drive warm‑on‑connect from the pool's `SandboxTemplate`
  (health/warm config), not a broker constant.

**(b) Be multi‑protocol among *identity‑carrying* protocols.** Factor the broker into a
**protocol‑agnostic core** + a **per‑pool protocol adapter**:

- **Core (shared):** JWT validation + group gate, per‑user quota, **session resolution
  / addressing** (§ below), and transparent streaming forward to the router with
  `X‑Session‑ID`/`X‑Session‑Pool`. None of this is protocol‑specific.
- **Adapter (per pool, from the `SandboxTemplate`):** knows the protocol's **lifecycle**
  *and* its **identity‑propagation mechanism**:
  - **`mcp`** (today): answer `initialize` locally, rewrite protocol version,
    short‑circuit `notifications/initialized`/`ping`, key sessions off
    `mcp-session-id`, warm‑on‑connect; propagate identity as the transport
    `Authorization` bearer (the MCP resource‑server slot).
  - **`a2a`** (next — validates the abstraction): A2A task/context lifecycle; propagate
    per the agent card's declared **security scheme**.
  - **`raw`** (optional): passthrough with no protocol lifecycle, identity as
    `Authorization` — the trusted‑pool shortcut of §5.5 for workloads that just want the
    bearer.

Which adapter runs is **per‑pool config**, since the `SandboxTemplate` already knows the
workload's shape (ports/health). The identity‑propagation step (§5.1) is then an adapter
capability, not broker‑global behavior.

**Session addressing (unblocks this and ForkSet).** Today `_sid_for` bakes in one
durable session per principal (`sess-<principal>-<hash>`). A generic broker must let the
**protocol's own session token** select the session — MCP's `mcp-session-id`, A2A's
task/context id — so a caller can address a *specific* session (e.g. a fork
`sess-fork-…-3`), not only the principal‑derived one. This is the same need
[[PRD-snapshot-fork]] (ForkSet) has for addressing individual forks, and it's the
deepest of the changes here (it touches the broker↔router session contract). The router
itself is unchanged — it already resolves whatever `X‑Session‑ID` it's handed (§5.2).

**Net:** the JWT‑propagation work and the "generic broker" work are the *same* refactor
seen from two angles — a **protocol‑aware core with pluggable, identity‑propagating
adapters**, where identity always rides the protocol's native mechanism (with RFC 8693
exchange to get the audience right).

### 5.7 What lives in the broker vs. agentgateway

The front door is already two components with a clean split
([architecture-broker.md](sandboxd/architecture-broker.md)): **agentgateway** (the
internet‑facing MCP‑aware edge) and the **broker** (internal, session‑aware). The
principle for placing this work: **agentgateway does coarse, stateless, edge‑global
policy; the broker does per‑session/per‑pool context that only it has.**

| Concern | Where | Why |
|---------|-------|-----|
| Verify the user JWT (issuer/aud/sig), reject at the edge | **agentgateway** (already does) | Stateless, request‑local; belongs at the internet edge as defense‑in‑depth. |
| Coarse tool/eligibility gate (the `tools/list` allowlist by group) | **agentgateway** (already does) | Edge‑global policy that needs no session context; already implemented there. |
| RFC 8693 **token exchange** (re‑mint → downscoped, audience = the in‑sandbox server) | **broker** | Needs the **target pool's** requestable scopes + the in‑sandbox server's audience — per‑pool context agentgateway doesn't carry. The broker already holds the validated claims and is a confidential OAuth client. |
| **Protocol‑native injection** (put the exchanged token on the MCP `Authorization` / A2A scheme) | **broker** (the adapter, §5.6) | The broker is the seam that talks to the specific pool/session and knows which adapter applies. |
| **Session resolution / addressing** (which `sess-…` / fork this request targets) | **broker** | Inherently per‑session; agentgateway is session‑agnostic. |
| Fan‑out quota / per‑subject session cap | **broker** | Needs session‑count state the broker owns. |

Why not push exchange up into agentgateway? Two reasons: (1) the exchange audience +
scope set are **per‑pool** (they depend on which sandbox the session lands in), which
agentgateway doesn't know — it terminates MCP generically and forwards; (2) it would
couple the edge to per‑pool delegation config, undoing the clean "edge = coarse global
authz, broker = session/pool context" separation the deployment already has. So the
edge stays as‑is (verify + coarse gate + forward the JWT unchanged), and **all of §5.1,
§5.6 lives in the broker.** If a future need arises to enforce delegation *policy* at
the edge (e.g. "this client may never delegate"), that coarse rule can live in
agentgateway while the mechanism stays in the broker.

## 6. Authorization gate (shared with BYOC / IAM)

*Who* may run an agent that acts on their behalf, and *which* audiences/scopes a
pool's agent may request, is the **same subject→entitlement decision** deferred for
BYOC and per‑session IAM. Build the gate once; all three consume it:
- Front door (broker/agentgateway) decides eligibility by group/claim;
- the scope downscoping (§5.1) is the per‑request enforcement.
This PRD assumes that gate; it does not re‑invent it.

## 7. Security considerations

- **Downscoping is mandatory** — never forward the user's full token/authority; the
  delegated token carries only agent‑needs ∩ user‑has, short‑lived.
- **Confused deputy / audience:** the delegated token's audience must be the
  intended downstream, so a malicious sandbox can't replay it elsewhere.
- **Untrusted workload holds a user‑scoped token** for the request — acceptable
  because it's downscoped + short‑lived + audience‑bound, and the sandbox is the
  agent the user is deliberately delegating to. Log every exchange (user, agent,
  scopes, downstream) for attribution.
- **Propagation channel** must be authenticated (P1.5): the router→worker hop
  carries a bearer token; mTLS + NetworkPolicy prevent interception/injection.
- **Revocation/audit:** carry `log_url`/`revocation` metadata if the downstream
  supports it; at minimum the broker audits each exchange.

## 8. The standards / interop reality (be honest)

- The blog's model works because **each downstream validates the delegation chain**.
  We only control that where the downstream is **an MCP server we host** — there it
  works fully. For **external** APIs/MCP servers, delegation works only if they
  accept the token format.
- The OAuth **agent‑delegation drafts** (`act`/`obo`) are **not finalized** — this
  is partly a standards bet. Keycloak's **RFC 8693 token exchange is available**, so
  the exchange mechanism is real today; the *claim shape* we emit may need to track
  the draft. Recommend: implement exchange now, keep the emitted claim structure
  configurable so it can follow the standard as it lands.

## 9. Phase 2 — autonomous / offline delegation (harder; scoped separately)

Phase 1 breaks the moment the agent works **outside a live user request**: a
background loop, a tool call outliving the token, or a suspended session resuming
when the user isn't connected. True offline delegation needs what we don't have:

- A **durable delegation grant** — the user consents once ("this agent may act for
  me"), stored server‑side, revocable.
- A **refresh authority** — we don't hold the user's refresh token (the MCP client
  does), so the broker can't silently re‑mint. Options: a dedicated offline‑access
  grant the user authorizes to the broker (broker becomes a holder of an
  offline/refresh token for the agent audience), or a delegation service that
  re‑issues from a stored grant.
- Interaction with **teleport**: the grant reference (not a live token) travels with
  the session; the token is re‑minted on resume.

This is the genuinely unsolved core and is intentionally **out of scope for Phase
1**. It gets its own PRD once Phase 1 proves the propagation path.

## 10. Testing / acceptance (Phase 1)

1. **Inbound:** a sandboxed MCP server receives `X-Delegated-Authorization`,
   validates it against Keycloak JWKS, and reports the correct user subject +
   scopes for the request.
2. **Outbound:** a sandboxed agent calls a second hosted MCP server with the token;
   the second server sees `act.sub` = user and enforces per‑user authz.
3. **Downscope:** the delegated token's scopes = requested ∩ user‑has (a user
   lacking a scope yields a token without it).
4. **Off by default:** a pool without delegation enabled forwards no identity
   (unchanged behavior).
5. **Fail modes:** exchange failure → request proceeds without a token unless the
   pool requires it (then 4xx), never a wedge.

Acceptance: an interactive agent in a sandbox makes a downstream call attributable
to the calling user, scoped to the user's permissions, with no user token stored.

## 11. Effort estimate

- **Phase 0 (raw passthrough, §5.5): small.** Broker adds one opt‑in header + a
  `SandboxTemplate.spec.delegation.enabled` field. Router/worker: **zero changes**
  (§5.2, verified). No Keycloak work beyond existing inbound validation. Appropriate
  for trusted pools; ship with/after P1.5.
- **Phase 1 (downscoped, §5.1): medium.** Adds RFC 8693 token exchange at Keycloak +
  the scope‑downscoping config on top of Phase 0's plumbing. Still no new
  control‑plane component (contrast the AWS vendor). Pairs with P1.5 for the
  propagation channel.
- **Phase 2 (offline, §9): large, separate PRD** — the durable‑grant / refresh‑authority
  core is unsolved.

## 12. Open questions

| # | Question | Leaning |
|---|----------|---------|
| Q1 | Where does the delegated token ride? | **Flipped by Update 2:** the protocol's **native** channel — MCP transport `Authorization: Bearer …` (the resource‑server slot), A2A's declared scheme; optional `X-On-Behalf-Of: <sub>` for audit. Not a bespoke `X-Delegated-Authorization`. |
| Q8 | How much lives in the broker vs. agentgateway? | See §5.7 — agentgateway does coarse edge authn/authz (verify JWT, group/tool gate) it already does; the **broker** owns per‑request token exchange + protocol‑native injection + session addressing, because those need per‑session/per‑pool context the gateway doesn't have. |
| Q9 | One broker process per protocol, or one broker with adapters? | One core + pluggable adapters selected per pool (§5.6); revisit only if a protocol needs a wholly separate deployment. |
| Q2 | Emit `act`/`obo` per the OAuth draft, or a simpler custom claim now? | Do RFC 8693 exchange now; make the emitted claim shape configurable to track the draft. |
| Q3 | Where are the agent's requestable scopes declared? | Operator‑declared per pool (`SandboxTemplate`), filtered to ∩ user‑has at exchange. Never workload‑chosen. |
| Q4 | Ship before, with, or after P1.5? | With/after — the propagation channel should be authenticated (mTLS + NetworkPolicy) before forwarding bearer tokens to workers. |
| Q5 | Does the sandbox get the token every request, or cached for the MCP session? | Every request (per‑request lifetime is the whole point); revisit only if perf demands. |
| Q6 | Phase 2 refresh authority: broker holds an offline grant vs. a delegation service? | Defer to the Phase 2 PRD. |
| Q7 | Start with Phase 0 raw passthrough or go straight to downscoped Phase 1? | Phase 0 is fine to exercise the path on a **trusted** pool (near‑zero code); it is **not** safe for untrusted/arbitrary‑image pools — those need Phase 1 downscoping. Both share the same broker seam + opt‑in field, so Phase 0 → Phase 1 is additive. |
