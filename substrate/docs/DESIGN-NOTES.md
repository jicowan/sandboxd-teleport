# Substrate Adaptation — Design Notes

Research + analysis backing the decision to run the AIO sandbox on
[agent-substrate](https://github.com/agent-substrate/substrate) via a
Substrate-native broker. This is the "why / what we learned" doc; the PRD
(`PRD.md`) is the "what we'll build."

Sources are the substrate repo's own README / docs/architecture.md /
docs/glossary.md / demos/sandbox (primary), verified via a fan-out deep-research
pass (18 sources, 25 claims adversarially 3-vote verified: 22 confirmed, 3
refuted). Caveat throughout: substrate is **v0.0.0 (released 2026-05-19), "VERY
early development," APIs "almost guaranteed to change."** Every component/CRD
name below may drift.

## What Substrate is

A Kubernetes **orchestration / compute-runtime layer** ("not an SDK for building
agents… a system for running them at scale"). Its defining model is the
*inverse* of agent-sandbox: rather than one dedicated pod per session, it
multiplexes many stateful **actors** onto a smaller pool of long-running
**worker** pods, using **gVisor checkpoint/restore (runsc)** to snapshot an
actor's **RAM + filesystem** to object storage (GCS/S3) and resume it sub-second
on any worker. Claims 30x+ oversubscription.

## Primitive mapping (agent-sandbox → substrate)

| agent-sandbox (today) | substrate |
|---|---|
| `SandboxWarmPool` | `WorkerPool` (CRD → Deployment via `atecontroller`) |
| `SandboxTemplate` | `ActorTemplate` (immutable, versioned; container image + snapshot config) |
| `SandboxClaim` → Sandbox pod | `Actor` (instance of ActorTemplate) on a `Worker` |
| `sandbox-router` (routes by `X-Sandbox-*`) | `atenet` / `atenet-router` (routes by `Host` header = actor DNS name) |
| claim / release + TTL | **suspend / resume snapshot** (no TTL, no claim/release, no GC yet) |

Control plane: `atecontroller` (reconciles CRDs), `ate-api-server` (owns Actor
lifecycle + scheduling, gRPC), `atelet` (node DaemonSet), `ateom-gvisor`
(runsc checkpoint/restore), `kubectl-ate` CLI. Dynamic Actor/Worker state lives
in ValKey/Redis, not etcd.

## Key findings that shape the build

1. **MCP is aspirational, not shipped.** README lists "Deploy secure, sandboxed
   MCP servers as Substrate Actors," but a stronger "natively supports hosting
   MCP servers" claim was **refuted 0-3**. The shipped sandbox demo exposes a
   plain `POST /process` endpoint (bespoke JSON, not MCP); repo-wide search for
   "MCP" = 0 code files. No reference MCP-on-substrate implementation exists.
2. **No auth / multi-tenancy / GC.** architecture.md has a "Problems We Haven't
   Addressed Yet" section listing control-plane auth/authz and identity/policy;
   GC "is not implemented yet." Our entire Keycloak + agentgateway + JWT +
   group-gate + quota layer has **no substrate counterpart** — it stays ours,
   in front.
3. **gVisor-on-EKS is an unverified hard prerequisite.** Whether runsc
   checkpoint/restore runs on EKS worker nodes (kernel / privileged DaemonSet
   requirements) is unproven and gates the entire approach.

## The demo client (`demos/sandbox/client/main.go`) — what it teaches

The demo client does **two jobs**, and both belong in the broker, not in Claude:

1. **Lifecycle (gRPC → `ate-api-server`):** `ResumeActor(ActorRef{Atespace,
   Name})` on startup; `SuspendActor(...)` deferred on exit. Uses
   `pkg/proto/ateapipb` gRPC client over TLS.
2. **Data plane (HTTP → `atenet-router`):** `POST http://<atenet>/process` with
   `Host: resources.ActorDNSName(atespace, actorID)` — the Host header is how
   atenet routes to the target actor. Body is bespoke JSON
   (`{command, envvars, cwd, timeout}`), **not MCP**.

Flags: `--id` (actor id), `--atespace`, `--ateapi` (gRPC, default :8080),
`--atenet` (HTTP router, default :8000).

### Implications
- **Claude is NOT a Substrate client.** Claude speaks MCP to agentgateway; the
  broker translates MCP into Substrate operations. The demo client is the
  reference for *the broker's Substrate-facing half*, not a component Claude
  runs. The client's two jobs map onto what the broker already does today:
  lifecycle (was k8s SDK `create/delete_sandbox` → becomes ate-api-server
  `Resume/SuspendActor`) and data-plane forwarding (was sandbox-router +
  `X-Sandbox-*` → becomes atenet-router + `Host: ActorDNSName`).
- **`/process` is not MCP** — this is the fork:

  - **Option A — broker translates MCP ↔ `/process`.** Broker converts each
    MCP `tools/call` into the actor's `/process` JSON and wraps the result back
    into MCP. Rejected: `/process` is command-exec only; we'd lose the AIO
    hub's ~31 tools (browser, files, code, markdown) — reduced to shell exec.
  - **Option B — run the AIO image AS the actor, exposing `/mcp` (CHOSEN).**
    The ActorTemplate wraps the AIO image; the actor exposes the built-in MCP
    hub on :8080 just like today. The broker does pure MCP pass-through to
    atenet-router (Host = actor DNS), exactly as it forwards to sandbox-router
    now. All 31 tools come through transparently; the broker's data plane is a
    near-identical swap. The demo's `/process` server is simply replaced by the
    AIO server.

### Broker behavior per MCP session (Substrate-native, Option B)
Actor model (decided): **one durable actor per user**, reused across sessions —
keyed on the passthrough JWT principal, not per MCP session.
1. MCP `initialize` → create-or-resume the caller's durable actor
   `ResumeActor(atespace, actorID=principal)`.
2. Forward `tools/list` / `tools/call` as MCP-over-HTTP to atenet-router with
   `Host: ActorDNSName(atespace, actorID)`.
3. Session end / MCP `DELETE` → `SuspendActor(...)` — **hibernate, not destroy**
   (the RAM+FS snapshot persists for instant resume; this is substrate's whole
   value).
4. agentgateway + Keycloak + JWT + group-gate + quota stay in front, unchanged.

## Open risks / must-verify before/while building

1. **gVisor runsc checkpoint/restore on EKS worker nodes** — hard gate; nothing
   works if this fails. Verify first.
2. **Does atenet-router preserve MCP session semantics** (Streamable HTTP,
   `Mcp-Session-Id`, SSE) through Host-header routing? The demo only proves
   stateless `/process` POSTs. Same session-continuity risk we hit with
   AgentCore Gateway.
3. **`internal/resources.ActorDNSName` is an internal package** — not importable
   from outside the substrate Go module. Broker must vendor it or reimplement
   the DNS-name scheme. Pushes the broker toward Go (matching substrate).
4. **No GC / no TTL in substrate** — the broker must own actor deletion and
   stale-snapshot cleanup entirely (quota + idle reclamation are broker jobs).
5. **Snapshot storage on EKS** — demo uses GCS; glossary says S3 supported;
   unverified in practice.

## Stance

Prototype-alongside, not replace. The density/hibernation model is genuinely
better for bursty per-user agent sessions, but v0.0.0 + no auth + no shipped
MCP + unverified-on-EKS gVisor make substrate a high-risk *production*
foundation today. Keep the working agentgateway/agent-sandbox stack on `main`;
develop the substrate path on the `substrate` branch behind the same auth front
door.
