# ADR 0001 — Do not use AWS Bedrock AgentCore Gateway (for now)

Status: Accepted
Date: 2026-06-29

## Context

The cluster runs the `mcp-gateway-operator` (`mcpgateway.bedrock.aws/v1alpha1`,
`MCPServer` CRD), which provisions AWS Bedrock AgentCore Gateways. An
`MCPServer` CR gives you, as managed AWS infrastructure:

- a remote MCP front door with **inbound OAuth** (clients authenticate to the
  gateway), and
- **outbound credential injection** to the backend MCP server (via a token
  vault `oauthProviderArn`).

Two live instances exist on this cluster (`alphavantage-stocks`,
`mcp-example`), wired to the Keycloak `agentcore` realm's inbound/outbound
clients.

The question was whether to put the AIO sandbox behind the Gateway as part of
the broker redesign.

## Decision

Do not use the AgentCore Gateway for the sandbox broker. Build the broker
behind the existing ALB + oauth2-proxy + Keycloak stack instead.

## Rationale

- **It doesn't solve the core pains.** The two problems driving this work are
  "client needs the SDK" and "client needs cluster RBAC." The Gateway solves
  neither; only the broker does. The Gateway addresses "eliminate the local
  proxy," which the broker + remote-MCP-over-OAuth also handles.
- **We already own the front door.** ALB + ACM TLS + oauth2-proxy inbound JWT
  validation against Keycloak are built and working. The Gateway would
  *replace* working infrastructure with a managed equivalent — a lateral move,
  not simplification.
- **Avoids a hard cloud dependency.** The current stack is portable
  (Kubernetes + Keycloak + an ALB) and could run on any cloud or on-prem. The
  Gateway pins the design to Bedrock AgentCore in a specific AWS region, adds
  per-gateway cost, and introduces AWS-side quotas/throttling we don't control.
- **Opaque hop in the data path.** It is unverified whether the Gateway
  forwards MCP session semantics (`Mcp-Session-Id`, `DELETE`) through to the
  target. If it does not, the "session = claim" model breaks and we'd build
  session affinity anyway — so the Gateway removes none of the hard work.

## Consequences

- We build and operate the broker ourselves (its RBAC, HA, and place in the
  data path).
- We may need a few hundred lines of remote-MCP-OAuth discovery glue (resource
  metadata / dynamic client registration) for Stage 2, depending on how well
  Claude Code's `--transport http` negotiates with a raw oauth2-proxy.

## When to revisit

Reconsider the Gateway if **Bedrock agents** (not Claude Code) become
first-class clients of the sandbox. At that point registering the broker as an
`MCPServer` target is a small addition, not a rewrite — the broker stays; the
Gateway becomes an additional front door for AWS-native consumers.
