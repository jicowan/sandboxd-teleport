# How to add a sandbox "app" (a new MCP server) to the front door

This walks through adding a **new app** — a different MCP‑server image users can run
as their own teleporting sandbox — to the reference front door. It's the end‑to‑end
version of what shipped `aio` + `everything`.

An "app" is one workload image. Through the front door it must serve **Streamable‑HTTP
MCP at `POST /mcp`** on a container port (the broker/router proxy MCP to it). Each app
gets its own client endpoint (`/<app>/mcp`), its own durable per‑user session, and its
own entitlement.

> **Prerequisite:** the generic pool exists (`aio-generic-pool` — a `WarmPool` whose
> `SandboxTemplate` has no image; see
> [admin-guide-crds.md](admin-guide-crds.md) and
> [PRD-arbitrary-image-sessions.md §13](PRD/PRD-arbitrary-image-sessions.md)). Apps run
> on it; you don't create a pool per app.

There are **four** places to touch. Only step 4 (Keycloak group) is optional.

---

## 0. The image (once)

Your app must serve Streamable‑HTTP MCP at `/mcp` on a port. If your MCP server only
speaks **stdio** (most reference servers do), wrap it — e.g. a Node image running the
server in HTTP mode, or `supergateway --outputTransport streamableHttp
--streamableHttpPath /mcp --port 8080 --stdio "<your stdio server>"`. Push it to a
registry the workers can pull.

- **Public registry** (ghcr/Docker Hub): pulls anonymously, nothing extra.
- **Private ECR**: the worker pulls it via its Pod Identity — the worker role needs the
  `ecr-pull` policy (see [install-guide-sandboxd.md](install-guide-sandboxd.md) Step 1).
  Without it, `/run` fails with `502` (containerd `401`).

Reference for the `everything` app's image (a real, distinct MCP server):

```dockerfile
FROM node:22-slim
ENV PORT=8080
RUN npm install -g @modelcontextprotocol/server-everything
EXPOSE 8080
CMD ["mcp-server-everything", "streamableHttp"]   # serves POST /mcp on :8080
```

## 1. AppTemplate (sandboxd) — *what runs*

Add an `AppTemplate` (the scheduling‑free workload half). Put it alongside the others
in `checkpoint-restore/controlplane/deploy/aio/generic-pool.yaml`:

```yaml
apiVersion: core.sandboxd.io/v1alpha1
kind: AppTemplate
metadata: { name: myapp-app, namespace: default }
spec:
  image: <registry>/myapp:v1
  ports: [{ container: 8080, host: 8080 }]
  health: { probe: tcp, probePort: 8080 }   # or http + /healthz if the server has one
  idle: { timeoutSeconds: 600, action: suspend }
  # iam: { roleArn: "arn:aws:iam::<acct>:role/..." }   # optional per-session AWS role
  # network: { egressMbps: 100, ingressMbps: 200 }     # optional per-sandbox bandwidth caps (Mbit/s; 0/unset = uncapped)
```

```sh
kubectl apply -f checkpoint-restore/controlplane/deploy/aio/generic-pool.yaml
```

> No scheduling/resources here — those are the *pool's* worker‑shape
> (`SandboxTemplate`), including its **runtime** (`gvisor` default, or `microvm`). An
> app runs on whatever generic pool it's assigned to; to run it as a microVM, point
> its session/app at a `runtime: microvm` pool (a microVM‑capable node group + the
> `sandboxd-microvm` worker image — see the install guide). The AppTemplate itself is
> runtime‑neutral.

## 2. Broker registry (`SANDBOXD_APPS`) — *map an id → app + entitlement*

Add the app to the broker's `SANDBOXD_APPS` env (in
`checkpoint-restore/controlplane/deploy/aio/broker-sandboxd.yaml`). The key is the
**app‑id** (used in the URL path + `X-Sandbox-App` header); the value binds it to the
AppTemplate, the pool, and the required Keycloak group:

```yaml
- name: SANDBOXD_APPS
  value: |
    {"aio":       {"appTemplate":"aio-app",       "pool":"aio-generic-pool","group":"sandbox-users"},
     "everything":{"appTemplate":"everything-app","pool":"aio-generic-pool","group":"sandbox-power"},
     "myapp":     {"appTemplate":"myapp-app",     "pool":"aio-generic-pool","group":"sandbox-users"}}
```

Roll the broker (`kubectl set env` / re‑apply + rollout). The `group` is the
**security boundary** — the broker returns `403` if the caller isn't in it. (The
`X-Sandbox-App` header is only a hint; the group check is authoritative.)

## 3. agentgateway routes — *the client‑facing endpoint*

agentgateway needs **two routes** per app (in `deploy/30-agentgateway.yaml`), because
it serves the OAuth discovery document per resource path. Add, before the other apps
(order doesn't matter since each has a distinct prefix, but keep discovery routes
grouped):

```yaml
# discovery: /.well-known/oauth-protected-resource/myapp/mcp -> myapp's resource
- name: myapp-wellknown
  matches: [{ path: { pathPrefix: /.well-known/oauth-protected-resource/myapp } }]
  policies:
    mcpAuthentication:
      mode: strict
      issuer: https://keycloak.example.com/realms/sandbox
      audiences: [sandbox-router]
      jwks: { url: https://keycloak.example.com/realms/sandbox/protocol/openid-connect/certs }
      provider: { keycloak: {} }
      resourceMetadata:
        resource: https://agentgateway.example.com/myapp/mcp
        authorization_servers: [https://keycloak.example.com/realms/sandbox]
        scopesSupported: [openid, sandbox]
        bearerMethodsSupported: [header]
    backendAuth: { passthrough: {} }
  backends:
  - mcp: { targets: [{ name: aio-broker, mcp: { host: http://aio-sandbox-broker-svc.default.svc.cluster.local.:8080/ } }] }
# data path: /myapp/mcp -> inject X-Sandbox-App: myapp -> broker
- name: myapp-route
  matches: [{ path: { pathPrefix: /myapp } }]
  policies:
    mcpAuthentication:
      mode: strict
      issuer: https://keycloak.example.com/realms/sandbox
      audiences: [sandbox-router]
      jwks: { url: https://keycloak.example.com/realms/sandbox/protocol/openid-connect/certs }
      provider: { keycloak: {} }
      resourceMetadata:
        resource: https://agentgateway.example.com/myapp/mcp
        authorization_servers: [https://keycloak.example.com/realms/sandbox]
        scopesSupported: [openid, sandbox]
        bearerMethodsSupported: [header]
    backendAuth: { passthrough: {} }
    requestHeaderModifier: { set: { X-Sandbox-App: myapp } }
    # tool authz: gate on group (or copy the AIO per-tool rules if it's an AIO-like hub)
    mcpAuthorization:
      rules: ['"sandbox-users" in jwt.groups']
  backends:
  - mcp: { targets: [{ name: aio-broker, mcp: { host: http://aio-sandbox-broker-svc.default.svc.cluster.local.:8080/ } }] }
```

Apply the ConfigMap and restart agentgateway. **There is no catch‑all `/` route** —
every app is explicit, so discovery stays route‑correct (a `POST /myapp/mcp` 401s with
`WWW-Authenticate: … resource_metadata=…/myapp/mcp`, which resolves to `resource:
…/myapp/mcp`).

> **`mcpAuthorization`:** these CEL rules are agentgateway's per‑tool allowlist (it
> filters `tools/list` too). For an AIO‑style hub, reuse the tiered AIO rules. For a
> simple server, gating on group (`'"sandbox-users" in jwt.groups'`) exposes all its
> tools to entitled users.

## 4. Keycloak group (optional) — *who may use it*

The app's `group` (step 2) must exist and the intended users must be members. If you
reuse an existing group (`sandbox-users` / `sandbox-power`), nothing to do. To gate an
app to a subset, create a new group (e.g. `sandbox-app-myapp`), set it as the app's
`group`, and add users. No realm/schema change — just group membership (the `groups`
claim is already mapped into the token).

## 5. Register + connect

The client adds the app as its own MCP server:

```sh
claude mcp add --transport http --client-id aio-sandbox-client \
  myapp https://agentgateway.example.com/myapp/mcp
```

Authenticate; the user gets a durable `sess-<user>-myapp-<hash>` sandbox on the generic
pool, independent of their other apps.

---

## Verify (server‑side)

```sh
# OAuth discovery is route-correct for the new app:
curl -s https://<gw>/.well-known/oauth-protected-resource/myapp/mcp | jq .resource
#   -> "https://<gw>/myapp/mcp"

# the session lands with the right appRef + image:
kubectl get session -n default | grep myapp
kubectl exec -n <cp-ns> <valkey-pod> -- redis-cli GET session:sess-<user>-myapp-<hash>
#   -> "image":"<registry>/myapp:v1", "pool":"aio-generic-pool", state Running
```

## Checklist

| # | Change | File | Required? |
|---|--------|------|-----------|
| 0 | Build/push an image that serves `POST /mcp` | (registry) | yes |
| 1 | `AppTemplate` (image/ports/health/idle) | `controlplane/deploy/aio/generic-pool.yaml` | yes |
| 2 | Add app‑id → {appTemplate, pool, group} to `SANDBOXD_APPS` | `controlplane/deploy/aio/broker-sandboxd.yaml` | yes |
| 3 | Two agentgateway routes (`/myapp` + `/.well-known/…/myapp`) | `deploy/30-agentgateway.yaml` | yes |
| 4 | Keycloak group membership | Keycloak | only if using a new group |
| — | Worker role `ecr-pull` policy | IAM | only for private‑ECR images |

**No sandboxd control‑plane change** (operator/router/worker/CRDs) is needed to add an
app — that's the point of the generic pool + AppTemplate model.
