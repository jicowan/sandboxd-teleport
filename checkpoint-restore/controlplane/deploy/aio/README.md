# AIO (+ multi-app) on sandboxd — reference front door deploy

Runs sandbox "apps" as **nested gVisor sandboxes** on a **generic pool** under the
sandboxd control plane, fronted by the sandboxd reference broker + agentgateway so a
Claude MCP client teleports its own durable session per app.

Full path (multi-app):

```
Claude --MCP+OAuth--> agentgateway (per-app route: /<app>/mcp, injects X-Sandbox-App)
   --(passthrough JWT + X-Sandbox-App)--> broker_sandboxd
   --X-Session-ID + X-Session-Pool + X-Session-App--> sandboxd router
   --resume--> operator (lazily creates Session{poolRef, appRef}) --> worker
   --> nested gVisor sandbox (the app's image) --> MCP tools
```

The broker transparently proxies MCP (forwards `initialize` + relays the sandbox's
`Mcp-Session-Id`), so any MCP server works — not just AIO.

## Files

| File | What |
|------|------|
| `generic-pool.yaml` | The **generic pool** (`SandboxTemplate aio-generic` = worker‑shape only, **no image**; `WarmPool aio-generic-pool`) + the **AppTemplates** (`aio-app`, `everything-app`). This is the current model. |
| `broker-sandboxd.yaml` | The reference front‑door broker (`broker/broker_sandboxd.py`, built from `broker/Dockerfile.sandboxd`). Its `SANDBOXD_APPS` env maps app‑id → {appTemplate, pool, group}. |
| `aio-pool.yaml` | **Legacy** single dedicated AIO pool (`SandboxTemplate` with a pinned image + `WarmPool aio-pool`). Kept for reference; the generic pool supersedes it. |

The shared front‑door infra (Keycloak realm, agentgateway multi‑app routes + its
ingress) is in the repo‑root `deploy/` (`00-keycloak-realm.yaml`,
`30-agentgateway.yaml`, `40-agentgateway-ingress.yaml`).

## Prerequisites

- The control plane is deployed (see `../smoke/controlplane.yaml`): operator,
  router, Valkey in `sandboxd-controlplane-system`.
- gVisor worker nodes labeled `sandbox=gvisor` (+ the matching taint toleration).
- The images referenced are pushed to ECR. A **private‑ECR** app image (e.g.
  `everything-app`) also needs the worker role's `ecr-pull` policy (see the install
  guide) so the worker can pull it.

## Apply

```sh
# 1. Generic pool + AppTemplates (in the control plane's session namespace)
kubectl apply -f generic-pool.yaml

# 2. The sandboxd reference broker
kubectl apply -f broker-sandboxd.yaml
```

Then apply the shared front door from repo‑root `deploy/` (Keycloak realm,
agentgateway, ingress) if not already up.

### Adding another app

1. Push the app's image (it must serve **Streamable‑HTTP MCP at `/mcp`** on its
   container port; private ECR is fine with the worker `ecr-pull` policy).
2. Add an `AppTemplate` (in `generic-pool.yaml`) with its image/ports/health/idle.
3. Add the app to the broker's `SANDBOXD_APPS` (`{appTemplate, pool, group}`).
4. Add an agentgateway route pair for it (`/<app>/mcp` data + its
   `/.well-known/oauth-protected-resource/<app>` discovery), injecting
   `X-Sandbox-App: <app>` — see `deploy/30-agentgateway.yaml`.
5. Client registers `https://<gw>/<app>/mcp` as a new MCP server.

## Notes / gotchas learned

- **First tool call is slow (~45s)** on a truly cold AIO session (image pull +
  Chrome boot). A client with a short per‑request timeout may need one retry; the
  session is warm afterward. Keep `minIdle` ≥ the expected concurrent new‑session
  rate so new sessions find a warm worker (a cold‑start `503` can poison a client's
  cached tool list → "connected · tools fetch failed"; reconnect/re‑auth recovers).
- **Generic vs dedicated pool:** a generic pool's `SandboxTemplate` has **no image**
  (worker‑shape only) and runs whatever `AppTemplate` the session's `appRef` names; a
  dedicated pool pins one image and takes `poolRef`‑only sessions. See
  [docs/sandboxd/admin-guide-crds.md](../../../../docs/sandboxd/admin-guide-crds.md)
  and [docs/sandboxd/PRD/PRD-arbitrary-image-sessions.md §13](../../../../docs/sandboxd/PRD/PRD-arbitrary-image-sessions.md).
- The resume/warm‑up deadline defaults to 90s (`SANDBOXD_RESUME_DEADLINE_SECONDS`) —
  must exceed AIO cold start.
- A **private‑ECR** workload image fails at `/run` with `502` (containerd `401`)
  unless the worker role has the `ecr-pull` policy (install guide, Step 1).
