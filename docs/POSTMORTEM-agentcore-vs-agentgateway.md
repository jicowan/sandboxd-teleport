# Post-mortem: why AgentCore Gateway failed this use case, and why agentgateway works

## Context

We need a remote MCP front door for a per-session AIO sandbox: a client
(Claude Code) authenticates with a Keycloak JWT, the gateway forwards MCP to
an in-cluster **broker** that claims/releases sandboxes and proxies tool calls
to the sandbox-router → AIO pod. The hard requirement that drove most of this
investigation: **the broker must learn the end-user's identity** (to key
sandboxes per user and enforce the `sandbox-users` group), not just the
gateway's service identity.

We evaluated two gateways: **AWS Bedrock AgentCore Gateway** (managed) and
**agentgateway** (Linux Foundation OSS, https://github.com/agentgateway/agentgateway).
AgentCore could not satisfy the use case without unacceptable tradeoffs;
agentgateway did, in one line of config.

## The core requirement that broke AgentCore: identity propagation to a *live* MCP target

AgentCore Gateway forwards to a target using an outbound credential whose
grant type is one of `CLIENT_CREDENTIALS`, `AUTHORIZATION_CODE`, or
`TOKEN_EXCHANGE` (OBO). For our **transparent, live-introspection MCP-server
target**, none of them works cleanly:

| Outbound grant | Carries end-user identity? | Result for a live MCP-server target |
|---|---|---|
| `CLIENT_CREDENTIALS` | No — gateway's M2M identity only | Works, but the broker only ever sees the gateway service account. No per-user keying, no group gate. |
| `TOKEN_EXCHANGE` (OBO) | Yes (verified at Keycloak) | **Target FAILS at creation.** A live MCP-server target runs `tools/list` introspection at create time, when **no user is present** — so OBO has no `subject_token` to exchange, can't mint an outbound token, and can't connect. Verified twice, on both MCP 2025-03-26 and 2025-11-25 gateways. |
| static `mcpToolSchema` + OBO | — | **API-rejected:** "mcpToolSchema is only supported for MCP Server targets with AUTHORIZATION_CODE grant type." |
| static schema + `AUTHORIZATION_CODE` (3LO) | Yes, per-user | **Viable but stacked with costs** (below). |

### The fundamental tension

To avoid the creation-time-introspection failure you must declare a **static
tool schema** — which the API only permits with **3LO**. And 3LO means an
**interactive browser consent** on the outbound leg. So for a transparent live
MCP backend you cannot simultaneously have (a) no creation-time-user failure
and (b) non-interactive per-user identity. You get either gateway-level
identity (CLIENT_CREDENTIALS) or interactive-consent per-user identity (3LO),
never transparent + non-interactive + per-user.

### The stack of incidental blockers we also hit on the 3LO path

Each was individually fixable, but together they show how much friction the
managed path carried for a non-Cognito, transparent-MCP use case:

1. **Stale CLI model** hid the `TOKEN_EXCHANGE` enum value (needed CLI ≥ 2.35).
2. **MCP version gate**: 3LO requires a gateway created on MCP `2025-11-25`;
   the version isn't updatable on an existing gateway — full recreate.
3. **`MCP-Protocol-Version` header** required on tool calls or the gateway
   rejects with "Unsupported MCP protocol version: 2025-03-26".
4. **Tool namespacing**: tools are exposed as `<target>___<tool>`.
5. **IAM**: the gateway role lacked `bedrock-agentcore:GetWorkloadAccessTokenForJWT`;
   only discoverable by turning on the gateway's vended CloudWatch logs.
6. **Missing `sub` claim**: AgentCore's `GetWorkloadAccessTokenForJWT` rejects
   an inbound token with no `sub` ("Subject claim is missing"); our device-flow
   access tokens omitted it until we added a sub-mapper.
7. **Provider/target grant mismatch**: OBO config on the provider + 3LO on the
   target produced an unrecoverable interactive-consent loop (the login page
   would not complete the grant), because the two flows aren't meant to combine.
8. **Entra-shaped docs**: the working AWS sample and `requested_token_use:
   on_behalf_of` are Microsoft Entra patterns; Keycloak behaves differently,
   and AgentCore's OBO machinery is tuned to Entra's response shape.

The token exchange itself was never the problem — Keycloak performed the RFC
8693 exchange correctly every way we tried it, returning a downstream token
carrying `preferred_username`/`groups`. The problem was always **AgentCore's
constraints on *when* and *how* it would drive that exchange for a transparent
MCP target.**

## Why agentgateway works

agentgateway is a proxy, not a managed tool-registry. It connects to the MCP
backend **per request**, with the user already present — so the entire
"no-user-at-creation" failure mode does not exist. Identity propagation is a
single documented policy:

```yaml
policies:
  mcpAuthentication:        # validate the inbound Keycloak JWT (+ serve OAuth metadata)
    issuer: https://keycloak.example.com/realms/sandbox
    jwks: { url: .../protocol/openid-connect/certs }
    provider: { keycloak: {} }
  backendAuth:
    passthrough: {}         # forward the SAME validated user JWT to the broker
```

With `backendAuth: passthrough`, the broker receives the **original user
bearer token** unchanged. Proven end-to-end — the broker logged:

```
[token] sub=7613ff43-... preferred_username=jicowan groups=['sandbox-users']
```

That is the exact per-user identity OBO/3LO could not deliver — achieved with
no token exchange, no consent, no IAM workload-identity, no MCP-version gate,
no static schema. The broker already validates JWTs as a resource server, so
passthrough slots straight in.

agentgateway also gave us, for free:
- **MCP session management** — issues `mcp-session-id` on initialize and routes
  subsequent calls (stateful session affinity). We had hand-rolled this in the
  broker and it kept breaking under AgentCore.
- **Live MCP backends** — no static-schema requirement.
- **Keycloak as a first-class IdP**, EKS-native (Gateway API), no AWS lock-in,
  removing the portability concern from the original design.

## Scorecard

| Capability | AgentCore Gateway | agentgateway |
|---|---|---|
| Per-user identity to a transparent live MCP backend | ✗ (only via 3LO + static schema + consent) | ✓ `backendAuth: passthrough` |
| Live tool passthrough (no static schema) | ✗ with identity | ✓ |
| Non-interactive (no consent loop) | ✗ on the identity path | ✓ |
| MCP session management | partial / version-gated | ✓ native, stateful |
| IdP flexibility (Keycloak) | tuned for Cognito/Entra | ✓ first-class Keycloak |
| Cloud lock-in | AWS/Bedrock | none (OSS, EKS) |
| Setup friction for this use case | high (8+ stacked blockers) | low (one policy) |

## Caveats / honest notes

- The remaining failures in the agentgateway spike (initialize timeout, tool-call
  504/500) were **not** agentgateway's fault — they were (a) the broker's slow
  forward-initialize to the AIO hub (fixed by answering `initialize` locally in
  the broker), and (b) **intermittent cluster DNS** (`Temporary failure in name
  resolution`) hitting the broker→Keycloak and broker→router hops. The DNS issue
  also degraded the AgentCore attempts; it is a CoreDNS reliability problem to
  address separately during the pivot.
- AgentCore Gateway is not "bad" — its OBO/identity model is built for
  declared-schema targets (OpenAPI/Lambda/static MCP) and Entra/Cognito. It is
  simply a poor fit for a **transparent, live-introspection MCP passthrough**
  with Keycloak, which is exactly our use case.

## Decision

Pivot the real design to **agentgateway** as the MCP front door, with
`backendAuth: passthrough` for per-user identity. Retire the AgentCore Gateway
path (spike infra deleted). See the pivot work for the production design.
