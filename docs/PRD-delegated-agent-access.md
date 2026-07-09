# PRD — delegated agent access (user‑identity propagation to sandboxes)

Status: **Proposed** (not scheduled). Decision‑ready spec; grounded in the shipped
code on the `checkpoint-restore` branch. Related:
[architecture-sandboxd.md](sandboxd/architecture-sandboxd.md),
[architecture-broker.md](sandboxd/architecture-broker.md),
[PRD-sandbox-iam-credentials.md](PRD-sandbox-iam-credentials.md) (the AWS analog).

> **Update (2026‑07‑09):** investigated "can the router pass an optional JWT to the
> sandbox generically?" Answer: **yes, and the router + worker need zero changes** —
> the broker is the only seam (see §5.2, verified against the code). Added **Phase 0
> — raw JWT passthrough** (§5.5) as the near‑zero‑code starting point for trusted
> pools, with the token‑theft tradeoff stated plainly, plus a generic scope note
> (any sandboxed workload MAY consume the token; sandboxd doesn't assume agents/MCP).

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

> **Scope note.** sandboxd is a **generic platform for running sandboxed pods** — an
> agent or MCP server is one possible workload, not an assumption. This PRD uses
> agent/MCP examples because delegation is most obviously useful there, but the
> *mechanism* — propagate the caller's identity token into the sandbox as a header —
> is workload‑agnostic. Any sandboxed workload MAY read the injected token; one that
> ignores it is unaffected. Nothing below requires the workload to be an agent.

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
- **Not** a new sandbox runtime capability — the agent/server code must be written
  to read the injected token (no transparent SDK convention exists, unlike AWS).

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
- **Inject it into the sandbox** as a header on the proxied request — proposed
  `X-Delegated-Authorization: Bearer <delegated-token>` (kept distinct from the
  sandbox workload's own `Authorization`, if any). Also pass the raw user subject
  (`X-On-Behalf-Of: <subject>`) for convenience/audit.
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

### 5.3 Sandbox workload: reads the token (cooperation required)

The agent / MCP server in the sandbox reads `X-Delegated-Authorization` and:
- **Inbound MCP server:** treats it as the caller's identity — validates it (JWKS
  from Keycloak), enforces per‑user authorization on its tools/resources.
- **Outbound agent:** attaches it as the `Authorization` on downstream calls to
  services that accept the delegation format.

This is a **documented contract**, not transparent — the workload must be written
to use the header. We provide the header + a validation recipe; the workload
cooperates. (Where the downstream is another sandbox MCP server we host, both ends
follow the same contract.)

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

- **What it delivers:** the sandboxed workload — **whatever it is** (sandboxd is a
  generic platform for running sandboxed pods; there is no assumption that an agent or
  MCP server runs inside) — receives a verifiable token identifying the calling user
  for the current request. A workload that wants caller identity or a user credential
  can read and validate it; a workload that ignores it is unaffected.
- **Mechanism:** in `broker_sandboxd.py._forward`, when the pool opts in, add
  `X‑Delegated‑Authorization: Bearer <the inbound user JWT>` (and optionally
  `X‑On‑Behalf‑Of: <subject>`). The broker already holds the validated token. The
  router forwards it and the worker DNATs it through, both unchanged (§5.2).
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
| Q1 | Header name/shape for the delegated token? | `X-Delegated-Authorization: Bearer …` + `X-On-Behalf-Of: <sub>`; keep distinct from any workload `Authorization`. |
| Q2 | Emit `act`/`obo` per the OAuth draft, or a simpler custom claim now? | Do RFC 8693 exchange now; make the emitted claim shape configurable to track the draft. |
| Q3 | Where are the agent's requestable scopes declared? | Operator‑declared per pool (`SandboxTemplate`), filtered to ∩ user‑has at exchange. Never workload‑chosen. |
| Q4 | Ship before, with, or after P1.5? | With/after — the propagation channel should be authenticated (mTLS + NetworkPolicy) before forwarding bearer tokens to workers. |
| Q5 | Does the sandbox get the token every request, or cached for the MCP session? | Every request (per‑request lifetime is the whole point); revisit only if perf demands. |
| Q6 | Phase 2 refresh authority: broker holds an offline grant vs. a delegation service? | Defer to the Phase 2 PRD. |
| Q7 | Start with Phase 0 raw passthrough or go straight to downscoped Phase 1? | Phase 0 is fine to exercise the path on a **trusted** pool (near‑zero code); it is **not** safe for untrusted/arbitrary‑image pools — those need Phase 1 downscoping. Both share the same broker seam + opt‑in field, so Phase 0 → Phase 1 is additive. |
