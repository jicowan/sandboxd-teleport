# aio-sandbox

A server-side **session broker** for running AIO Agent Sandboxes on Kubernetes
via the [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
framework, without putting cluster credentials or the sandbox SDK on the
client.

## Why

The predecessor design (in `agent-sandbox/examples/aio/aio-sandbox-bundle/`)
required every client to:

- install the `k8s-agent-sandbox` SDK,
- hold a kubeconfig with RBAC to create `SandboxClaim` CRs, and
- run a local stdio↔HTTP MCP proxy.

This broker moves lifecycle and cluster authority into the cluster. The client
becomes an OAuth-authenticated MCP caller — no SDK, no kubeconfig, no RBAC,
and (Stage 2) no local proxy.

See [docs/DESIGN.md](./docs/DESIGN.md) for the full architecture and
[docs/adr/0001-no-agentcore-gateway.md](./docs/adr/0001-no-agentcore-gateway.md)
for why we are not using AWS Bedrock AgentCore Gateway.

## Status

- **Stage 1 — DONE and deployed.** Broker owns lifecycle (`POST/GET/DELETE
  /sessions`), authenticated by the existing Keycloak JWT, authorized by the
  `sandbox-users` group at the oauth2-proxy sidecar. The client has no SDK,
  no kubeconfig, no SandboxClaim RBAC. The broker-aware `aio_proxy.py` claims
  via the broker on first tool use. Verified end-to-end against the live
  cluster. See [docs/DEPLOY.md](./docs/DEPLOY.md).
- **Stage 2 — planned.** Broker speaks MCP with session-id ↔ claim mapping
  and OAuth resource metadata; drops the local proxy entirely. Blocked on
  verifying MCP session forwarding through the ALB + oauth2-proxy (see
  DESIGN.md open question).

## Deploy

See **[docs/DEPLOY.md](./docs/DEPLOY.md)** for the full runbook (image build,
ACM, Route 53, Keycloak mappers, MCP registration) and the gotchas hit during
the first deploy.

## Layout

```
docs/      design + ADRs
broker/    the broker service
deploy/    k8s manifests (Deployment, ServiceAccount/RBAC, Service)
skill/     thin client skill (Stage 1: calls the broker's REST API)
```

## Relationship to agent-sandbox bundle

This repo reuses the cluster-side infrastructure built in the `agent-sandbox`
repo's `examples/aio/aio-sandbox-bundle/auth/` (ALB, oauth2-proxy sidecar,
Keycloak `sandbox` realm). It replaces that bundle's client-side pieces
(`claim.py`/`release.py` SDK scripts, `aio_proxy.py`) with the broker.
