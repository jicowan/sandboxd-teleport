# sandboxd documentation

`sandboxd` is a session‑teleport control plane on Amazon EKS. Warm pools of
privileged worker pods run arbitrary OCI images as **nested gVisor sandboxes**;
a session's RAM + filesystem state is checkpointed to S3 and can be restored
("teleported") onto a different worker or node. An MCP client (e.g. Claude) talks
to it through an authenticating front door and a session‑aware router, so a user
keeps one durable session that survives suspend/resume.

This directory holds the operational documentation. Everything here is grounded
in the code and manifests in this repository; where a document describes the live
reference cluster it says so explicitly.

## Documents

| # | Document | Audience | What it covers |
|---|----------|----------|----------------|
| 0 | [overview-and-vs-substrate.md](overview-and-vs-substrate.md) | Everyone / evaluators | What sandboxd is at a glance, and how it differs from [Agent Substrate](https://github.com/agent-substrate/substrate) (shared gVisor‑teleport idea; why sandboxd exists — Substrate is blocked on managed EKS). |
| 1 | [end-user-guide-broker.md](end-user-guide-broker.md) | End users | Point Claude / an MCP client at the broker and authenticate. |
| 2 | [admin-guide-broker.md](admin-guide-broker.md) | Platform admins | Install & configure the broker, agentgateway, and Keycloak. |
| 3 | [architecture-broker.md](architecture-broker.md) | Architects / admins | How the broker, agentgateway, and Keycloak fit together (the auth front door). |
| 3b | [howto-add-an-app.md](howto-add-an-app.md) | Platform admins | Add a new sandbox "app" (MCP server) to the front door: AppTemplate + broker registry + agentgateway routes + Keycloak group. |
| 3c | [HOWTO-tool-access-and-quotas.md](HOWTO-tool-access-and-quotas.md) | Platform admins | Filter which tools each user sees (agentgateway `mcpAuthorization` tiers) and the create gate / per‑user session model. |
| 4 | [install-guide-sandboxd.md](install-guide-sandboxd.md) | Platform admins | Deploy the operator, router, Valkey, RBAC, Pod Identity, CRDs, and a pool. |
| 5 | [admin-guide-crds.md](admin-guide-crds.md) | Platform admins | Reference for every CRD field (SandboxTemplate, AppTemplate, WarmPool, Session, ForkSet, BaseSnapshot). |
| 6 | [architecture-sandboxd.md](architecture-sandboxd.md) | Architects / admins | How sandboxd, the router, the operator, Valkey, and workers relate. |
| 6b | [architecture-networking-lifecycle.md](architecture-networking-lifecycle.md) | Architects / engineers | Deep dive on the sandbox **networking data path** (veth + stable interior IP, DNAT, reconnect-on-restore) and the **sandbox lifecycle** (readiness/idle/reset). |
| 7 | [runbook-reproduce-test-env.md](runbook-reproduce-test-env.md) | Operators | End‑to‑end reproduction of the test environment + run a sample container. |
| 8 | [api-reference-sandboxd-worker.md](api-reference-sandboxd-worker.md) | Operators / integrators | HTTP API surface of the sandboxd worker agent (`:8090`). |
| 9 | [security-spiffe-spire.md](security-spiffe-spire.md) | Platform admins / security | Secure the control‑plane hops (router→operator, operator→worker) with SPIFFE/SPIRE mTLS: install, register identities, enable, verify. |

**Background (optional):**
[../../checkpoint-restore/history-worker-networking-and-teleport.md](../../checkpoint-restore/history-worker-networking-and-teleport.md)
is the dated engineering log from the worker bring‑up — the problems hit getting a
real workload to boot, network, checkpoint, and teleport, and how each was resolved.
It's kept for rationale only; the current design is in doc 6b, not here.

## New here? Order of operations

sandboxd itself (steps 1–2) is **standalone** — you can drive it directly via its
CRDs + the router API, with no front door. The front door (steps 3–4) is an
**optional reference** for giving users authenticated MCP access. Manifests live in
two trees: the sandboxd control plane + pools under
`checkpoint-restore/controlplane/deploy/`, and the shared front‑door infra (Keycloak
realm, agentgateway + ingress) in the repo‑root `deploy/`. The docs give the paths.

1. **Control plane** — CRDs, RBAC, Valkey, operator, router, worker Pod Identity
   (S3 + optional ECR pull), a worker pool:
   [install-guide-sandboxd.md](install-guide-sandboxd.md) (or the condensed
   [runbook Part B](runbook-reproduce-test-env.md#b1-install-the-control-plane)).
2. **(Optional) mTLS** — SPIRE + secure the control‑plane hops, *before* running
   sessions: [security-spiffe-spire.md](security-spiffe-spire.md) (runbook step B1.5).
3. **(Optional) reference front door** — Keycloak realm + agentgateway + ingress
   (repo‑root `deploy/00`,`30`,`40`) and the sandboxd broker + generic pool/AppTemplates
   (`checkpoint-restore/controlplane/deploy/aio/`):
   [admin-guide-broker.md](admin-guide-broker.md).
4. **Connect a client** — register each app's endpoint (`…/<app>/mcp`) in Claude and
   authenticate: [end-user-guide-broker.md](end-user-guide-broker.md).

The [runbook](runbook-reproduce-test-env.md) walks the whole thing end‑to‑end
(direct‑worker path → control plane → front door) as one script; the guides above are
the reference for each layer. For *how it all fits*, read
[architecture-sandboxd.md](architecture-sandboxd.md) and
[architecture-broker.md](architecture-broker.md) first.

## Worked examples

- **On‑demand / multi‑app** (request→response): a generic pool running MCP apps behind
  the reference front door — `checkpoint-restore/controlplane/deploy/aio/`.
- **[ForkSet / RL fan‑out](../../examples/forkset/)** (parallel rollouts): fan out N
  isolated environments from one source — cold‑start (independent trials) or a golden
  snapshot (branch‑from‑a‑known‑state) — driven directly against the router. Shows the
  `Lazy` + checkpoint‑when‑done (temporal oversubscription) pattern.

## Proposals (not yet scheduled)

| Document | Status | What it covers |
|----------|--------|----------------|
| [PRD-arbitrary-image-sessions.md](../PRD-arbitrary-image-sessions.md) | Proposed | "Bring your own container" — let an authorized user run an arbitrary image as a teleporting sandbox (authz, registry policy, dedicated pool, self‑service path). Implement if demand arises. |
| [PRD-graceful-scale-in.md](../PRD-graceful-scale-in.md) | **Implemented** | Operator sets `controller.kubernetes.io/pod-deletion-cost` on worker pods (idle low, busy high) so WarmPool scale‑in deletes idle workers before busy ones. |
| [PRD-checkpoint-on-terminate.md](../PRD-checkpoint-on-terminate.md) | **Implemented** | Checkpoint a running session to S3 when its worker pod terminates (scale‑in, drain, rollout, eviction) so it teleport‑resumes losslessly. Companion to graceful scale‑in. |
| [PRD-sandbox-iam-credentials.md](../PRD-sandbox-iam-credentials.md) | **Implemented** | Let a sandboxed workload assume an AWS IAM role via a container‑credentials endpoint — per‑session, auto‑refreshed, teleport‑safe, never the worker's identity. |
| [PRD-delegated-agent-access.md](../PRD-delegated-agent-access.md) | Proposed | Propagate the caller's identity past the broker into the sandbox so an agent/MCP server acts **on behalf of the user** against app APIs / other MCP servers (RFC 8693 token exchange, per‑request; app‑layer, not AWS). Phase 2: offline delegation. |
| [PRD-durable-assignment-state.md](../PRD-durable-assignment-state.md) | **Implemented** | Make Kubernetes (`Session.status` in etcd) the durable source of truth and Valkey a rebuildable cache, so a Valkey restart doesn't orphan S3 checkpoints / lose the session index. |
| [PRD-control-plane-scalability.md](../PRD-control-plane-scalability.md) | **Implemented** (hot paths) | Removed the control plane's O(N)-on-a-timer sweeper scans and bounded the etcd status-mirror write rate under session churn (indexed O(due) sweeps, mirror only durability-critical transitions, O(1) per-pool counts). Remaining follow-ups gated on real load. |
| [PRD-session-garbage-collection.md](../PRD-session-garbage-collection.md) | **Implemented** | Reap a dead session's whole footprint (S3 snapshot + Valkey entry + `Session` CR) across all dead-session classes: TTL, abandoned (dead-worker zombie), orphan-S3, orphan-CR. Ownership-aware CR deletion; dry-run-first. |
| [PRD-snapshot-fork.md](../PRD-snapshot-fork.md) | **Implemented** | **ForkSet:** fan out N independent sessions from one common source — a **snapshot** (`BaseSnapshot` copy-on-promote → restore, identical RAM+FS state) or an **image** (cold-start, independent per-boot init), via optional `baseRef`. RL parallel rollouts / branch-from-common-start. Live-verified; finalizer-backed base reclaim + refCount; no router change. Fan-out hardening in operator **v42** (worker double-booking, CR-delete worker release, per-fork idle policy, pool-count fix — see the PRD's As-built update). Worked example: [`examples/forkset/`](../../examples/forkset/). |
| [PRD-on-demand-suspend.md](../PRD-on-demand-suspend.md) | **Implemented** | Declarative, edge-triggered checkpoint+suspend on request via `Session.spec.suspendRequest` (opaque token) + `status.lastSuspendHandled` watermark. The "save my state now" primitive; a `SessionReconciler` performs one suspend per token, never fighting reactive resume. Live-verified (operator v29). |
| [PRD-broker-fork-session.md](../PRD-broker-fork-session.md) | Proposed | The **example** broker's `fork_session` MCP tool composing the CRDs (suspendRequest → BaseSnapshot → ForkSet → return ids; increment 2 = per-call `target` routing to a fork by id). Reference/example scope — the product API is the CRDs. Depends on on-demand-suspend. |

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

- EKS cluster `EKSClusterStack-cluster`, region `us-west-2`, account `111122223333`.
- gVisor nodes labeled `sandbox=gvisor` with taint `sandbox=gvisor:NoSchedule`.
- Control plane in namespace `sandboxd-controlplane-system`; pools/sessions/workers
  in namespace `default`.
- S3 bucket `aio-checkpoint-spike-111122223333-us-west-2`.
- Public front door `agentgateway.example.com`; identity provider `keycloak.example.com`.

Substitute your own account, cluster, DNS, and bucket where you deploy.
