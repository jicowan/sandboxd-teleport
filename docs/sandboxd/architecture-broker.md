# Architecture — the auth front door (Keycloak, agentgateway, broker)

This document explains the three components that sit between an MCP client and
the sandboxd control plane: **Keycloak** (identity), **agentgateway** (the
authenticating MCP proxy + tool authorization), and the **broker** (session
derivation + forwarding). Together they turn an anonymous HTTPS request into an
authenticated, authorized, per‑user MCP session routed into a sandbox.

> **This front door is a *reference*, not a required part of sandboxd.** sandboxd
> itself is a **generic platform for running sandboxes** — the sandbox inside a session
> can be an MCP server, an agent, a web service, a batch job, or anything else that
> runs in a container. sandboxd has no opinion about *who* creates a session or *how*
> callers reach it; you drive it directly through its CRDs and the router's HTTP API.
>
> The broker + agentgateway + Keycloak described here are **one worked example** of a
> user‑facing interface on top of sandboxd — specifically a **narrow, MCP‑focused**
> one: it exposes sandboxes that speak Streamable‑HTTP MCP to Claude/MCP clients, with
> OAuth login and per‑tool authorization. Anyone adopting sandboxd will likely **build
> their own broker/interface** suited to their workload — e.g. a plain reverse proxy or
> load balancer for a web‑service sandbox, a job scheduler for batch sandboxes, an
> agent runtime's own session manager, or a different identity/authorization model
> entirely. Read this as a pattern to adapt (how to authenticate a caller, derive a
> stable session id, and forward to the router), not as the only way to use sandboxd.

For the control plane on the other side of the broker (router, operator, Valkey,
workers), see [architecture-sandboxd.md](architecture-sandboxd.md).

## Design goals

- **One OAuth‑protected front door per app** — a per‑app MCP endpoint (e.g.
  `/aio/mcp`, `/everything/mcp`) that any compliant MCP client can use with no
  secret. One broker + one gateway front several sandbox apps (each a different
  sandbox image), selected per route.
- **Identity is centralized** in an OIDC provider (Keycloak); the platform never
  stores passwords.
- **Tool‑level authorization** — standard users get safe tools; only power users
  get shell/code execution, and unauthorized tools are *invisible*, not just
  blocked.
- **Durable per‑user, per‑app sessions** — a (user, app) pair maps to one stable
  sandbox session that teleports across workers, independent of which broker replica
  handles a request. A user's different apps are distinct sessions that never collide.
- **Defense in depth** — the token is verified at the edge (agentgateway) *and*
  re‑verified at the broker, so the broker is safe even if reached directly.

## Component roles

| Component | Kind | Responsibility |
|-----------|------|----------------|
| **Keycloak** | OIDC identity provider | Authenticates users; issues signed JWTs with audience, group, and username claims. Realm `sandbox`, public client `aio-sandbox-client`. |
| **agentgateway** | MCP‑aware reverse proxy (`cr.agentgateway.dev/agentgateway:v1.3.1`) | Terminates the MCP protocol at the edge; verifies the JWT (issuer/audience/signature); enforces a per‑tool authorization allowlist and filters unauthorized tools out of `tools/list`; forwards the untouched JWT to the broker. Serves **one route per app** (`/aio/mcp`, `/everything/mcp`, …) and injects `X-Sandbox-App: <app>` on each. |
| **broker** (`broker_sandboxd.py`) | FastAPI MCP server | Re‑verifies the JWT; **resolves the app** (from `X-Sandbox-App`) to a pool + AppTemplate and **enforces the app's required Keycloak group**; derives a durable **per‑user+app** session id; **transparently proxies** MCP to the router — forwarding *every* method (including `initialize`) and relaying the sandbox's `Mcp-Session-Id` back unchanged. |

## Request path

```
MCP client (Claude)
  │  Authorization: Bearer <user JWT>   (aud=sandbox-router, azp=aio-sandbox-client, groups=[sandbox-users|sandbox-power])
  ▼
https://agentgateway.example.com/aio/mcp        (per-app path; also /everything/mcp, …)
  │  ALB Ingress "agentgateway" (internet-facing, TLS 443 → :3000)
  ▼
Service agentgateway-svc:3000  →  agentgateway pod
  │  1. mcpAuthentication (strict): verify iss + aud against Keycloak JWKS
  │  2. mcpAuthorization: allow/deny each tool; hide unauthorized tools
  │  3. requestHeaderModifier: inject X-Sandbox-App: aio  (per-route)
  │  4. backendAuth: passthrough — forward the SAME JWT unchanged
  ▼
http://aio-sandbox-broker-svc.default.svc.cluster.local:8080/   (Service → broker pods)
  │  broker_sandboxd.py:
  │  1. re-verify JWT (iss, aud, azp, group) against Keycloak JWKS
  │  2. resolve X-Sandbox-App → (pool, AppTemplate); enforce the app's group (403 if missing)
  │  3. principal = preferred_username; sid = "sess-<principal>-<app>-<sha256[:16]>"
  │  4. TRANSPARENTLY proxy EVERY method (incl. initialize) to router /mcp with
  │     X-Session-ID + X-Session-Pool (capacity) + X-Session-App (AppTemplate),
  │     relaying the sandbox's Mcp-Session-Id header back unchanged
  ▼
sandboxd router (control plane)  →  resume/route → sandbox (runs the app's MCP server)
```

> **Reach the broker only through agentgateway.** The broker's tool‑level
> authorization and per‑app routing (the `X-Sandbox-App` injection) are applied by
> agentgateway, so a client that reached the broker Service directly would bypass the
> tool allowlist (the broker's own JWT re‑validation still applies, but not the
> allowlist). Do **not** expose the broker Service on its own ingress/load balancer;
> keep agentgateway the single public entry point (a NetworkPolicy restricting the
> broker Service to agentgateway is a reasonable hardening step).

## Keycloak (identity)

Defined by `deploy/00-keycloak-realm.yaml` (a `KeycloakRealmImport` CR in the
`keycloak` namespace).

- **Realm:** `sandbox` (display name "AIO Sandbox"), access‑token lifespan 3600s.
- **Issuer:** `https://keycloak.example.com/realms/sandbox`
- **JWKS:** `https://keycloak.example.com/realms/sandbox/protocol/openid-connect/certs`
- **Client:** `aio-sandbox-client` — a **public** client (no secret), PKCE (S256),
  standard authorization‑code flow + device grant. Redirect URIs are loopback
  (`http://localhost`/`127.0.0.1` wildcards) so desktop/CLI MCP clients can
  complete the flow.
- **Client scope `sandbox`** shapes the access token:
  - `aud = sandbox-router` (audience mapper) — what agentgateway and the broker require.
  - `groups` claim, emitted as **bare names** (`sandbox-users`).
  - `preferred_username` — the principal the broker uses to derive the session id.
- **Groups:** `sandbox-users` is provisioned by the realm import. **`sandbox-power`
  is not** — an admin must create it in Keycloak and add power users to it.

Because group names are emitted bare, authorization rules test membership as
`"sandbox-power" in jwt.groups` (no leading slash).

## agentgateway (edge auth + tool authorization)

Defined by `deploy/30-agentgateway.yaml` (ConfigMap `agentgateway-config`,
Deployment `agentgateway`, Service `agentgateway-svc`). A single HTTP listener on
port `3000` with **one route per app** — a data route (`/aio/mcp`,
`/everything/mcp`, …) and a matching OAuth discovery route
(`/.well-known/oauth-protected-resource/<app>/mcp`) for each. There is **no
catch‑all `/` route** anymore.

Each data route:

- injects `X-Sandbox-App: <app>` via a `requestHeaderModifier` (this header is how
  the broker knows which app the request is for), and
- has its own `resourceMetadata.resource` so RFC 9728 discovery is route‑correct —
  a client hitting `/aio/mcp` discovers `/aio/mcp` as the protected resource, not a
  generic `/mcp`.

The same three policies apply to every route:

### 1. `mcpAuthentication` (mode: strict)

Verifies the bearer JWT before anything else:

- `issuer: https://keycloak.example.com/realms/sandbox`
- `audiences: [sandbox-router]`
- `jwks.url: …/protocol/openid-connect/certs`
- `provider: keycloak: {}`
- `resourceMetadata` publishes RFC 9728 protected‑resource metadata pointing
  clients at Keycloak as the authorization server (`authorization_servers:
  [https://keycloak.example.com/realms/sandbox]`), with `scopesSupported:
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

A FastAPI app that speaks MCP streamable‑HTTP at the root path `/` — both `POST`
(client→server) and `GET` (the server→client SSE stream) — plus `GET /healthz`,
listening on port `8080`. It holds **no Kubernetes client** and manages no claims —
its whole job is authenticate → resolve app → derive session → transparently proxy.

### Multi‑app model

One broker fronts several sandbox **apps**, each backed by a different sandbox
image. The mapping lives in the `SANDBOXD_APPS` env var — a JSON registry of
`app-id → {appTemplate, pool, group}`:

- **`appTemplate`** — the AppTemplate (the *workload*) the session should run.
- **`pool`** — the WarmPool (the *capacity*) to schedule it on. These are generic
  pools: a WarmPool whose SandboxTemplate carries no image (worker‑shape only), so
  it runs whatever AppTemplate a session brings via `Session.spec.appRef`.
- **`group`** — the Keycloak group required to use this app.

The broker reads `X-Sandbox-App` (injected by agentgateway's per‑app route) and
resolves it to that triple. The header is only a **hint**; the **security boundary
is the group check** — if the caller's `groups` claim lacks the app's required
group, the broker returns `403`. `SANDBOXD_DEFAULT_APP` names the fallback app used
if a request ever arrives without `X-Sandbox-App` (normally every request has it,
since agentgateway's per‑app route injects it).

If `SANDBOXD_APPS` is unset the broker runs in **legacy single‑app mode**: it uses
`SANDBOXD_POOL` + `SANDBOXD_APP`, ignores `X-Sandbox-App`, and its session ids omit
the app segment (see below).

### Authentication (defense in depth)

`_authenticate()` re‑validates every request even though agentgateway already
did:

1. Require `Authorization: Bearer …`, else `401`.
2. Fetch the RS256 signing key from Keycloak JWKS (with retry to tolerate flaky
   DNS).
3. `jwt.decode(..., algorithms=["RS256"], audience="sandbox-router",
   issuer="https://keycloak.example.com/realms/sandbox",
   require=[exp, iss, aud])`.
4. `azp` must equal `aio-sandbox-client`, else `403` ("not via the Gateway").
5. If a base required group is configured (`sandbox-users`), it must be in the
   `groups` claim, else `403`. In multi‑app mode the resolved app's own required
   group is additionally enforced (see [Multi‑app model](#multi-app-model)).
6. Principal = `preferred_username` → else `email` → else `sub`.

> Note: the broker checks the standard `azp` claim, which Keycloak emits
> automatically for the client. (The realm import also defines a hardcoded
> `client_id` claim mapper, but that is a different claim name and is not what the
> broker enforces.)

### Durable session id

`_sid_for(principal, app)` builds a stable id that folds in the app:

```
sid = "sess-" + sanitize(principal)[:24] + "-" + <app> + "-" + sha256(principal+app)[:16]
```

Because it's derived purely from the (principal, app) pair, **the same user always
gets the same session id for a given app**, across reconnects and across broker
replicas — and a user's *different* apps get *distinct* session ids that never
collide. That's what makes the session durable and teleportable: any broker replica
computes the same id, and the control plane maps that id to whichever worker
currently holds the session (or resumes it from a snapshot).

In legacy single‑app mode the app segment is omitted (`sess-<principal>-<hash>`),
unchanged from before.

### Transparent MCP proxy

The broker is a **transparent MCP proxy** — it does **not** answer `initialize`
locally or special‑case any method. It **forwards every method** (including
`initialize` and `notifications/initialized`) straight to the sandbox's own MCP
server via the router, and it **relays the sandbox's `Mcp-Session-Id` response
header back to the client unchanged** (and passes the client's `Mcp-Session-Id`
through to the sandbox on later calls).

This is deliberate: it lets **stateful MCP servers** — ones that issue and then
require their own `Mcp-Session-Id` — work through the front door, not just AIO.
Forwarding `initialize` also **warms** the session (it triggers the resume /
cold‑start on first contact), so there is no separate synthetic‑initialize or
fire‑and‑forget `/_warm` path anymore.

> **Two orthogonal session ids.** `X-Session-ID` is the **control‑plane routing**
> id the broker derives per (user, app); it never leaves the platform. `Mcp-Session-Id`
> is the **MCP protocol** session, owned by the sandbox's MCP server and opaque to
> the broker. They are independent — the broker forwards one and relays the other.

### Forwarding

For every call, `_forward()` POSTs the raw MCP body to `{ROUTER_URL}/mcp` with:

- `X-Session-ID: <sid>` — the durable per‑user+app routing id.
- `X-Session-Pool: <pool>` — the WarmPool (**capacity**) the session should run on.
- `X-Session-App: <appTemplate>` — the AppTemplate (**workload**) the session
  should run. The operator lazily creates the `Session` with `poolRef` (pool) +
  `appRef` (app) on first contact from these hints, so a generic pool runs whatever
  app the session brings. (Legacy single‑app mode sends only `X-Session-Pool`.)
- the client's `Mcp-Session-Id` (when present) and `Content-Type` / `Accept`,
  copied through.

Responses stream back unchanged (SSE or JSON) with the sandbox's `Mcp-Session-Id`
relayed verbatim, so token‑by‑token streaming reaches the client. A `DELETE` on
the MCP endpoint is a **no‑op** returning `204`: it does *not* destroy the durable
session — the session idle‑suspends to storage and teleport‑resumes later.

See [PRD-broker-multi-app.md](../PRD-broker-multi-app.md) and
[PRD-arbitrary-image-sessions.md](../PRD-arbitrary-image-sessions.md) §13 for the
generic‑pool + AppTemplate design behind these headers.

### The broker is protocol‑generic toward the control plane

The broker knows about MCP and auth; the router and operator know nothing about
MCP. The only contract between them is the routing headers (`X-Session-ID`,
`X-Session-Pool`, `X-Session-App`) on `/mcp` — the router proxies the opaque body
and the operator lazily materializes a `Session` (pool + app) from those hints.
This keeps the control plane reusable by other front doors — the broker is
deliberately swappable.

## Trust boundaries

```
 Internet │ agentgateway            │ broker              │ control plane
──────────┼─────────────────────────┼─────────────────────┼──────────────────
 user JWT │ verify iss/aud/sig      │ RE-verify iss/aud/  │ trusts X-Session-ID
 (opaque) │ enforce tool allowlist  │  azp/group          │  header from broker
          │ inject X-Sandbox-App    │ resolve app +       │  (see note below)
          │ passthrough JWT         │  enforce app group; │
          │                         │ derive sid, proxy   │
```

- **Edge (agentgateway):** the only component exposed to the internet. It is the
  single place tool authorization is enforced.
- **Broker:** re‑verifies identity, so compromising the in‑cluster network alone
  doesn't let an attacker impersonate a user to the broker. `X-Sandbox-App` is only
  a hint — the broker enforces the app's **Keycloak group** as the real boundary, so
  a forged/absent header can't grant access to an app the caller isn't entitled to.
  It does *not* re‑check the tool allowlist — that's agentgateway's job.
- **Control plane:** today the router trusts the `X-Session-ID` header it receives.
  That's acceptable because only the broker can reach it in‑cluster; hardening this
  seam with mTLS + NetworkPolicy is the planned P1.5 phase (see
  [architecture-sandboxd.md](architecture-sandboxd.md)).

See [admin-guide-broker.md](admin-guide-broker.md) for how to install and operate
all of this.
