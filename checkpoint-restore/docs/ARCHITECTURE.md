# Sandbox Checkpoint/Restore — Architecture (substrate-like on agent-sandbox)

Status: Draft (2026-07-01), on the `checkpoint-restore` branch.
Grounded in the proven primitives in [`../NOTES.md`](../NOTES.md); read that first.

## Goal

Decouple a sandbox's **state** from its **pod**. A user session can be
checkpointed to object storage, its pod freed (so we stop paying for idle
capacity), and later **resumed onto any interchangeable warm sandbox pod** — on
any node — with RAM + filesystem state intact. This is agent-substrate's
WorkerPool + suspend/resume model, delivered on the kubernetes-sigs
**agent-sandbox** stack (so we keep Sandbox / SandboxClaim / SandboxWarmPool
semantics and the existing broker/auth front door).

Explicitly NOT in-place resume (that pins state to one always-running pod and
saves no cost).

## What is already proven (see NOTES.md)

- **gVisor `runsc checkpoint`/`restore` works on our EKS nodes** (out-of-band).
- **In-place checkpoint of a live containerd-managed gVisor pod** (python + AIO,
  incl. Chrome) via `runsc --root=/run/containerd/runsc/k8s.io checkpoint
  --leave-running <id>`; pod keeps running.
- **AIO runs correctly under gVisor** (set `runtimeClassName: gvisor` on the
  SandboxTemplate; the RuntimeClass `scheduling` block pins the node).
- **Pod-level checkpoint→restore of a real Sandbox-CRD gVisor pod** (busybox:
  pause + workload), root-first, **each container checkpointed to its own
  image**, restored into a fresh `-root`. RAM + FS continuity proven; restored
  sandbox runs independently of the original.
- **S3 + Pod Identity round-trip** for the checkpoint (only the ~600K–1MB-class
  checkpoint travels; base rootfs stays node-local).

Key mechanics learned:
- A k8s gVisor pod = **one sentry** shared by pause (sandbox ROOT) + workload
  (non-root sub-container). Restore must be **root-first**, and **each container
  needs its OWN checkpoint image** (a single whole-sandbox image reused for both
  causes the `.gvisor.filestore.<sandbox-id>` collision).
- `runsc restore` takes **two** image inputs: `-image-path` (memory) and
  `-fs-restore-image-path` (filesystem delta). Base image layers already exist
  on the warm pod, so only the **rw overlay delta + memory** need travel.
- Off-the-shelf containerd/CRI does **not** expose pod checkpoint/restore
  (`ctr tasks checkpoint` = "not implemented" for `io.containerd.runsc.v1`);
  driving `runsc` directly is required.
- Drive node ops via **SSM Run Command**, not node-shell (node-shell wedges on
  backgrounded `runsc restore`; SSM is async + captures output).

## Target architecture

```
                 broker (existing, per-user)                         S3 (checkpoints)
                    |  claim / resume                                   ^   |
                    v                                                   |   v
   SandboxClaim --> agent-sandbox controller --> warm Sandbox pod   (upload on suspend /
   (per user)        (SandboxWarmPool)            (gVisor, idle)      download on resume)
                                                       ^
                                                       | drives runsc against containerd -root
                                            ckpt-restore controller / node-agent (DaemonSet)
                                              - suspend: checkpoint pod -> S3, free pod
                                              - resume:  pick warm pod -> restore state onto it
```

### Components

1. **Checkpoint/Restore controller + node-agent (NEW — the thing to build).**
   A privileged DaemonSet on gVisor nodes (like the proven `ckpt-shim`) plus a
   small controller, exposing two verbs:
   - **Suspend(sandboxId):** on the node hosting the pod, `runsc checkpoint`
     each container of the pod to its own image (pause img + workload img),
     upload to `s3://…/<sandboxId>/`, then release/delete the pod.
   - **Resume(sandboxId, ontoWarmPodX):** on warm pod X's node, download the
     checkpoint, and restore it **onto pod X** so the K8s Pod object for X now
     runs the resumed sandbox.
   Node-agent binds to the S3 role via Pod Identity (proven `ckpt-spike`
   pattern, generalized).

2. **Warm pool = agent-sandbox `SandboxWarmPool`** of gVisor Sandbox pods, sized
   for concurrency. Reuse as-is; these are the fungible workers.

3. **Broker (existing).** Maps user→checkpoint(S3); on connect, asks the
   controller to resume the user's checkpoint onto a warm pod, then routes MCP
   to it; on idle/disconnect, asks the controller to suspend.

4. **S3 + Pod Identity (exists).** Per-session checkpoint store.

## The gating unknown — SPIKE THIS NEXT, before building the controller

Everything above hinges on one unproven step:

> **Can we restore a checkpoint ONTO a warm, containerd-managed Sandbox pod —
> i.e. into containerd's own `-root` (`/run/containerd/runsc/k8s.io`), replacing
> that warm pod's freshly-booted workload — while the K8s Pod object for the
> warm pod stays valid and kubelet/containerd don't fight us?**

We have proven restore into **our own** `-root`. We have NOT proven restore into
**containerd's** `-root` over a live warm pod. Sub-questions:
- Can `runsc restore` target containerd's `-root` with the warm pod's existing
  container id, or must we stop containerd's workload container first and swap
  the sentry state underneath it? (containerd's shim has its own `wait`/state.)
- Does the warm pod's rootfs/overlay + **gofer-mount-confs** match the
  checkpoint's enough for the workload restore to complete? (For a single
  workload container, e.g. busybox/AIO, the earlier collision was avoided by
  per-container images + a matching rootfs.)
- CPU-feature + runsc-version match across source→target node (pin instance
  family; annotation `dev.gvisor.internal.cpufeatures`).
- Does kubelet see the restored container as the same running container (so it
  doesn't restart/kill it)? If not, does the controller need to own the pod
  outside kubelet's readiness/liveness?

**Fallback if containerd-managed restore proves infeasible:** the warm worker
becomes a privileged pod that owns its own runsc `-root` and runs the sandbox as
a **root** container (substrate's actual mechanism). This restores cleanly (we
proved that class) but sacrifices native Sandbox-CRD semantics for the inner
sandbox — the broker/controller then own routing + lifecycle, as substrate does.
Decision deferred until the spike above resolves.

## Incremental plan

- **T1 — GATING SPIKE (next):** with two busybox Sandbox pods (A = source,
  B = warm target, ideally different nodes): checkpoint A → S3 → **restore A's
  state onto pod B via containerd's `-root`**, and confirm B's K8s Pod object
  survives + serves the resumed state. This proves (or kills) the K8s-native
  model. Use SSM.
- **T2 — suspend frees the pod:** after checkpoint→S3, delete/release pod A;
  confirm state is fully in S3 (no dependency on A's node).
- **T3 — cross-node + cross-gen:** A and B on different instance generations
  (c6a→c7a); validate CPU-feature match / pinning.
- **T4 — AIO:** repeat T1–T3 with the AIO Sandbox (proven checkpointable) as the
  workload; validate MCP hub state + reconnect (W3: `:8080` reachable again,
  broker opens a fresh MCP session — no socket survival needed).
- **T5 — controller:** wrap the proven sequence in the controller + node-agent
  and the broker seam (Suspend/Resume verbs).

## Open design questions

- In-place container swap vs. controller-owned pod (kubelet interaction) — T1
  resolves.
- One workload container assumed; multi-container pods need per-container images
  + ordering (mechanism generalizes but untested).
- Networking: restore rebuilds netstack from the new node's netns; MCP is a
  **reconnect** (fresh session), not socket survival — matches GKE (no network
  guarantee). Bake into the broker's resume path.

---

# RESOLVED MODEL (2026-07-01, after spikes T1/T1c/T2)

The pre-T1 diagram above ("restore onto a warm Sandbox pod") is SUPERSEDED.
T1/T1c proved you cannot restore onto a kubelet-managed pod out-of-band
(kubelet recreates the workload; gVisor won't inject a restored sub-container
into a started sandbox). T2 proved the substrate-style model end-to-end. The
resolved design:

## Worker vs. sandbox — the load-bearing distinction

| Thing | K8s Pod? | Placed/accounted by | Has cluster IP / DNS? |
|---|---|---|---|
| **Worker** | **YES** — normal Pod from a Deployment (`WorkerPool` CRD → Deployment) | **kube-scheduler**, via the worker pod's resource requests | YES (VPC-CNI pod IP; DNS if fronted by a Service) |
| **Sandbox / actor** | **NO** — a runsc ROOT container INSIDE the worker pod | control plane picks *which idle worker* (assignment only) | NO cluster IP, NO CoreDNS name, NO Service |

Everything below follows from this: the schedulable/accountable unit is a
**real Kubernetes pod (the worker)**; the portable stateful unit is the
**sandbox (runsc root container)** that teleports onto workers.

## The controller does NOT reimplement kube-scheduler

Because the worker is a real pod, **kube-scheduler already does all node/
resource reasoning**: the `WorkerPool` Deployment declares `replicas` +
per-worker resource requests; kube-scheduler bin-packs worker pods onto nodes
and reserves capacity. The C/R controller therefore keeps only a lightweight
**assignment table** (substrate: Redis/Valkey), NOT a cluster/resource model:
which workers exist, idle-vs-busy, pool/sandboxClass, and each actor's snapshot
URI. Resume = O(1) "pick any idle worker in a matching pool" → dial its
node-agent to restore. No feasibility/bin-packing math.

**Why it never asks "does this node have room?": one actor per worker.** Each
worker pod is sized (resource requests) for ONE sandbox up front; kube-scheduler
reserved that room when it placed the worker. So when an actor resumes onto an
idle worker, the room exists by construction. **Oversubscription is TEMPORAL,
not spatial** — many actors, fewer workers; idle actors checkpoint to S3 and
free their worker for the next. Capacity scaling = scale the `WorkerPool`
Deployment `replicas` (normal K8s). If no idle worker at resume, wait or scale
the pool up — never place a pod ourselves. (Spatial oversubscription — many
sandboxes per worker — is the ONLY thing that would force per-worker resource
accounting; substrate avoids it and so should we, at least v1.)

## Networking / addressing (grounded in substrate source)

- Worker pod: normal VPC-CNI pod IP; routable; DNS-resolvable if fronted by a
  Service. This is the only real IP.
- Sandbox: lives in a **persistent interior netns** in the worker, connected by
  a **veth pair with a stable hardcoded interior IP** (substrate: `169.254.17.2`
  / `actorVethIP`). Same interior address every resume.
- Path to a sandbox: **worker-pod-IP (routable) → veth → interior IP:port** (e.g.
  AIO `:8080/mcp`). The sandbox has no address of its own.
- **CoreDNS/kube-dns cannot resolve a sandbox** (not a Pod/Service/Endpoint).
  Only workers are DNS-visible. Substrate uses an app-layer name
  `ActorDNSName(atespace, actorId)` as a **Host header**, resolved by its router
  against the assignment table — NOT a DNS record.
- **Sockets do NOT survive restore** (verified for BOTH GKE and substrate;
  substrate tears down/rebuilds the veth around C/R). The stable interior IP
  means the *address* is constant; the *session* must be re-established →
  reconnect model.

## Router & registration

- The current agent-sandbox `sandbox-router` (routes by `X-Sandbox-*`, backed by
  Pod/Service endpoints) will NOT address a non-pod sandbox. We need a router of
  substrate's `atenet-router` class: routes by Host header = sandbox/actor id,
  resolved against the **assignment table** to `worker-pod-IP` (+ interior IP).
- "Registration" is NOT a K8s endpoints watch — it's the **resume workflow
  writing `actorId → workerPod, workerPodIP, interiorIP` into the store**; the
  router (and broker) read that store. On suspend the assignment is cleared.

## Broker → sandbox MCP path

- Broker → **router** over HTTP with Host/header = actor/sandbox id.
- Router resolves id → worker pod IP (store), forwards; worker proxies to the
  sandbox interior IP:port.
- Every resume = a **fresh MCP session** (reconnect + `initialize`); no live
  TCP/SSE survives. Broker owns reconnect.

## Claims — mostly dissolve

- agent-sandbox `SandboxClaim` binds a user to a SPECIFIC pod because state lives
  in that pod, with TTL/release to reclaim. **In this model state is portable
  (S3), so no user is bound to any pod.** Workers are fungible.
- Substrate has **no claim/release**: one **durable Actor per principal** +
  **resume-time worker assignment**. The tracked unit for quota/lifecycle is the
  **actor + its S3 snapshot**, not a pod. Reclamation = delete idle actors'
  snapshots (broker-owned; substrate has no GC).

## Restore latency — MEASURED (busybox, warm worker, 360K checkpoint, 2026-07-01)

| Phase | Time |
|---|---|
| `runsc checkpoint` | ~91 ms |
| `runsc create` (gofer/sandbox spin-up) | ~268 ms |
| `runsc restore` call returns | ~91 ms |
| restore → usable (`exec` succeeds) | ~74 ms |
| **create + restore + usable** | **~434 ms** |

- Sub-500ms for a trivial workload; counter resumed correctly.
- **`create` dominates (~270ms)** → a truly warm worker can **pre-`create`** an
  idle container so resume ≈ just the restore (~150ms).
- Excludes **S3 download** (small for busybox; **seconds for AIO ~600M** → the
  argument for fs-delta+memory-only, and for keeping the base image node-local).
- **Memory size**: 360K here. AIO ~600M adds page-load, but gVisor **lazy-loads
  pages** so "usable" can precede full residency; steady-state warms as pages
  fault in. Measure AIO specifically (T4).

## AIO (real workload) — MEASURED (2026-07-01)

Full AIO gVisor sandbox (Chrome + MCP hub + Jupyter + code-server + Xvnc):
| Phase | Value |
|---|---|
| `runsc checkpoint` (AIO, --leave-running) | **~3.04 s** |
| checkpoint image size | **696M** (checkpoint.img 6.6M + pages.img 723M + meta 136K) |
| S3 upload (696M, shim pod / Pod Identity) | **~3.08 s** |
| S3 download (696M) | **~4.56 s** |

Implications:
- The **S3 round-trip (~7.6s for 696M) dominates** the real-workload resume, NOT
  the runsc ops. This is THE argument for shipping only **fs-delta + memory
  actually touched** (not a full 696M image) and for keeping the base image
  node-local. A per-session delta is far smaller than the full RAM dump.
- Checkpoint itself (~3s) is fine for suspend-on-idle.
- For fast resume: pre-warm workers (pre-`create`), and consider keeping hot
  actors' checkpoints on a faster tier or node-local cache, S3 for cold.
- Not yet measured: AIO *restore*-to-usable (blocked on AIO first-boot bootstrap
  under a standalone root container — the golden-checkpoint approach). gVisor
  lazy page-load means usable-time should be well under full-download time once
  the image is local.

## Control-plane shape to build (mirror substrate, gVisor-only, EKS+S3)

- **WorkerPool CRD → Deployment** of worker pods (each owns its runsc `-root`;
  privileged; one sandbox at a time; `sandboxClass: gvisor`). kube-scheduler
  places them.
- **Lifecycle/scheduling controller** (≈ substrate `ateapi`): Create/Resume/
  Suspend/Delete actor; resume-time idle-worker selection; assignment + snapshot
  URIs in a store (ElastiCache/Valkey). Owns GC (delete idle snapshots).
- **Node-agent** (≈ `atelet`+`ateom`; DaemonSet, privileged, hostPath): the
  proven runsc driver (checkpoint each container to its own image, root-first
  restore, `-overlay2=root:self`, no `-direct` on overlay, `-detach`), + S3 I/O
  (Pod Identity). We prototyped this via SSM+shim.
- **Router** (≈ `atenet-router`): Host-header→worker-pod routing from the store.
- **Broker** (exists): per-user actor mapping, resume-on-connect, reconnect MCP,
  quota over actors, suspend-on-idle.
