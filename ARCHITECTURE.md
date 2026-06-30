# Architecture

How a Claude Code user gets an authenticated, per-user AIO sandbox over MCP,
and how authentication and authorization work at each hop.

## Components and trust boundaries

```
   PUBLIC INTERNET           │            EKS CLUSTER (private)
                             │
  ┌──────────────┐          ALB          ┌───────────────┐   ┌──────────────┐
  │  Claude Code │──HTTPS──▶ (ACM TLS) ──▶│ agentgateway  │──▶│   broker     │
  │  (laptop)    │   MCP +   internet-     │ (MCP gateway) │   │ (MCP server) │
  └──────┬───────┘   OAuth   facing        └──────┬────────┘   └──────┬───────┘
         │                             passthrough │ user JWT          │ X-Sandbox-*
         │  browser OAuth (PKCE)                   │                   ▼
         ▼                                         │            ┌──────────────┐
  ┌──────────────┐                                 │            │sandbox-router│
  │   Keycloak   │◀────────────────────────────────┘            │ (ClusterIP)  │
  │ sandbox realm│   validate JWT (JWKS) at every hop           └──────┬───────┘
  └──────────────┘                                                     │ headless svc
                                                                       ▼
                                                              ┌──────────────┐
                                                              │  AIO sandbox │
                                                              │  pod (/mcp)  │
                                                              └──────────────┘
```

- **agentgateway** terminates the client connection and inbound OAuth; it
  forwards the *user's own* token downstream (`backendAuth: passthrough`).
- **broker** is the only component with RBAC to create `SandboxClaim`s. It
  re-validates the token (defense in depth) and owns sandbox lifecycle.
- **sandbox-router** and **AIO pods** are ClusterIP-only; never public.
- **Keycloak** is the single source of identity; tokens are validated against
  its JWKS at both the gateway and the broker.

## Authentication & authorization — swim lane

Two phases: a one-time **OAuth handshake** (browser), then **per-request**
token validation and authorization on every MCP call.

```
 Claude Code        agentgateway            Keycloak              broker              router/AIO
     │                   │                      │                    │                    │
     │ ── A. FIRST CONNECT / OAUTH (once) ───────────────────────────────────────────────│
     │                   │                      │                    │                    │
     │ POST /mcp (no tok)│                      │                    │                    │
     │──────────────────▶│                      │                    │                    │
     │ 401 + WWW-Authenticate:                  │                    │                    │
     │   resource_metadata=…                    │                    │                    │
     │◀──────────────────│                      │                    │                    │
     │ GET /.well-known/oauth-protected-resource│                    │                    │
     │──────────────────▶│                      │                    │                    │
     │ { authorization_servers:[Keycloak] }     │                    │                    │
     │◀──────────────────│                      │                    │                    │
     │ discover + browser login (auth-code+PKCE, client_id=aio-sandbox-client)            │
     │─────────────────────────────────────────▶│  user logs in;     │                    │
     │                   │                      │  in sandbox-users? │                    │
     │ access token (JWT: sub, preferred_username, groups=[sandbox-users], aud=…)         │
     │◀─────────────────────────────────────────│                    │                    │
     │                   │                      │                    │                    │
     │ ── B. EVERY MCP REQUEST (per call) ───────────────────────────────────────────────│
     │                   │                      │                    │                    │
     │ POST /mcp         │                      │                    │                    │
     │  Authorization:   │                      │                    │                    │
     │  Bearer <JWT>     │                      │                    │                    │
     │──────────────────▶│                      │                    │                    │
     │                   │ validate JWT:        │                    │                    │
     │                   │  iss/aud/exp + sig ──▶│ (JWKS, cached)     │                    │
     │                   │◀─────────────────────│                    │                    │
     │                   │ [authz: optional     │                    │                    │
     │                   │  mcpAuthorization CEL │                    │                    │
     │                   │  on tool name/claims] │                    │                    │
     │                   │ passthrough SAME JWT  │                    │                    │
     │                   │─────────────────────────────────────────▶│                    │
     │                   │                      │  re-validate JWT    │                    │
     │                   │                      │  (JWKS) ───────────▶│ (Keycloak)         │
     │                   │                      │  assert azp==aio-sandbox-client          │
     │                   │                      │  AUTHZ: groups has  │                    │
     │                   │                      │   sandbox-users?    │                    │
     │                   │                      │   else 403          │                    │
     │                   │                      │  principal =        │                    │
     │                   │                      │   preferred_username│                    │
     │                   │                      │                     │ claim/reuse sandbox│
     │                   │                      │                     │  (per session id)  │
     │                   │                      │                     │ forward tools/call │
     │                   │                      │                     │  + X-Sandbox-* ───▶│ run in pod
     │                   │                      │                     │◀───────────────────│ result
     │ result ◀───────────────────────────────────────────────────────────────────────────│
```

### Authentication — who you are
- **Inbound (client → agentgateway):** MCP OAuth 2.0. On `401`, the client
  follows `WWW-Authenticate` → `/.well-known/oauth-protected-resource` →
  `authorization_servers` = Keycloak, then runs authorization-code + PKCE in a
  browser. Token is cached by Claude Code (OS keychain). agentgateway validates
  `iss`, `aud`, `exp`, and signature against Keycloak's JWKS on every request.
- **Gateway → broker:** `backendAuth: passthrough` — the **same user JWT** is
  forwarded unchanged. No OBO, no token exchange, no service token. This is why
  the broker sees the real end user.
- **At the broker (resource server):** re-validates the JWT against JWKS
  (independent of the gateway), and asserts `azp == aio-sandbox-client` to
  confirm the token came through the expected login client.

### Authorization — what you may do
- **Coarse, today:** the broker requires the JWT's `groups` claim to contain
  `sandbox-users` (`AIO_REQUIRED_GROUP`), else `403`. Membership is managed in
  Keycloak. This gates *whether you can get a sandbox at all*.
- **Fine-grained, planned:** agentgateway's `mcpAuthorization` (CEL) can gate
  *individual tools* on JWT claims — e.g. only `sandbox-power` may call
  `sandbox_execute_bash`, or require `scope` `sandbox:browser` for `browser_*`.
  Enforced at the gateway, before the broker.

### Identity → sandbox mapping
- The broker derives the **principal** from `preferred_username` (falls back to
  `email`/`sub`).
- A `SandboxClaim` is labelled with both the **principal** and the **MCP
  session id**. One MCP session ⇒ one sandbox (reused across calls). A new
  session for the same user ⇒ another sandbox — so a user can have **N concurrent
  sandboxes**, while different users are isolated by principal. Per-user quota is
  a future policy check over the principal label.

## Request lifecycle inside the broker

1. **`initialize`** is answered **locally and instantly** (no AIO round trip),
   so the gateway's connect timeout never fires on the handshake.
2. First real **`tools/call`** (or `tools/list`): resolve the session's sandbox
   by label; if none, claim a template-backed, warm-pool-adopted sandbox and
   label it. Claims are template-backed so a **headless Service** is created and
   the router can reach the pod.
3. Forward the MCP request to `sandbox-router` with `X-Sandbox-ID/Namespace/Port`
   headers; stream the response (JSON or SSE) back unchanged so AIO tools pass
   through verbatim.
4. **`DELETE`** releases the session's sandbox; the claim TTL is the backstop.

The broker offloads blocking SDK/JWKS work to a threadpool (single uvicorn
worker stays responsive) and caches+retries JWKS (tolerates transient DNS).

## Why agentgateway (not AWS AgentCore Gateway)

AgentCore Gateway could not propagate end-user identity to a *transparent,
live-introspection* MCP target without 3LO + a static tool schema + interactive
consent. agentgateway does it with one line (`backendAuth: passthrough`), keeps
live tool passthrough and native MCP sessions, runs on the cluster, and avoids
cloud lock-in. Full analysis: `docs/POSTMORTEM-agentcore-vs-agentgateway.md`.

## Failure modes & mitigations (learned in practice)

| Symptom | Cause | Mitigation |
|---|---|---|
| Intermittent 504 / "Temporary failure in name resolution" | A CoreDNS replica that is `Ready` but a network black hole | Delete the bad pod (dig each pod IP to find it); NodeLocal DNSCache for durability |
| Broker liveness restarts mid-call | Blocking SDK calls stalled the single uvicorn loop | `run_in_threadpool` for blocking calls; tolerant `/healthz` probes |
| Broker/sandbox pods disrupted | Karpenter node consolidation | `karpenter.sh/do-not-disrupt` on the pods + SandboxTemplate |
| Handshake timeout at the gateway | Broker forwarded `initialize` to the slow AIO hub | Broker answers `initialize` locally; claims lazily |
| 504 reusing a sandbox | Stale claim points at a wedged sandbox | Reap by TTL; (planned) readiness re-check before reuse |
