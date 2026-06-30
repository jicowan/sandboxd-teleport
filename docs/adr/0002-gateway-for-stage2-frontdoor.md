# ADR 0002 — Use AgentCore Gateway as the Stage 2 MCP front door

Status: Accepted
Date: 2026-06-29
Supersedes (partially): [ADR-0001](./0001-no-agentcore-gateway.md)

## Context

ADR-0001 rejected AgentCore Gateway for Stage 1 because it solved neither of
the two pains (client SDK, client RBAC) — the broker did — while adding a
cloud dependency and an opaque hop.

Stage 2's goal is to drop the local `aio_proxy.py` and let Claude Code connect
to a remote MCP endpoint with native OAuth. That deliverable — an OAuth2 MCP
front door doing RFC 9728 discovery + PKCE with the client and round-tripping
MCP sessions — *is* the Gateway's core function. Building it ourselves means a
from-scratch OAuth resource server whose discovery must exactly satisfy Claude
Code's client, plus MCP session plumbing.

The blocker we investigated: does the end-user identity survive the Gateway so
the broker can still key claims per-user and enforce the `sandbox-users` group?

## Decision

Use AgentCore Gateway as the Stage 2 front door, with **outbound auth =
OAuth2 Token Exchange / On-Behalf-Of (OBO, RFC 8693)** rather than
client-credentials. The broker remains as the Gateway's MCP-server target and
keeps owning lifecycle.

## Rationale

- The Gateway provides exactly Stage 2's hard part (client OAuth + MCP
  transport + session management) as managed infrastructure.
- **OBO preserves user identity (verified).** With client-credentials the
  backend would see only the gateway's M2M principal — but OBO is supported
  for MCP-server targets and the exchanged token carries the user's
  sub/preferred_username/groups. Confirmed against Keycloak 26.5.6: the
  exchange via `sandbox-gateway-outbound` yields a downstream token still
  carrying `preferred_username: jicowan` and `groups: ['sandbox-users']`. So
  per-user keying and the group gate survive — the thing ADR-0001 worried we'd
  lose does not, in fact, get lost.
- Tool discovery stays transparent (live proxying to the target); the earlier
  "DYNAMIC vs 3LO/OBO incompatibility" was a conflation — AWS documents no
  incompatibility between OBO and semantic search.

## Scope of the reversal

This reverses ADR-0001 **only for the front door**. The broker is unchanged in
purpose (lifecycle owner) and is not replaced. ADR-0001's portability concern
still stands as a tradeoff: Stage 2 introduces a hard Bedrock/AWS dependency on
the data path.

The Stage-1 broker + local proxy remain in the repo (`broker.py`,
`proxy/aio_proxy.py`) as a portable, Gateway-free design — but as of the Stage-2
cutover they are **not deployed/live**. Standing up Stage 2 required:

- removing the router's oauth2-proxy sidecar and rebinding it to `0.0.0.0`,
- deleting the public `sandbox-router` Ingress (router is now ClusterIP-only),
- deleting the `sandbox-router.jicomusic.com` R53 record and its ACM cert.

So the Stage-1 path (local proxy → public router ALB, authenticated by the
router sidecar) no longer functions as-is. Reactivating the fallback means
re-applying the Stage-1 router posture (re-add the sidecar via the
agent-sandbox bundle's `auth/20-router-deployment-patch.yaml`, rebind to
loopback, recreate the public Ingress + cert + DNS). The *code* is the
fallback; the *running infrastructure* is single-track on Stage 2.

## Consequences

- We operate one new managed dependency (the Gateway) and the OBO credential
  provider (token vault).
- Keycloak needs the `sandbox-gateway-outbound` confidential client
  (`standard.token.exchange.enabled=true`) and an audience mapper adding it to
  user-token `aud` (v2 standard exchange requires the actor be in the subject
  token's audience).
- The broker becomes an OAuth2 resource server validating the OBO token
  (one issuer/audience; assert `azp == sandbox-gateway-outbound`).

## When to revisit

If the AWS dependency becomes a problem (multi-cloud, on-prem, cost), fall back
to the Stage-1 broker + local proxy. The code is intact, but reactivation is
not zero-touch — it requires restoring the Stage-1 router posture (sidecar +
loopback bind + public Ingress + cert + DNS), as noted under "Scope of the
reversal".
