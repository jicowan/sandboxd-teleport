# Stage 2 — Drop the local proxy (AgentCore Gateway front door + OBO identity)

Stage 1 removed the client's SDK + cluster RBAC but kept a local
`aio_proxy.py` container for stdio↔HTTP MCP translation and OIDC. Stage 2
removes that too. Claude Code connects to a **remote MCP endpoint** over HTTP
with native OAuth, and AWS Bedrock AgentCore Gateway is that endpoint.

This reverses [ADR-0001](./adr/0001-no-agentcore-gateway.md) **for the front
door only** — see [ADR-0002](./adr/0002-gateway-for-stage2-frontdoor.md). The
broker still exists and still owns lifecycle.

## Do we still need both the proxy and the broker?

**No — Stage 2 drops the proxy; the broker stays (reshaped).**

- **Proxy → removed.** Its jobs were stdio↔HTTP translation, the OIDC device
  flow, and `X-Sandbox-*` injection. The Gateway provides the client-facing
  HTTP-MCP transport and OAuth; the broker injects the headers. Gone from the
  client: the proxy container, `aio_login.py`, `oidc.json`, the SDK, the
  kubeconfig.
- **Broker → kept, reshaped** from a REST `POST /sessions` service into the
  Gateway's MCP-server target. The Gateway cannot do sandbox lifecycle — only
  the broker can. The Stage-1 lifecycle logic (SDK + ServiceAccount create/
  delete claim) and the `_principal()` / group-check logic are reused.

Result: `Claude → Gateway → broker → sandbox-router → AIO pod`.

## The key enabler: OBO token-exchange preserves user identity (VERIFIED)

The naive worry was that outbound `CLIENT_CREDENTIALS` collapses identity to
the gateway's own M2M principal, killing per-user keying and the
`sandbox-users` gate. The fix — confirmed working against this Keycloak — is
to use **OAuth Token Exchange / On-Behalf-Of (OBO, RFC 8693)** as the
*outbound* grant instead of client-credentials.

Verified on Keycloak 26.5.6 (`TOKEN_EXCHANGE_STANDARD_V2` is on by default):
exchanging jicowan's user token via the `sandbox-gateway-outbound` confidential
client yields a downstream token that still carries:

```
sub: 7613ff43-...            # end user
preferred_username: jicowan  # readable principal
groups: ['sandbox-users']    # the group gate SURVIVES the exchange
azp: sandbox-gateway-outbound # the actor (gateway) is identifiable
aud: [sandbox-router, sandbox-gateway-outbound]
```

So the broker behind the Gateway can recover the end user, key the claim
per-user, and enforce the group gate — exactly as in Stage 1.

### Keycloak prerequisites for OBO (all applied during verification)

1. A confidential client `sandbox-gateway-outbound` with
   `serviceAccountsEnabled=true` and attribute
   `standard.token.exchange.enabled=true` (the OBO actor; the Gateway's
   outbound credential provider uses its id+secret).
2. The actor client must be **within the subject token's audience** — v2
   standard exchange requires it. Added via an `oidc-audience-mapper` on the
   `sandbox` client scope (`included.client.audience=sandbox-gateway-outbound`),
   so user tokens carry `aud: [sandbox-router, sandbox-gateway-outbound]`.
3. The `sandbox` scope's existing `preferred_username` and `groups` mappers
   (added in Stage 1) flow through the exchange unchanged.

Note: Keycloak v2 standard exchange puts the actor in `azp`, not RFC 8693's
nested `act` claim. The broker reads `preferred_username`/`groups` for identity
and may assert `azp == sandbox-gateway-outbound` to confirm the token arrived
via the Gateway.

## Target architecture

```
Claude Code
   │  HTTP MCP (Streamable HTTP) + native OAuth (PKCE against Keycloak)
   ▼
AgentCore Gateway
   │   INBOUND: CUSTOM_JWT authorizer → sandbox realm
   │     - 401 + WWW-Authenticate (RFC 9728); client runs OAuth, gets a
   │       sandbox-realm user token (auth-to-the-gateway-first)
   │     - validates aud/client_id via Keycloak JWKS
   │   SESSION: sessionConfiguration ON → issues client Mcp-Session-Id,
   │     maps it to the backend session
   │   DISCOVERY: searchType default; live tool proxying to the target
   │   OUTBOUND: OAuth2 TOKEN-EXCHANGE (OBO), provider = sandbox-gateway-outbound
   │     - exchanges the inbound user token for a downstream token that
   │       still carries sub/preferred_username/groups
   │  Bearer (OBO downstream token)
   ▼
broker MCP endpoint   (ALB + ACM, default ns)
   │   - validates the OBO Bearer (iss/aud via JWKS; azp == gateway-outbound)
   │   - reads preferred_username (principal) + groups (authz)
   │   - enforces sandbox-users membership
   │   - on initialize: claim-or-reuse a sandbox for this principal/session
   │   - forwards MCP (tools/list, tools/call) to the router, injecting
   │     X-Sandbox-* headers (transparent passthrough of the AIO hub's tools)
   ▼
sandbox-router → AIO pod :8080/mcp
```

Client footprint: `claude mcp add --transport http aio-sandbox <gateway-url>`
plus a one-time browser login. Nothing else.

## What the broker becomes

A **Streamable HTTP MCP server** that:

1. Validates the inbound OBO Bearer as an OAuth2 resource server (one known
   issuer/audience; assert `azp`). Reads `preferred_username` + `groups`.
2. Enforces `sandbox-users` (the gate, now on the real user identity again).
3. On MCP `initialize`, claims-or-reuses a sandbox keyed on the principal
   and/or the Gateway-provided session id, remembers the mapping.
4. Proxies `tools/list` / `tools/call` to the sandbox-router with
   `X-Sandbox-*` headers — transparent (the Gateway relays whatever the AIO
   hub publishes).
5. Releases on session end / TTL.

Stage-1 lifecycle code is reused verbatim; only the front (REST → MCP server)
and the token validation (X-Auth-Request-* → OBO JWT) change.

## Identity model — DECIDED: gateway-level (CLIENT_CREDENTIALS)

The clean per-user OBO path does **not exist** at the Gateway→target layer.
The AWS API enum `OAuthGrantType` is `['CLIENT_CREDENTIALS','AUTHORIZATION_CODE']`
— there is no `ON_BEHALF_OF_TOKEN_EXCHANGE` for gateway-target outbound auth
(that flow only exists on the `GetResourceOauth2Token` identity API, which the
Gateway doesn't use for target forwarding). The installed MCPServer CRD has no
grant-type field at all and the operator hardcodes `CLIENT_CREDENTIALS`; the
operator also reconciles continuously, so post-hoc editing the target's grant
gets reverted.

Decision: **accept gateway-level identity** for Stage 2 (CLIENT_CREDENTIALS).
Consequences:
- The broker receives the Gateway's own M2M token (`sandbox-gateway-outbound`
  service account), not a user token. It carries `azp` and `aud` but **no**
  `preferred_username`/`groups` for the end user.
- The broker keeps the `azp == sandbox-gateway-outbound` assertion (proves the
  call came through the Gateway) but **disables the `sandbox-users` group gate**
  (`AIO_REQUIRED_GROUP=""`) — the M2M token has no user groups.
- Authorization moves to the **Gateway inbound authorizer**
  (`allowedClients`/`allowedAudience` on the sandbox realm). It gates which
  client/audience can call at all, not group membership.
- Per-user attribution / quotas / the `sandbox-users` gate are **deferred** to
  the "finest" authz tier, which would require either 3LO
  (`AUTHORIZATION_CODE`, interactive consent) or a REQUEST interceptor Lambda
  that injects trusted user-claim headers. Tracked, not built.

The OBO Keycloak artifacts (token-exchange client + audience mapper) remain in
the realm — harmless and reusable if we later pursue 3LO or interceptor-based
identity.

## Network posture (Stage 2)

- **Router**: ClusterIP-only, no sidecar, bound `0.0.0.0:8080`. Reachable only
  in-cluster (the broker). Its public ALB/Ingress, R53 record, and ACM cert
  were deleted.
- **Broker**: served by an **internal** ALB (not internet-facing). AgentCore
  Gateway reaches it privately via the MCPServer `privateEndpoint`
  (VPC/subnets/SG), matching the reference alphavantage target. TLS still
  terminates at the ALB. `broker.jicomusic.com` resolves to the internal ALB.
- **Broker auth**: the broker validates the inbound OBO JWT itself (JWKS) —
  no oauth2-proxy sidecar. Verified: `/healthz` 200, unauthenticated `/mcp`
  returns 401.

## STATUS: Stage 2 VERIFIED end-to-end (2026-06-30)

A `tools/call` through the public Gateway URL with a user JWT executed
`sandbox_execute_bash` in a freshly-claimed warmpool sandbox and returned the
output. Full chain proven: Gateway (inbound JWT) → client-credentials → internal
ALB → broker (token validated, sandbox claimed) → ClusterIP router → AIO pod.
27 AIO tools surfaced through the Gateway (namespaced `aio-sandbox-broker___*`).

Gotchas resolved during bring-up (all now in DEPLOY notes):
- **MCP endpoint path**: the Gateway fetches tools from the endpoint as given
  and MCP servers serve at the ROOT (`/`), per the keycloak project docs — not
  `/mcp`. Target FAILED with `{"detail":"Not Found"}` until the broker mounted
  its handler at `/` and the endpoint stayed bare-host.
- **Inbound `insufficient_scope` (403)**: AgentCore's CUSTOM_JWT authorizer
  validates the `client_id` claim, which Keycloak access tokens don't carry by
  default. Fixed with a hardcoded `client_id` claim mapper on the sandbox scope.
- **No Gateway session management**: this API version's MCPGatewayConfiguration
  has no sessionConfiguration field. The Gateway answers `initialize` itself and
  does not return Mcp-Session-Id. The broker still claims per request-flow and
  releases on DELETE; per-session sandbox affinity relies on the broker, and
  header propagation is allowlisted but the Gateway does not surface a session
  id to the client in this version.
- **Target creation latency**: ~5 min while the privateEndpoint (VPC Lattice)
  provisions; CREATING is normal, not a failure.

## KNOWN ISSUE — session affinity (investigated 2026-06-30)

Symptom: after a `browser_navigate` to a heavy page timed out, subsequent tool
calls failed with `{"detail":"Unknown MCP session"}` (HTTP 404), and the
client retried (looked like a hang). Leaked claims accumulated (no DELETE).

Isolation test (hitting the broker DIRECTLY, bypassing the Gateway, from
inside the broker pod): `initialize` → 200 with `Mcp-Session-Id`; `tools/list`
WITH that id → 200. **The broker's session→claim logic is correct.**

Conclusion: the break is at the **Gateway hop**. This API version's Gateway has
no session management; it opens its own backend session to the broker and does
not reliably resurface the broker's `Mcp-Session-Id` after a backend
connection reset (which the navigation *timeout* triggered). The broker then
receives either a stale id it never issued or a fresh initialize, and 404s /
spawns a new claim.

### Resolution (broker 0.2.3)

Tried principal-keying first; reverted it — it caused per-call sandbox churn
(a new claim per request, prior ones torn down → "Could not connect to backend
sandbox"). Reverted to **session-keyed** mapping with one change that removes
the brittleness: **on an unresolved/absent session id, claim-or-reuse instead
of returning 404.** So `_resolve_sandbox_id(sid)` miss → `_claim_sandbox(sid)`
rather than `HTTPException(404)`. Per-session sandbox mapping is preserved; the
"Unknown MCP session" failure mode is gone.

Verified after deploy:
- Claims persist (probed: a claim stayed stable >20s; not a lifecycle bug —
  the earlier "vanishing claims" was churn from the broken per-call logic +
  stray DELETEs, now gone).
- `_resolve_sandbox_id` returns the bound sandbox for a known session, None for
  unknown (→ claim path).
- Gateway end-to-end: initialize + 3 sequential bash calls → no 404s, exactly
  1 claim, no leak. Broker logs show only 200/202 (+ a 204 on DELETE).

### Still flaky: Gateway↔AIO re-initialize latency (NOT a broker bug)

Intermittently a call fails with `MCP server did not respond to the initialize
request within 40 seconds`. Broker logs show **no broker error** on these — the
Gateway periodically opens a *fresh backend MCP session* (a new initialize, seen
as a 202), and the AIO hub sometimes takes >40s to answer that re-initialize.
The 40s budget is the Gateway's backend-initialize timeout; the latency is the
AIO hub cold-initializing. The next call typically succeeds against the same
sandbox. Largely outside the broker's control; mitigations to explore: a
warm/keepalive ping to keep the AIO hub session hot, or check whether the
Gateway's backend timeout is tunable. Heavy pages (buzzfeed) also exceed
`browser_navigate`'s own budget — separate from this.

## Build plan (incremental, verify-first)

1. **OBO token-exchange in Keycloak — DONE/VERIFIED.** `sandbox-gateway-outbound`
   client + audience mapper; exchange preserves user identity.
2. **Broker MCP endpoint — DONE (code + auth verified).** `broker_mcp.py` is a
   Streamable HTTP MCP server:
   - **Resource server**: validates the inbound OBO Bearer against Keycloak
     JWKS (RS256, iss, aud=`sandbox-router`, exp) and asserts
     `azp == sandbox-gateway-outbound` so only Gateway-exchanged tokens pass.
     Reads `preferred_username` + `groups`; enforces `sandbox-users`.
     **Verified** against live Keycloak: OBO token → accepted (jicowan,
     [sandbox-users]); plain non-exchanged token → rejected on azp.
   - **Session→claim**: on MCP `initialize` (no `Mcp-Session-Id`) it mints a
     session id, claims a warmpool-adopted, template-backed sandbox, labels
     the SandboxClaim with the session id + principal, forwards the
     initialize, and returns `Mcp-Session-Id`. Subsequent calls resolve the
     sandbox by the session label (stateless across replicas). `DELETE /mcp`
     releases.
   - **Transparent forwarding**: relays MCP JSON-RPC to the router with
     `X-Sandbox-*`, streaming the response (json or SSE) unchanged.
   - One image, two apps: `BROKER_APP=mcp` (Stage 2, default) or `rest`
     (Stage 1 fallback), selected by `entrypoint.sh`.

   Implementation decisions made (flag for review):
   - The broker forwards to the router over plain in-cluster HTTP
     (`sandbox-router-svc...:8080`) and does NOT re-authenticate to the
     router. This assumes the router trusts in-cluster callers OR we add a
     bearer. Today the router sits behind its own oauth2-proxy sidecar on the
     ALB path; the in-cluster ClusterIP path needs confirming (the broker may
     need to hit the sidecar port and present a token).
   - Claim keyed on the **MCP session id** (label), not the principal —
     gives one sandbox per MCP session (multiple concurrent sessions per user
     get separate sandboxes). Principal is recorded as a second label for
     attribution / future per-user policy.
3. **AWS wiring.** Create an OAuth2 credential provider (token vault) for
   `sandbox-gateway-outbound` with `grant_type` = token-exchange; create the
   Gateway (CUSTOM_JWT inbound authorizer + sessionConfiguration); register the
   broker as an `MCPServer` target with the OBO outbound provider.
4. **Verify with curl** end-to-end through the Gateway (initialize →
   tools/list → tools/call); confirm session-id round-trips, tools are live,
   and the broker logs the real `preferred_username`.
5. **Register in Claude Code** with `claude mcp add --transport http`; confirm
   the native OAuth flow + a real sandbox tool call.
6. **Retire** the Stage-1 local proxy registration (keep `aio_proxy.py` in the
   repo as the Stage-1 fallback).

## Open questions to resolve during build

1. **Does AWS's outbound credential provider expose RFC 8693 token-exchange
   with the inbound user token as `subject_token`?** AWS docs describe OBO via
   `GetResourceOauth2Token` (`oauth2Flow=ON_BEHALF_OF_TOKEN_EXCHANGE`). Confirm
   the credential-provider config wires the Keycloak `sandbox-gateway-outbound`
   client and that the Gateway sends the inbound token as the subject. (We've
   proven Keycloak accepts the exchange; the open part is the AWS-side config.)
2. **Session identifier the broker sees.** Confirm what session id arrives on
   the backend connection and that it's stable per client session, so claim
   reuse works.
3. **Group gate placement.** Broker-side check on the OBO token's `groups`
   (precise) vs. Gateway inbound `allowedClients` (coarse). Plan: keep the
   broker-side group check; it's where Stage 1 left it and the claim survives
   the exchange.
