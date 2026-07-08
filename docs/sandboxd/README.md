# sandboxd documentation

`sandboxd` is a session‑teleport control plane on Amazon EKS. Warm pools of
privileged worker pods run arbitrary OCI images as **nested gVisor sandboxes**;
a session's RAM + filesystem state is checkpointed to S3 and can be restored
("teleported") onto a different worker or node. An MCP client (e.g. Claude) talks
to it through an authenticating front door and a session‑aware router, so a user
keeps one durable session that survives suspend/resume.

This directory holds the operational documentation. Everything here is grounded
in the code and manifests on the `checkpoint-restore` branch; where a document
describes the live reference cluster it says so explicitly.

## Documents

| # | Document | Audience | What it covers |
|---|----------|----------|----------------|
| 1 | [end-user-guide-broker.md](end-user-guide-broker.md) | End users | Point Claude / an MCP client at the broker and authenticate. |
| 2 | [admin-guide-broker.md](admin-guide-broker.md) | Platform admins | Install & configure the broker, agentgateway, and Keycloak. |
| 3 | [architecture-broker.md](architecture-broker.md) | Architects / admins | How the broker, agentgateway, and Keycloak fit together (the auth front door). |
| 4 | [install-guide-sandboxd.md](install-guide-sandboxd.md) | Platform admins | Deploy the operator, router, Valkey, RBAC, Pod Identity, CRDs, and a pool. |
| 5 | [admin-guide-crds.md](admin-guide-crds.md) | Platform admins | Reference for every CRD field (SandboxTemplate, WarmPool, Session). |
| 6 | [architecture-sandboxd.md](architecture-sandboxd.md) | Architects / admins | How sandboxd, the router, the operator, Valkey, and workers relate. |
| 7 | [runbook-reproduce-test-env.md](runbook-reproduce-test-env.md) | Operators | End‑to‑end reproduction of the test environment + run a sample container. |
| 8 | [api-reference-sandboxd-worker.md](api-reference-sandboxd-worker.md) | Operators / integrators | HTTP API surface of the sandboxd worker agent (`:8090`). |

## Proposals (not yet scheduled)

| Document | Status | What it covers |
|----------|--------|----------------|
| [PRD-arbitrary-image-sessions.md](../PRD-arbitrary-image-sessions.md) | Proposed | "Bring your own container" — let an authorized user run an arbitrary image as a teleporting sandbox (authz, registry policy, dedicated pool, self‑service path). Implement if demand arises. |
| [PRD-graceful-scale-in.md](../PRD-graceful-scale-in.md) | **Implemented** | Operator sets `controller.kubernetes.io/pod-deletion-cost` on worker pods (idle low, busy high) so WarmPool scale‑in deletes idle workers before busy ones. |
| [PRD-checkpoint-on-terminate.md](../PRD-checkpoint-on-terminate.md) | **Implemented** | Checkpoint a running session to S3 when its worker pod terminates (scale‑in, drain, rollout, eviction) so it teleport‑resumes losslessly. Companion to graceful scale‑in. |
| [PRD-sandbox-iam-credentials.md](../PRD-sandbox-iam-credentials.md) | **Implemented** | Let a sandboxed workload assume an AWS IAM role via a container‑credentials endpoint — per‑session, auto‑refreshed, teleport‑safe, never the worker's identity. |
| [PRD-delegated-agent-access.md](../PRD-delegated-agent-access.md) | Proposed | Propagate the caller's identity past the broker into the sandbox so an agent/MCP server acts **on behalf of the user** against app APIs / other MCP servers (RFC 8693 token exchange, per‑request; app‑layer, not AWS). Phase 2: offline delegation. |
| [PRD-durable-assignment-state.md](../PRD-durable-assignment-state.md) | **Implemented** | Make Kubernetes (`Session.status` in etcd) the durable source of truth and Valkey a rebuildable cache, so a Valkey restart doesn't orphan S3 checkpoints / lose the session index. |

## The whole picture in one diagram

```
                 Keycloak (OIDC IdP, realm "sandbox")
                        ▲  issues user JWT (aud=sandbox-router, groups)
                        │
   MCP client ──HTTPS──►│  agentgateway  ──passthrough JWT──►  broker
   (Claude)   /mcp      │  (JWT verify +     (re-verify JWT,      │ X-Session-ID
                        │   tool allowlist)   derive session id)  │ X-Session-Pool
                        │                                         ▼
                        │                              sandboxd router  ──resume──►  operator
                        │                              (session→worker,               (assignment +
                        │                               stream proxy)                  teleport workflow)
                        │                                         │                          │
                        │                                         ▼                          ▼
                        │                              sandboxd worker pod  ◄── Valkey (assignment table)
                        │                              (nested gVisor sandbox)
                        │                                         │  checkpoint / restore
                        │                                         ▼
                        │                                        S3  (per-session snapshots)
```

- Documents 1–3 cover the **front door** (left of the router).
- Documents 4–6 cover the **control plane** (router, operator, Valkey, workers, S3).
- Document 7 stitches it together into a reproducible walkthrough.

## Reference cluster coordinates

The live examples throughout these docs use the reference environment:

- EKS cluster `EKSClusterStack-cluster`, region `us-west-2`, account `820537372947`.
- gVisor nodes labeled `sandbox=gvisor` with taint `sandbox=gvisor:NoSchedule`.
- Control plane in namespace `sandboxd-controlplane-system`; pools/sessions/workers
  in namespace `default`.
- S3 bucket `aio-checkpoint-spike-820537372947-us-west-2`.
- Public front door `agentgateway.jicomusic.com`; identity provider `keycloak.jicomusic.com`.

Substitute your own account, cluster, DNS, and bucket where you deploy.
