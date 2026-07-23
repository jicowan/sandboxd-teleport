# sandboxd-teleport

**sandboxd** is a **session‑teleport control plane on Amazon EKS**. Warm pools of
privileged worker pods run arbitrary OCI images as **nested gVisor sandboxes**; a
session's RAM + filesystem state is checkpointed to S3 and restored ("teleported")
onto any interchangeable worker on any node — surviving suspend/resume, scale‑in,
node drain, and eviction. Many mostly‑idle sessions share few workers (temporal
oversubscription), and idle sessions park in S3.

sandboxd is **standalone**: you install the operator, router, and workers and drive
it directly through its **CRDs** (`SandboxTemplate`, `AppTemplate`, `WarmPool`,
`Session`, `ForkSet`, `BaseSnapshot`) and the router's HTTP API. It does **not**
require a broker, gateway, or identity provider.

```
                          ┌──────────── sandboxd (the product) ─────────────┐
   your client / harness  │  router ──resume──▶ operator ◀──▶ Valkey        │
   ──X-Session-ID──▶ router│    │  (assignment + teleport workflow)          │
                          │    ▼                                            │
                          │  worker pod ──runsc──▶ nested gVisor sandbox    │
                          │    │  checkpoint / restore                      │
                          │    ▼                                            │
                          │   S3  (per-session snapshots)                   │
                          └─────────────────────────────────────────────────┘
```

## Documentation

**Start at [docs/sandboxd/](./docs/sandboxd/README.md)** — the source of truth:
install order, architecture, the full CRD reference, the reproduction runbook, the
worker API, and the SPIFFE/SPIRE security guide. See also
[docs/sandboxd/overview-and-vs-substrate.md](./docs/sandboxd/overview-and-vs-substrate.md)
for what sandboxd is and how it compares to Agent Substrate.

## A reference front door (optional)

sandboxd has no opinion about *who* may create a session. This repo also ships an
**optional reference front door** showing one way to give end users an
authenticated MCP interface to sandboxes running on sandboxd:

```
Claude Code ──HTTPS MCP + OAuth──▶ agentgateway ──▶ broker ──▶ sandboxd router ──▶ sandbox
              (browser login,        (validate JWT,   (per-user session id,
               Keycloak)              tool authz,      per-app routing)
                                      passthrough)
```

- **agentgateway** — public MCP gateway. Validates the Keycloak JWT, serves OAuth
  discovery, gates tools by group, and (multi‑app) routes each app path to the
  broker with an `X-Sandbox-App` header.
- **broker** (`broker/broker_sandboxd.py`) — re‑validates the JWT, enforces the
  group, derives a durable per‑user (per‑app) session id, and **transparently
  proxies MCP** to the sandboxd router. Supports several sandbox "apps" behind one
  broker (see [docs/PRD-broker-multi-app.md](./docs/PRD-broker-multi-app.md)).
- **Keycloak** — OIDC provider (`sandbox` realm; public client `aio-sandbox-client`).

The front door is documented in `docs/sandboxd/` (broker end‑user / admin /
architecture guides). It is a **reference**, not a requirement — swap in your own
identity/broker layer, or drive sandboxd directly.

## Repo layout

```
checkpoint-restore/         sandboxd itself
  controlplane/             operator + router (Go) + CRDs + deploy manifests
    deploy/                 control-plane, aio pool/app, SPIRE, smoke manifests
  sandboxd/                 the worker agent (Go) + runsc
  docs/                     pre-build design notes (ARCHITECTURE, TDD, runbooks)
broker/                     the reference front-door broker (broker_sandboxd.py)
deploy/                     shared front-door infra: Keycloak realm, agentgateway
                            (+ its ingress)
docs/sandboxd/              ← primary docs (product + front door)
docs/                       PRDs + design notes
```

## Reference environment

The live examples throughout the docs use an EKS cluster with
gVisor nodes tainted `sandbox=gvisor`, control plane in
`sandboxd-controlplane-system`, pools/sessions in `default`. Substitute your own
account, cluster, DNS, and bucket. (Docs use scrubbed placeholders —
`111122223333`, `example.com` — replace with your values.)
