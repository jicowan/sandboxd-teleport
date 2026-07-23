# delete_me — quarantined dead code (pending removal)

These files belong to the **old agent-sandbox integration**, which is no longer
used. They are parked here for a grace period before deletion, not kept as
reference.

## What this was

An earlier version of the broker (`broker_mcp.py`) fronted the
[kubernetes-sigs **agent-sandbox**](https://github.com/kubernetes-sigs/agent-sandbox)
project — a *different* sandbox backend — creating `SandboxClaim`s and proxying to
agent-sandbox's own `sandbox-router`. That path has been superseded by **sandboxd**
(this repo's session‑teleport control plane) with its own reference front door
(`broker/broker_sandboxd.py`).

As of 2026‑07‑22 the old `aio-sandbox-broker` Deployment still exists on the live
cluster but is **orphaned** — the `aio-sandbox-broker-svc` Service selector points
at the sandboxd broker, so no traffic reaches it. (Scale it to 0 / delete it when
convenient.)

## Contents

| Moved from | What it was |
|------------|-------------|
| `broker/broker_mcp.py` | The agent-sandbox broker (creates `SandboxClaim`s). |
| `broker/Dockerfile` | Built `broker_mcp.py` (the sandboxd broker uses `Dockerfile.sandboxd`). |
| `broker/requirements.txt` | Deps for `broker_mcp.py` (sandboxd uses `requirements-sandboxd.txt`). |
| `deploy/20-broker.yaml` | Deployed the agent-sandbox broker. |
| `deploy/10-router-clusterip.yaml` | agent-sandbox's `sandbox-router` (NOT the sandboxd router). |
| `deploy/00-serviceaccount-rbac.yaml` | RBAC granting the broker `SandboxClaim`/`sandboxes` access (agent-sandbox CRDs). |

### Retired Stage-1/Stage-2 + AgentCore‑Gateway design docs

These describe an abandoned front‑door track (AWS Bedrock **AgentCore Gateway** +
OBO/3LO token exchange, and the Stage‑1/2 `broker_mcp.py`/`proxy` era). The live front
door is **agentgateway** (OSS) with `backendAuth: passthrough` — see the current
`docs/POSTMORTEM-agentcore-vs-agentgateway.md` and `docs/sandboxd/`. Kept only as
history.

| Moved from | What it was |
|------------|-------------|
| `docs/adr/0001-no-agentcore-gateway.md` | ADR: don't use AgentCore Gateway for the (Stage‑1) broker. |
| `docs/adr/0002-gateway-for-stage2-frontdoor.md` | ADR: use AgentCore Gateway as the Stage‑2 front door (OBO/RFC 8693). This decision was itself reversed — the front door is agentgateway, not AgentCore. |
| `docs/STAGE2.md` | Stage‑2 design map (AgentCore + OBO + `broker_mcp`). |
| `docs/DESIGN.md` | Early overall design (Stage‑1/2 era). |
| `docs/3LO-PLAN.md` | Per‑user identity via 3LO through AgentCore Gateway — obviated by agentgateway JWT passthrough. |
| `docs/POSTMORTEM-agentcore-vs-agentgateway.md` | The evaluation that chose agentgateway over AgentCore Gateway. Its conclusion is now just *the architecture* (see `docs/DESIGN-agentgateway.md` + `docs/sandboxd/`), so it's kept only as the historical rationale. |

(The `docs/adr/` directory was removed — both ADRs live here now.)

## NOT moved (still live / shared)

- `deploy/00-keycloak-realm.yaml` — shared auth (Keycloak realm), used by the sandboxd front door.
- `deploy/30-agentgateway.yaml` — the **live** front door (multi-app routes → the sandboxd broker).
- `deploy/40-agentgateway-ingress.yaml` — live public ingress for agentgateway.

The current sandboxd reference front door lives in `broker/broker_sandboxd.py` +
`broker/Dockerfile.sandboxd` and its deploy manifests under
`checkpoint-restore/controlplane/deploy/aio/` (+ the shared `deploy/00`/`30`/`40`).
