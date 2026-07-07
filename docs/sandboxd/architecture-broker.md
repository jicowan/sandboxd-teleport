# Architecture — the auth front door (Keycloak, agentgateway, broker)

This document explains the three components that sit between an MCP client and
the sandboxd control plane: **Keycloak** (identity), **agentgateway** (the
authenticating MCP proxy + tool authorization), and the **broker** (session
derivation + forwarding). Together they turn an anonymous HTTPS request into an
authenticated, authorized, per‑user MCP session routed into a sandbox.

For the control plane on the other side of the broker (router, operator, Valkey,
workers), see [architecture-sandboxd.md](architecture-sandboxd.md).

## Design goals

- **One public MCP endpoint**, OAuth‑protected, that any compliant MCP client can
  use with no secret.
- **Identity is centralized** in an OIDC provider (Keycloak); the platform never
  stores passwords.
- **Tool‑level authorization** — standard users get safe tools; only power users
  get shell/code execution, and unauthorized tools are *invisible*, not just
  blocked.
- **Durable per‑user sessions** — a user maps to one stable sandbox session that
  teleports across workers, independent of which broker replica handles a request.
- **Defense in depth** — the token is verified at the edge (agentgateway) *and*
  re‑verified at the broker, so the broker is safe even if reached directly.

## Component roles

| Component | Kind | Responsibility |
|-----------|------|----------------|
| **Keycloak** | OIDC identity provider | Authenticates users; issues signed JWTs with audience, group, and username claims. Realm `sandbox`, public client `aio-sandbox-client`. |
| **agentgateway** | MCP‑aware reverse proxy (`cr.agentgateway.dev/agentgateway:v1.3.1`) | Terminates the MCP protocol at the edge; verifies the JWT (issuer/audience/signature); enforces a per‑tool authorization allowlist and filters unauthorized tools out of `tools/list`; forwards the untouched JWT to the broker. |
| **broker** (`broker_sandboxd.py`) | FastAPI MCP server | Re‑verifies the JWT; derives a durable session id from the user principal; warms the session on `initialize`; forwards MCP calls to the router with routing headers. |

## Request path

```
MCP client (Claude)
  │  Authorization: Bearer <user JWT>   (aud=sandbox-router, azp=aio-sandbox-client, groups=[sandbox-users|sandbox-power])
  ▼
https://agentgateway.jicomusic.com/mcp
  │  ALB Ingress "agentgateway" (internet-facing, TLS 443 → :3000)
  ▼
Service agentgateway-svc:3000  →  agentgateway pod
  │  1. mcpAuthentication (strict): verify iss + aud against Keycloak JWKS
  │  2. mcpAuthorization: allow/deny each tool; hide unauthorized tools
  │  3. backendAuth: passthrough — forward the SAME JWT unchanged
  ▼
http://aio-sandbox-broker-svc.default.svc.cluster.local:8080/   (Service → broker pods)
  │  broker_sandboxd.py:
  │  1. re-verify JWT (iss, aud, azp, group) against Keycloak JWKS
  │  2. principal = preferred_username; sid = "sess-<principal>-<sha256[:16]>"
  │  3. on "initialize": fire-and-forget POST router /_warm
  │  4. forward MCP body to router /mcp with X-Session-ID + X-Session-Pool
  ▼
sandboxd router (control plane)  →  resume/route → sandbox
```

There is also a secondary **internal** ALB (`broker.jicomusic.com`) that reaches
the broker Service directly, bypassing agentgateway's edge check. The broker's
own JWT re‑validation still applies, so this path is authenticated — but it does
**not** apply agentgateway's tool allowlist. Treat it as an internal/testing
door; the primary, tool‑authorized path is `agentgateway.jicomusic.com/mcp`.

## Keycloak (identity)

Defined by `deploy/00-keycloak-realm.yaml` (a `KeycloakRealmImport` CR in the
`keycloak` namespace).

- **Realm:** `sandbox` (display name "AIO Sandbox"), access‑token lifespan 3600s.
- **Issuer:** `https://keycloak.jicomusic.com/realms/sandbox`
- **JWKS:** `https://keycloak.jicomusic.com/realms/sandbox/protocol/openid-connect/certs`
- **Client:** `aio-sandbox-client` — a **public** client (no secret), PKCE (S256),
  standard authorization‑code flow + device grant. Redirect URIs are loopback
  (`http://localhost`/`127.0.0.1` wildcards) so desktop/CLI MCP clients can
  complete the flow.
- **Client scope `sandbox`** shapes the access token:
  - `aud = sandbox-router` (audience mapper) — what agentgateway and the broker require.
  - `groups` claim, emitted as **bare names** (`sandbox-users`, not `/sandbox-users`).
  - `preferred_username` — the principal the broker uses to derive the session id.
- **Groups:** `sandbox-users` is provisioned by the realm import. **`sandbox-power`
  is not** — an admin must create it in Keycloak and add power users to it.

Because group names are emitted bare, authorization rules test membership as
`"sandbox-power" in jwt.groups` (no leading slash).

## agentgateway (edge auth + tool authorization)

Defined by `deploy/30-agentgateway.yaml` (ConfigMap `agentgateway-config`,
Deployment `agentgateway`, Service `agentgateway-svc`). A single HTTP listener on
port `3000`, one route matching all paths (`pathPrefix: /`).

Three policies apply to the route:

### 1. `mcpAuthentication` (mode: strict)

Verifies the bearer JWT before anything else:

- `issuer: https://keycloak.jicomusic.com/realms/sandbox`
- `audiences: [sandbox-router]`
- `jwks.url: …/protocol/openid-connect/certs`
- `provider: keycloak: {}`
- `resourceMetadata` publishes RFC 9728 protected‑resource metadata pointing
  clients at Keycloak as the authorization server (`authorization_servers:
  [https://keycloak.jicomusic.com/realms/sandbox]`), with `scopesSupported:
  [openid, sandbox]` and `bearerMethodsSupported: [header]`. This is what lets a
  client like Claude Code discover where to log in with no manual OAuth config.

### 2. `mcpAuthorization` (per‑tool allowlist)

Rules are **OR'd** — a tool call is allowed if *any* rule matches — and
unauthorized tools are also filtered out of the `tools/list` response, so a
standard user never even sees the tools they can't call:

```
- '"sandbox-power" in jwt.groups'                 # power tier → ALL tools
- 'mcp.tool.name.startsWith("browser_")'          # anyone: browser automation
- 'mcp.tool.name == "sandbox_file_operations"'    # anyone: safe sandbox tools
- 'mcp.tool.name == "sandbox_str_replace_editor"'
- 'mcp.tool.name == "sandbox_convert_to_markdown"'
- 'mcp.tool.name == "sandbox_get_context"'
- 'mcp.tool.name == "sandbox_get_packages"'
- 'mcp.tool.name == "sandbox_load_skill"'
# sandbox_execute_bash / sandbox_execute_code are intentionally absent →
# only sandbox-power can see or call them.
```

`mcp.tool.name` is the bare tool name as published by the AIO MCP hub and passed
through by the broker.

### 3. `backendAuth: passthrough`

agentgateway forwards the **same user JWT unchanged** to the broker backend
(`http://aio-sandbox-broker-svc.default.svc.cluster.local:8080/`). It does not
mint a service token; the broker sees the real user identity.

## Broker (`broker_sandboxd.py`)

A FastAPI app that speaks MCP streamable‑HTTP at the root path `/` (and
`GET /healthz`), listening on port `8080`. It holds **no Kubernetes client** and
manages no claims — its whole job is authenticate → derive session → forward.

### Authentication (defense in depth)

`_authenticate()` re‑validates every request even though agentgateway already
did:

1. Require `Authorization: Bearer …`, else `401`.
2. Fetch the RS256 signing key from Keycloak JWKS (with retry to tolerate flaky
   DNS).
3. `jwt.decode(..., algorithms=["RS256"], audience="sandbox-router",
   issuer="https://keycloak.jicomusic.com/realms/sandbox",
   require=[exp, iss, aud])`.
4. `azp` must equal `aio-sandbox-client`, else `403` ("not via the Gateway").
5. If a required group is configured (`sandbox-users`), it must be in the
   `groups` claim, else `403`.
6. Principal = `preferred_username` → else `email` → else `sub`.

> Note: the broker checks the standard `azp` claim, which Keycloak emits
> automatically for the client. (The realm import also defines a hardcoded
> `client_id` claim mapper, but that is a different claim name and is not what the
> broker enforces.)

### Durable session id

`_sid_for(principal)` builds a stable id:

```
sid = "sess-" + sanitize(principal)[:24] + "-" + sha256(principal)[:16]
```

Because it's derived purely from the principal, **the same user always gets the
same session id**, across reconnects and across broker replicas. That's what makes
the session durable and teleportable: any broker replica computes the same id, and
the control plane maps that id to whichever worker currently holds the session
(or resumes it from a snapshot).

### Warm‑on‑initialize

When the MCP method is `initialize`, the broker answers the handshake locally and
fires a background `POST {ROUTER_URL}/_warm` with `X-Session-ID` + `X-Session-Pool`.
`/_warm` is a protocol‑agnostic router primitive that ensures the session is
Running (triggering a resume/cold‑start) **without** proxying any payload — so the
user's sandbox is already warming while the client finishes its handshake. It's
best‑effort: failures are swallowed and the first real call still works (just
possibly after a cold start).

### Forwarding

For every non‑handshake call, `_forward()` POSTs the raw MCP body to
`{ROUTER_URL}/mcp` with:

- `X-Session-ID: <sid>` — the durable per‑user id.
- `X-Session-Pool: <SANDBOXD_POOL>` — which pool/template the session should use
  (default `aio-pool`). The control plane lazily creates a Session object on first
  contact from this hint.
- `Content-Type` / `Accept` copied through.

Responses stream back unchanged (SSE or JSON), so token‑by‑token streaming from
the sandbox reaches the client. The broker only special‑cases `initialize`
(answered locally / protocol‑version normalized). A `DELETE` on the MCP endpoint
is a **no‑op** returning `204`: it does *not* destroy the durable session — the
session idle‑suspends to storage and teleport‑resumes later.

### The broker is protocol‑generic toward the control plane

The broker knows about MCP and auth; the router and operator know nothing about
MCP. The only contract between them is the two headers (`X-Session-ID`,
`X-Session-Pool`) and the `/_warm` primitive. This keeps the control plane
reusable by other front doors — the broker is deliberately swappable.

## Trust boundaries

```
 Internet │ agentgateway            │ broker              │ control plane
──────────┼─────────────────────────┼─────────────────────┼──────────────────
 user JWT │ verify iss/aud/sig      │ RE-verify iss/aud/  │ trusts X-Session-ID
 (opaque) │ enforce tool allowlist  │  azp/group          │  header from broker
          │ passthrough JWT         │ derive sid, forward │  (see note below)
```

- **Edge (agentgateway):** the only component exposed to the internet. It is the
  single place tool authorization is enforced.
- **Broker:** re‑verifies identity, so compromising the in‑cluster network alone
  doesn't let an attacker impersonate a user to the broker. It does *not* re‑check
  the tool allowlist — that's agentgateway's job.
- **Control plane:** today the router trusts the `X-Session-ID` header it receives.
  That's acceptable because only the broker can reach it in‑cluster; hardening this
  seam with mTLS + NetworkPolicy is the planned P1.5 phase (see
  [architecture-sandboxd.md](architecture-sandboxd.md)).

## Known drift / things to reconcile

The reference cluster has some state that isn't fully captured in the repo IaC —
worth knowing when you operate it:

- **Broker Service selector.** `deploy/20-broker.yaml` ships the older
  agent‑sandbox broker (`app: aio-sandbox-broker`). The live
  `aio-sandbox-broker-svc` selector was cut over to `app:
  aio-sandbox-broker-sandboxd` (the sandboxd broker). The cutover/rollback is a
  Service‑selector patch — see the admin guide.
- **`broker.jicomusic.com` ingress** (internal ALB) exists in the cluster but has
  no manifest under `deploy/`; it's referenced only in docs. Reconcile it into IaC
  or treat it as a manually‑applied internal door.
- **`sandbox-power` group** is not in the realm import — create it in Keycloak by
  hand.

See [admin-guide-broker.md](admin-guide-broker.md) for how to install and operate
all of this.
