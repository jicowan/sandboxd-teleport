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
