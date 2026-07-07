# Admin guide — broker, agentgateway, and Keycloak

How to install, configure, and operate the authenticating MCP front door: the
**broker** (`broker_sandboxd.py`), **agentgateway**, and **Keycloak**. This
assumes the sandboxd control plane (router, operator, Valkey, a pool) is already
deployed — see [install-guide-sandboxd.md](install-guide-sandboxd.md). For how the
pieces fit together, read [architecture-broker.md](architecture-broker.md) first.

All examples use the reference deployment (cluster `EKSClusterStack-cluster`,
`us-west-2`, namespace `default`). Substitute your own DNS, cert ARNs, and image
registry.

## Components and where they're defined

| Component | Manifest(s) | Live object(s) (ns `default`, Keycloak in ns `keycloak`) |
|-----------|-------------|-----------------|
| Keycloak realm | `deploy/00-keycloak-realm.yaml` | `KeycloakRealmImport/sandbox-realm` |
| agentgateway | `deploy/30-agentgateway.yaml`, `deploy/40-agentgateway-ingress.yaml` | ConfigMap `agentgateway-config`, Deployment `agentgateway`, Service `agentgateway-svc`, Ingress `agentgateway` |
| broker (sandboxd) | `broker/broker_sandboxd.py`, `broker/Dockerfile.sandboxd`, `checkpoint-restore/controlplane/deploy/aio/broker-sandboxd.yaml` | Deployment `aio-sandbox-broker-sandboxd`, Service `aio-sandbox-broker-svc` |
| RBAC/SA for the front door | `deploy/00-serviceaccount-rbac.yaml` | (as defined there) |

> The order of files (`00`→`40`) is the apply order.

---

## 1. Keycloak

You need a running Keycloak reachable at a stable HTTPS hostname
(`keycloak.jicomusic.com` in the reference env) — typically the Keycloak operator
in the `keycloak` namespace fronted by its own Ingress. Installing Keycloak
itself is out of scope here; this section covers the **realm** the platform needs.

### Import the realm

```sh
kubectl apply -f deploy/00-keycloak-realm.yaml
```

This creates `KeycloakRealmImport/sandbox-realm`, which provisions realm
`sandbox` with:

- **Client** `aio-sandbox-client` — public (no secret), PKCE S256, authorization‑code
  + device flows, loopback redirect URIs for CLI/desktop MCP clients.
- **Client scope** `sandbox` mapping onto the access token:
  - `aud = sandbox-router`
  - `groups` (bare names, e.g. `sandbox-users`)
  - `preferred_username`
- **Group** `sandbox-users`.

Issuer / JWKS that everything downstream validates against:

```
issuer: https://keycloak.jicomusic.com/realms/sandbox
jwks:   https://keycloak.jicomusic.com/realms/sandbox/protocol/openid-connect/certs
```

### Create the power‑user group (manual)

The realm import does **not** create `sandbox-power`. Create it and add users:

1. Keycloak admin console → realm `sandbox` → Groups → **Create group**
   `sandbox-power`.
2. Add the users who should have shell/code execution.

Group names are emitted as bare names, so the agentgateway rule tests
`"sandbox-power" in jwt.groups`.

### Manage users

- Add a user to `sandbox-users` to grant baseline access (browser + safe tools).
- Add a user to `sandbox-power` for shell/code execution.
- Removing a user from a group takes effect on their next token (access‑token
  lifespan is 3600s, so up to an hour for an already‑issued token, unless you also
  revoke sessions).

### Verify a token (optional)

You can sanity‑check what a user's token contains by decoding it and confirming
`iss`, `aud=sandbox-router`, `azp=aio-sandbox-client`, and `groups`.

---

## 2. agentgateway

agentgateway is the internet‑facing MCP proxy that verifies tokens and enforces
tool authorization.

### Deploy

```sh
kubectl apply -f deploy/30-agentgateway.yaml        # ConfigMap + Deployment + Service
kubectl apply -f deploy/40-agentgateway-ingress.yaml # ALB Ingress (TLS)
```

- **Deployment** `agentgateway`: image `cr.agentgateway.dev/agentgateway:v1.3.1`,
  args `["-f","/etc/agentgateway/config.yaml"]`, listens on `:3000`, config mounted
  from the ConfigMap.
- **Service** `agentgateway-svc`: ClusterIP `:3000`.
- **Ingress** `agentgateway`: `alb`, **internet‑facing**, TLS 443 → `:3000`,
  host `agentgateway.jicomusic.com`, health‑check path `/mcp` (success codes
  `200,400,401,406`). Requires an ACM cert ARN in the annotation — replace with
  your own.

### Configure — the `agentgateway-config` ConfigMap

The config has one HTTP listener (`:3000`) and one route (`pathPrefix: /`) with
three policies. Edit the ConfigMap to change behavior; the key knobs:

**Identity (`mcpAuthentication`, mode `strict`):**

```yaml
mcpAuthentication:
  mode: strict
  issuer: https://keycloak.jicomusic.com/realms/sandbox
  audiences: [sandbox-router]
  jwks:
    url: https://keycloak.jicomusic.com/realms/sandbox/protocol/openid-connect/certs
  provider: { keycloak: {} }
  resourceMetadata:
    resource: https://agentgateway.jicomusic.com/mcp
    authorization_servers:
      - https://keycloak.jicomusic.com/realms/sandbox
    scopesSupported: [openid, sandbox]
    bearerMethodsSupported: [header]
```

Update `issuer`, `jwks.url`, `resource`, and `authorization_servers` to your
Keycloak/DNS. `audiences` must match the `aud` your realm mints (`sandbox-router`).

**Tool authorization (`mcpAuthorization.rules`)** — this is where you tune who can
call what. Rules are OR'd; unauthorized tools are also hidden from `tools/list`:

```yaml
mcpAuthorization:
  rules:
  - '"sandbox-power" in jwt.groups'                 # power tier → all tools
  - 'mcp.tool.name.startsWith("browser_")'
  - 'mcp.tool.name == "sandbox_file_operations"'
  - 'mcp.tool.name == "sandbox_str_replace_editor"'
  - 'mcp.tool.name == "sandbox_convert_to_markdown"'
  - 'mcp.tool.name == "sandbox_get_context"'
  - 'mcp.tool.name == "sandbox_get_packages"'
  - 'mcp.tool.name == "sandbox_load_skill"'
```

To grant a new tool to standard users, add a rule for it. To make a tool
power‑only, remove its rule (the `"sandbox-power" in jwt.groups` rule still lets
power users call it). `sandbox_execute_bash` and `sandbox_execute_code` are
deliberately unlisted → power‑only.

**Backend (`backendAuth` + `backends`):**

```yaml
backendAuth: { passthrough: {} }   # forward the user JWT unchanged
backends:
- mcp:
    targets:
    - name: aio-broker
      mcp:
        host: http://aio-sandbox-broker-svc.default.svc.cluster.local.:8080/
```

Point `host` at your broker Service. After editing the ConfigMap, restart the
deployment so it re‑reads the file:

```sh
kubectl rollout restart deploy/agentgateway -n default
```

---

## 3. The broker (`broker_sandboxd.py`)

### Build the image

```sh
cd broker
docker build -f Dockerfile.sandboxd -t <registry>/aio-sandbox-broker-sandboxd:<tag> .
docker push <registry>/aio-sandbox-broker-sandboxd:<tag>
```

`Dockerfile.sandboxd` is `python:3.13-slim` + `requirements-sandboxd.txt`, running
`uvicorn broker_sandboxd:app --host 0.0.0.0 --port 8080`.

### Deploy

```sh
kubectl apply -f checkpoint-restore/controlplane/deploy/aio/broker-sandboxd.yaml
```

This creates Deployment `aio-sandbox-broker-sandboxd` (2 replicas, container
`broker`, port `8080`, readiness `GET /healthz`). It sets:

```yaml
env:
- { name: SANDBOXD_POOL,       value: aio-pool }
- { name: SANDBOXD_ROUTER_URL, value: http://sandboxd-router.sandboxd-controlplane-system.svc.cluster.local:8080 }
```

> Note: `broker-sandboxd.yaml` ships only a Deployment — no Service. The Service
> `aio-sandbox-broker-svc` is shared with the older broker and is where cutover
> happens (below).

### Configuration — all broker environment variables

The broker reads only environment variables (no config file). Defaults are in the
code, so you can run with almost none set.

| Env var | Default | Meaning |
|---------|---------|---------|
| `SANDBOXD_POOL` | `aio-pool` | Value sent as `X-Session-Pool`; selects the pool/template a new session uses. |
| `SANDBOXD_ROUTER_URL` | `http://sandboxd-router.sandboxd-controlplane-system.svc.cluster.local:8080` | Router base URL; broker POSTs `/mcp` and `/_warm` here. Trailing slash stripped. |
| `AIO_OIDC_ISSUER` | `https://keycloak.jicomusic.com/realms/sandbox` | Required JWT issuer; also derives the JWKS URL (`{issuer}/protocol/openid-connect/certs`). |
| `AIO_EXPECTED_AUDIENCE` | `sandbox-router` | Required JWT `aud`. |
| `AIO_GATEWAY_AZP` | `aio-sandbox-client` | Required JWT `azp` (proves the token came via the gateway client). |
| `AIO_REQUIRED_GROUP` | `sandbox-users` | Group the JWT `groups` claim must contain. |
| `SANDBOXD_MAX_SESSIONS_PER_USER` | `1` | Intended per‑user session cap. **Read but not enforced** in the current code (each principal maps to exactly one durable session anyway). |
| `AIO_MCP_PROTOCOL_VERSION` | `2025-11-25` | MCP protocol version advertised in the `initialize` response. |

If you deploy Keycloak under different DNS or a different realm/client/audience,
set the `AIO_*` vars in the broker Deployment to match — otherwise the broker's
own JWT re‑validation will reject tokens.

### Health

- `GET /healthz` → `200` (readiness probe).
- MCP is served at the **root path** `/` (POST for calls, DELETE is a `204`
  no‑op).

---

## 4. The public front door (ALB) and cutover

The public endpoint is an ALB Ingress → the shared broker Service
`aio-sandbox-broker-svc` → broker pods (selected by label). Cutting traffic
between broker implementations is a **Service‑selector patch** — no DNS or client
change.

### Cut over to the sandboxd broker

```sh
kubectl patch svc aio-sandbox-broker-svc -n default --type=merge \
  -p '{"spec":{"selector":{"app":"aio-sandbox-broker-sandboxd"}}}'
```

### Roll back to the previous broker

```sh
kubectl patch svc aio-sandbox-broker-svc -n default --type=merge \
  -p '{"spec":{"selector":{"app":"aio-sandbox-broker"}}}'
```

Clients need no change — same endpoint, same OAuth.

> The primary internet‑facing path is `agentgateway.jicomusic.com/mcp` (through
> agentgateway, with the tool allowlist). There is also an internal ALB
> `broker.jicomusic.com` that reaches the broker directly (JWT still enforced by
> the broker, but **no** tool allowlist). The internal ingress currently has no
> manifest under `deploy/`; if you rely on it, add it to IaC.

---

## 5. End‑to‑end verification

1. **Front door reachable + demands auth:**

   ```sh
   curl -s -o /dev/null -w '%{http_code}\n' https://agentgateway.jicomusic.com/mcp
   # expect 401 (unauthenticated) — proves the edge is up and enforcing
   ```

2. **Broker healthy in‑cluster** (from a debug pod):

   ```sh
   kubectl run tmp --rm -it --image=curlimages/curl -n default -- \
     curl -s http://aio-sandbox-broker-svc.default.svc.cluster.local:8080/healthz
   # expect: ok
   ```

3. **Full path with a real client:** connect Claude Code to
   `https://agentgateway.jicomusic.com/mcp` (see
   [end-user-guide-broker.md](end-user-guide-broker.md)), authenticate, and confirm
   tools appear. A `sandbox-users` account should NOT see
   `sandbox_execute_bash`/`sandbox_execute_code`; a `sandbox-power` account should.

4. **Warm path:** on `initialize` the broker fires `/_warm`; confirm the session's
   pool provisions/holds a worker (see the control‑plane admin guide for
   `kubectl get warmpool`).

---

## 6. Operations

### Logs

```sh
kubectl logs -n default deploy/agentgateway --tail=100              # edge: auth failures, routing
kubectl logs -n default deploy/aio-sandbox-broker-sandboxd --tail=100 # broker: auth, session ids, forwarding
```

### Common issues

| Symptom | Cause | Fix |
|---------|-------|-----|
| All clients get `401` at the edge | Keycloak/JWKS unreachable, wrong issuer/audience in `agentgateway-config`. | Verify `issuer`/`jwks.url`/`audiences` match the realm; check Keycloak is up. |
| Client authenticates but broker returns `403` | `azp`/group mismatch (broker re‑validation), or `AIO_*` env not matching the realm. | Ensure the user is in `sandbox-users`; align broker `AIO_GATEWAY_AZP`/`AIO_REQUIRED_GROUP`/`AIO_EXPECTED_AUDIENCE`/`AIO_OIDC_ISSUER` with Keycloak. |
| Power user can't see exec tools | User not in `sandbox-power`, or the group name has a leading slash. | Add to `sandbox-power`; confirm bare group names in the token. |
| `tools fetch failed` in client | First call hit a cold start / no‑capacity blip; client cached an empty tool list. | User reconnects/re‑auths. Keep the pool's `minIdle` ≥ expected concurrent new sessions (control‑plane admin guide). |
| Broker can't reach router | Wrong `SANDBOXD_ROUTER_URL`, router down. | Fix the env; check `kubectl get pods -n sandboxd-controlplane-system`. |
| Edited `agentgateway-config` but nothing changed | agentgateway reads the file at start. | `kubectl rollout restart deploy/agentgateway -n default`. |

### Scaling

- Broker and agentgateway are stateless — scale replicas freely. Because the
  broker derives the session id deterministically from the principal, any replica
  handles any user consistently.
- Session capacity is governed by the **pool** on the control‑plane side, not the
  broker — see [install-guide-sandboxd.md](install-guide-sandboxd.md) and
  [admin-guide-crds.md](admin-guide-crds.md) for `replicas`/`minIdle`.

### Rotating identity config

If you change the Keycloak realm, client, audience, or DNS, update **all three**
consumers so they agree: `agentgateway-config` (`issuer`/`jwks`/`audiences`), the
broker `AIO_*` env, and the realm's audience mapper. A mismatch anywhere breaks
auth.
