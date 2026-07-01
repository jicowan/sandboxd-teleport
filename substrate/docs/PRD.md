# PRD — AIO Sandbox on Agent Substrate (via broker)

Status: Draft
Branch: `substrate`
Related: [`DESIGN-NOTES.md`](./DESIGN-NOTES.md), and (on `main`) the
agentgateway design that this reuses for the auth front door.

## 1. Problem / goal

Run the existing AIO sandbox (the AIO all-in-one image and its built-in MCP hub
of ~31 tools) on **agent-substrate** instead of the kubernetes-sigs
agent-sandbox controller, so that per-user agent sessions get substrate's
**suspend/resume snapshot** model (hibernate idle sessions, resume sub-second,
high pod-oversubscription) — while preserving the current client experience:
Claude Code connects as a remote MCP server, authenticated once via
Keycloak/agentgateway, and uses the same 31 tools.

**In scope (MVP):** a Substrate-native broker + Substrate deploy resources that
let an authenticated user drive an AIO sandbox actor over MCP end-to-end.

**Out of scope (MVP):** replacing the `main` (agent-sandbox) deployment;
multi-cluster; production SLAs; anything substrate itself doesn't yet ship
(GC, native MCP hosting, control-plane auth).

## 2. Success criteria (MVP "done")

A user running Claude Code, authenticated through the existing agentgateway +
Keycloak front door, can:
1. Connect to the MCP endpoint and see the AIO hub's tools (`tools/list`).
2. Call `sandbox_execute_bash` (and at least one `browser_*` and one
   `sandbox_file_operations` tool) and get correct results, executed inside a
   **Substrate actor** running the AIO image.
3. Disconnect; the actor **suspends** (snapshot taken, worker freed).
4. Reconnect; the actor **resumes** with filesystem state intact (prove state
   persistence: write a file, disconnect, reconnect, read it back).
5. All of the above gated by the `sandbox-users` group and per-user quota,
   exactly as on `main`.

## 3. Hard prerequisites (gates)

- **G1 — gVisor on EKS:** substrate's runsc checkpoint/restore must run on EKS
  worker nodes. If it cannot, the entire PRD is blocked; escalate before any
  broker work.
- **G2 — MCP session continuity through atenet:** atenet-router must carry
  Streamable-HTTP MCP (incl. `Mcp-Session-Id` / SSE) to the actor, or we need a
  documented workaround (broker-owned session affinity). Determines whether the
  broker's session→actor model works.
- **G3 — snapshot storage on EKS:** substrate must snapshot to S3 (or another
  store available on EKS), not only GCS.

## 4. Architecture (target)

```
Claude Code --MCP+OAuth--> agentgateway (Keycloak inbound, JWT passthrough)   [reused from main]
                               |
                               v
                        substrate-broker (Go)     [NEW]
                          - validates JWT (JWKS), sandbox-users group, quota
                          - MCP initialize -> ResumeActor(atespace, user-actor)   via ate-api-server gRPC
                          - forward tools/list, tools/call as MCP over HTTP
                              to atenet-router, Host: ActorDNSName(atespace,id)
                          - MCP DELETE / idle -> SuspendActor(...)
                          - owns deletion + stale-snapshot cleanup (no substrate GC)
                               |
                               v
                        atenet-router --> AIO actor (:8080 /mcp)   [AIO image as ActorTemplate]
```

Decision (see DESIGN-NOTES "Option B"): **the AIO image runs AS the actor,
exposing its `/mcp` hub** — broker does MCP pass-through, not MCP↔`/process`
translation. Keeps all 31 tools transparent.

## 5. Components to build

| # | Component | Notes |
|---|---|---|
| C1 | `WorkerPool` manifest | warm worker capacity (≈ SandboxWarmPool) |
| C2 | `ActorTemplate` manifest | wraps the AIO image + snapshot config (S3) |
| C3 | `substrate-broker` (Go) | lifecycle via ate-api-server gRPC + MCP data-plane forward to atenet; reuses the auth logic from the `main` broker (JWKS validate, group gate, per-user quota) |
| C4 | agentgateway target/route | point the existing gateway at the substrate-broker (reuse Keycloak, passthrough) |
| C5 | RBAC / ServiceAccount | broker's permissions to call ate-api-server / create actors |

## 6. Stages (execute in order)

Each stage is independently verifiable and ends with a demo/checkpoint. If any
stage's doc grows too large, split it into its own PRD (e.g. `PRD-broker.md`).

- **Stage 0 — Substrate on EKS + gate verification.**
  Install substrate on the (or a parallel) EKS cluster. Verify **G1** (run the
  stock `demos/sandbox` actor: create, `/process`, suspend, resume, confirm
  state persists) and **G3** (S3 snapshots). Deliverable: a working stock demo
  on EKS + a go/no-go note on G1/G3. Also confirm the privileged gVisor node
  DaemonSet (`atelet` / `ateom-gvisor`) **coexists with existing cluster
  workloads** without destabilizing nodes (we're installing into the shared
  production cluster, isolated by namespace + RBAC). **This is the make-or-break
  stage — do it before writing broker code.**

- **Stage 1 — AIO image as an actor.**
  Build C2 (ActorTemplate wrapping the AIO image) + C1 (WorkerPool). Create the
  actor, port-forward atenet, and hit the AIO `/mcp` endpoint **directly**
  (curl `initialize` + `tools/list` + one `tools/call`) through atenet's Host
  routing. Verify **G2** (MCP session continuity) here with raw curl before any
  broker exists. Deliverable: AIO hub reachable as an actor over MCP.

- **Stage 2 — Substrate broker (lifecycle + forward).**
  Build C3: the Go broker. MCP `initialize` → **create-or-resume the caller's
  durable per-user actor** (one actor per principal, keyed on the JWT
  principal; reused across sessions); forward tools/list/call to atenet; MCP
  `DELETE`/idle → SuspendActor (snapshot, keep the actor). No auth yet — trust
  localhost, test with a raw MCP client. Deliverable: broker drives
  create-or-resume + suspend + forwarding end to end, and a second session for
  the same principal resumes the *same* actor.

- **Stage 3 — Auth + quota (reuse main's logic).**
  Port the `main` broker's JWKS validation, `sandbox-users` group gate, and
  per-user quota into C3. Add broker-owned actor deletion + stale-snapshot
  cleanup (substitutes for substrate's missing GC/TTL). Deliverable: only
  in-group users can drive actors; quota enforced.

- **Stage 4 — Front door (agentgateway) + Claude Code.**
  Build C4/C5: point agentgateway at the substrate-broker (reuse Keycloak
  realm + `aio-sandbox-client`), register in Claude Code as a remote HTTP MCP
  server, run the success-criteria flow (§2) end to end, including the
  suspend→resume state-persistence proof. Deliverable: MVP complete.

## 7. Risks (from DESIGN-NOTES)

- Substrate is v0.0.0; APIs may break under us mid-build. Pin a commit SHA.
- `internal/resources.ActorDNSName` is internal — vendor or reimplement the
  actor DNS scheme (pushes broker to Go).
- No substrate GC/TTL — broker must own all reclamation.
- Session continuity (G2) is the highest-uncertainty technical risk after G1.

## 8. Resolved decisions

- **Cluster: existing EKS cluster, dedicated namespace** (not a parallel
  cluster). Substrate installs into its own namespace (e.g. `ate-system`) with
  demo/actor resources in a separate namespace (e.g. `aio-substrate`). Accept
  the blast-radius of substrate's privileged node DaemonSet (`atelet` /
  `ateom-gvisor`) on the shared cluster; isolate everything else by namespace +
  RBAC. Stage 0 must confirm the gVisor DaemonSet coexists with existing
  workloads without destabilizing nodes.
- **Actor model: one durable actor per user**, reused across sessions (not one
  per MCP session). This is the point of substrate: MCP `initialize` →
  create-or-resume the user's actor; disconnect → suspend (snapshot); reconnect
  → resume with RAM+FS state intact. Quota is measured in **actors per user**;
  reclamation is **idle-actor cleanup** owned by the broker (no substrate GC/TTL).
  The success-criteria state-persistence proof (§2.4) exercises exactly this.

## 9. Open questions

- Go module strategy for the broker: import substrate's `pkg/proto/ateapipb`
  (public) but reimplement `internal/resources` (`ActorDNSName`) bits.

## 10. If this PRD gets too big

Split by stage into: `PRD-0-eks-gate.md`, `PRD-broker.md` (Stages 2–3),
`PRD-frontdoor.md` (Stage 4). Keep this file as the umbrella/index.
