# AIO Sandbox Broker — Design

## Problem

The current architecture (in `agent-sandbox/examples/aio/aio-sandbox-bundle/`)
puts cluster authority and session lifecycle on the **client**:

1. The client runs `claim.py` / `release.py`, which require the
   `k8s-agent-sandbox` SDK installed locally.
2. Those scripts need a kubeconfig and **RBAC to create `SandboxClaim` CRs**
   on the cluster — distributed to every laptop.
3. A local `aio_proxy.py` translates stdio MCP ↔ HTTP and injects auth +
   `X-Sandbox-*` headers, reading lifecycle state from a shared
   `~/.config/aio-sandbox/current.json` file.

This means: broad RBAC blast radius, no server-side policy enforcement
(quotas, allowed templates, per-user limits), fragile client-driven cleanup,
and three local moving parts (SDK scripts, state file, proxy) that must stay
in sync.

## Goal

Move lifecycle and cluster authority **server-side** into a single
in-cluster **broker**. The client becomes an OAuth-authenticated MCP caller
with no SDK, no kubeconfig, no RBAC, and (Stage 2) no local proxy.

We are explicitly **not** using AWS Bedrock AgentCore Gateway. See
[ADR-0001](./adr/0001-no-agentcore-gateway.md). The existing ALB +
oauth2-proxy + Keycloak stack already provides the remote front door and
inbound OAuth; the Gateway would add a cloud dependency and an opaque hop
without solving the two core pains (client SDK + client RBAC), which only
the broker solves.

## Target architecture

```
Claude Code  --HTTP MCP + OAuth-->  ALB (ACM TLS)
                                       |
                                       v
                              oauth2-proxy  (verifies JWT vs Keycloak JWKS)
                                       |  forwards Authorization + X-Auth-Request-*
                                       v
                              sandbox broker            <-- the only new component
                                 |  - ServiceAccount: SandboxClaim CRUD
                                 |  - identity from forwarded JWT claims
                                 |  - policy: per-user quota, allowed templates
                                 |  - MCP session-id  <->  SandboxClaim (label)
                                 |  - initialize = claim, DELETE/idle = release
                                 v
                          sandbox-router  -->  AIO pod :8080/mcp
```

Everything left of the broker already exists and is unchanged. The broker is
the only thing we build.

## The broker's responsibilities

1. **Identity.** oauth2-proxy validates the JWT and forwards it plus
   `X-Auth-Request-User` / `X-Auth-Request-Email`. The broker trusts these
   (it is only reachable through the sidecar) and uses them as the caller
   principal.
2. **Lifecycle.** Create a `SandboxClaim` (or draw from a warmpool) on
   session start; delete it on session end. This is the logic currently in
   `claim.py` / `release.py`, relocated and given a ServiceAccount instead of
   the user's kubeconfig.
3. **Policy.** Enforce per-principal limits the client could never enforce
   for itself: max concurrent sandboxes, allowed `SandboxTemplate`s, TTL
   ceilings.
4. **Forwarding.** Inject the `X-Sandbox-*` headers (the broker now owns the
   claim→pod mapping) and forward MCP traffic to the router → AIO pod.

## Session model (Stage 2 — the elegant version)

Streamable-HTTP MCP already has session semantics that map cleanly onto a
claim lifecycle:

| MCP event | Broker action |
|---|---|
| `initialize` (no `Mcp-Session-Id`) | Create `SandboxClaim`, label it `mcp-session-id=<id>` and `principal=<sub>`, return `Mcp-Session-Id` |
| subsequent calls (with session id) | Look up claim by label, resolve pod from `status.sandbox`, forward |
| `DELETE` (client teardown) | Delete the claim |
| idle timeout / claim TTL | Backstop release |

Because the session→pod mapping is **stored as labels on the
`SandboxClaim`**, the broker is effectively stateless: on restart it
reconstructs the map with a single `LIST SandboxClaim` by label. Run 2+
replicas for HA.

**Open question (must verify before Stage 2):** does the remote MCP transport
the client uses preserve `Mcp-Session-Id` and the `DELETE` across the ALB and
oauth2-proxy to the broker? If a hop drops or rewrites the session header, the
broker needs an alternate session key (e.g. derived from a stable JWT claim
like `sid`, or its own cookie). Test end-to-end before relying on it.

## Staging

### Stage 1 — Broker owns lifecycle (kills client SDK + RBAC) — BUILT

Smallest step that solves both stated pains. Keeps the local `aio_proxy.py`
for MCP forwarding; the proxy now claims via the broker instead of reading a
`current.json` written by the SDK.

- Broker (`broker/broker.py`, FastAPI, SDK-only) exposes:
  - `POST /sessions`  → claims a sandbox for the caller (`session_id` ==
    `SandboxClaim` name), returns `{session_id, sandbox_id, namespace,
    container_port, expires_at, principal}`.
  - `DELETE /sessions/{id}` → releases it (idempotent).
  - `GET /sessions/{id}` → status.
- Broker runs with a ServiceAccount scoped to `SandboxClaim` CRUD +
  `Sandbox` read (`deploy/00-serviceaccount-rbac.yaml`). **The client loses
  the kubeconfig, the SDK, and the create-claim RBAC entirely.**
- Identity comes from the oauth2-proxy sidecar's `X-Auth-Request-*` headers;
  the `--allowed-group=sandbox-users` gate (coarse authz) runs at the sidecar
  before the broker.
- `aio_proxy.py` calls `POST /sessions` lazily on first tool use (cached per
  process), using the same OIDC bearer the router uses. It no longer depends
  on `claim.py`/`release.py` or `current.json` when `AIO_BROKER_URL` is set.

Status: code + manifests built and import/dry-run validated. Not yet
deployed end-to-end (needs broker image build/push, ACM cert for
`broker.jicomusic.com`, R53 record).

**Known gotcha — principal identity:** the test user `jicowan` has no `email`
claim in its token, so oauth2-proxy's `X-Auth-Request-Email` is empty and the
broker's `_principal()` falls back to `X-Auth-Request-User` (a UUID). For
readable principals, either set an email on the Keycloak user or have the
broker prefer `preferred_username`. Cosmetic in Stage 1 (principal isn't used
for authz yet); revisit when broker-side policy (finest authz) lands.

### Stage 2 — Drop the local proxy

Teach the broker MCP session semantics (table above), have it forward MCP
itself, and advertise OAuth resource metadata so a remote MCP client can
authenticate directly. Then the client registration collapses to:

```
claude mcp add --transport http aio-sandbox https://sandbox-router.jicomusic.com/mcp
```

The local proxy container, `aio_login.py`, the device flow, the `oidc.json`
cache, and `current.json` all go away. Client footprint = Claude Code + one
browser login.

## Authorization (who can claim)

Authentication is not authorization. A valid `sandbox`-realm JWT proves
identity; it does not by itself grant the right to claim a sandbox.

**Stage 1 (implemented — coarse, group-gated):**
- A Keycloak group `sandbox-users` exists in the realm.
- The `sandbox` client scope emits a `groups` claim on the access token
  (via an `oidc-group-membership-mapper`).
- Both oauth2-proxy sidecars (router and broker) enforce
  `--allowed-groups=sandbox-users`. A token from a user not in the group is
  rejected with 403 **at the sidecar, before the broker runs**.
- Access is managed entirely in Keycloak: add/remove users from the group.
  No code, no redeploy.

**Later (planned — fine-grained, broker-side policy):**
When there are multiple templates or a need for quotas, move policy into the
broker. The broker already receives the full JWT
(`--pass-authorization-header=true`) and parsed claims
(`X-Auth-Request-Groups` / `X-Auth-Request-Email`). At that point the broker
becomes a proper OAuth resource server (verifies the JWT signature against
Keycloak JWKS itself, rather than trusting forwarded headers for security
decisions) and can enforce:
- per-group template selection (e.g. `sandbox-power-users` → GPU template),
- per-principal concurrent-sandbox quotas (the quota we deliberately dropped
  from Stage 1),
- attribute-based limits from custom token claims (e.g. `max_sandboxes`,
  `allowed_templates`).
This is the natural home for the "extend the sandbox API after user
feedback" work. Tracked as the **finest** authorization tier; the group gate
above is the **coarsest** tier and ships first.

## What the broker does NOT do (non-goals for Stage 1/2)

- Per-claim authorization at the router (any in-group token can still address
  any sandbox the router routes to — unless we add a broker-signed,
  router-verified claim token).
- Fine-grained authorization (templates/quotas) — planned, see above.
- Rate limiting beyond what the ALB provides.
- Multi-cluster / multi-region.

## Component inventory (what's reused)

| Component | State | Source |
|---|---|---|
| ALB + ACM + R53 | exists | `agent-sandbox` bundle `auth/40-router-ingress.yaml` |
| oauth2-proxy sidecar | exists | `auth/20-router-deployment-patch.yaml` |
| Keycloak `sandbox` realm + `aio-sandbox-client` client | exists | `auth/00-keycloak-sandbox-realm.yaml` |
| sandbox-router | exists, unchanged | upstream `agent-sandbox` |
| **sandbox broker** | **to build** | this repo, `broker/` |
| lifecycle logic | port from `claim.py`/`release.py` | this repo, `broker/` |

## Repo layout (this repo)

```
aio-sandbox/
  docs/
    DESIGN.md            # this file
    adr/
      0001-no-agentcore-gateway.md
  broker/                # the broker service (Stage 1: REST; Stage 2: + MCP)
  deploy/                # k8s manifests (Deployment, SA/RBAC, Service)
  skill/                 # thin client skill (Stage 1: calls broker REST)
```

## Resolved decisions (2026-06-29)

1. **Language: Python for Stage 1.** Reuses the proven `claim.py`/`release.py`
   logic; swaps the local kubeconfig for an in-cluster ServiceAccount. Revisit
   Go for Stage 2 when the broker becomes a real MCP forwarder in the data
   path.
2. **Claim strategy: always template-backed, warmpool-adopted.** The broker
   calls `create_sandbox(template=..., warmpool=<pool>, shutdown_after_seconds=...)`.
   The SDK puts **both** `sandboxTemplateRef` and `warmpool` in the claim
   spec, so:
   - The claim always references the `SandboxTemplate` → the headless Service
     is created → the sandbox is routable by the sandbox-router. **Never
     create a bare `Sandbox`** (no template ⇒ no service ⇒ unroutable).
   - `warmpool=<pool>` tells the controller to adopt a ready pod from
     `aio-sandbox-warmpool` when one exists, falling back to a fresh
     cold-start when the pool is empty. The broker does **not** need to count
     unclaimed pods; the controller handles adoption-or-create.
3. **Sandbox identity:** in the warmpool case the sandbox id != claim name.
   Use the SDK's `resolve_sandbox_name(claim_name, ...)` (it reads
   `SandboxClaim.status.sandbox`) to get the real id for the `X-Sandbox-*`
   headers.
4. **Idle detection: deferred to a later phase.** For Stage 1, the claim TTL
   (`shutdown_after_seconds`) is the sole reclamation mechanism — the
   controller auto-deletes the claim on expiry (`shutdownPolicy: Delete`).
   `DELETE /sessions/{id}` is the happy-path explicit release.

## Open question (blocks Stage 2 only)

- **MCP session forwarding** through ALB + oauth2-proxy: does the remote
  transport preserve `Mcp-Session-Id` and the `DELETE` end-to-end to the
  broker? Test before designing Stage 2's session=claim mapping. Not needed
  for Stage 1.
