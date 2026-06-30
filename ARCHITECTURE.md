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
     │                   │ AUTHZ (mcpAuthz CEL): │                    │                    │
     │                   │  is mcp.tool.name     │                    │                    │
     │                   │  allowed for          │                    │                    │
     │                   │  jwt.groups? else     │                    │                    │
     │                   │  filter / deny tool   │                    │                    │
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
     │                   │                      │  quota: claims <    │                    │
     │                   │                      │   MAX? else 429     │                    │
     │                   │                      │                     │ claim/reuse sandbox│
     │                   │                      │                     │ (session+principal)│
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

Authorization happens at **two** layers — *may you get a sandbox at all*
(broker) and *which tools you may call* (gateway):

**1. Can you create / use a sandbox? (broker)**
- **Group gate:** the broker requires the JWT's `groups` claim to contain
  `sandbox-users` (`AIO_REQUIRED_GROUP`), else `403`. Membership is managed in
  Keycloak.
- **Per-user quota:** before claiming a *new* sandbox, the broker counts the
  caller's existing claims (by `principal` label) and returns `429` once it
  reaches `AIO_MAX_SANDBOXES_PER_USER` (default `3`). Because the broker holds
  the only RBAC to create claims, this can't be bypassed by opening more MCP
  sessions.

**2. Which tools may you call? (gateway — `mcpAuthorization`)**
- agentgateway evaluates CEL `rules` (OR'd) against the passed-through JWT on
  every `tools/call`, and **also filters `tools/list`** so unauthorized tools
  are invisible. Tiers:
  - `sandbox-power` → every tool, including `sandbox_execute_bash` /
    `sandbox_execute_code`.
  - `sandbox-users` (standard) → `browser_*` + non-exec `sandbox_*` (files,
    editor, markdown, context, packages, skill loader). Exec tools are filtered
    out of `tools/list` and rejected on call (`-32602 Unknown tool`).
- Verified live: a standard-tier user sees 30 tools (no exec); a power-tier
  user sees 32 and can run shell. Rules live in `deploy/30-agentgateway.yaml`.

### Identity → sandbox mapping (multi-tenancy)
- The broker derives the **principal** from `preferred_username` (falls back to
  `email`/`sub`).
- A `SandboxClaim` is labelled with both the **principal** and the **MCP
  session id**. One MCP session ⇒ one sandbox (reused across calls). A new
  session for the same user ⇒ another sandbox — so a user can have **N concurrent
  sandboxes**, while different users are isolated by principal.
- **Tenant isolation:** the session→claim lookup is scoped by **both**
  `session-id` AND `principal` labels, so a session id (even if guessed or
  leaked) can never resolve to another user's sandbox. Sandboxes are never
  shared across principals; the router dispatches each request to a single
  addressed pod by `X-Sandbox-ID`.
- **Per-user quota:** `N` is capped at `AIO_MAX_SANDBOXES_PER_USER` (default 3)
  — the broker counts the principal's claims before creating a new one and
  returns `429` past the cap.

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
