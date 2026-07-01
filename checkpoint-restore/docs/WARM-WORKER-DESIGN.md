# Warm Worker Pod — Design Sketch

Status: Draft / early sketch (on `checkpoint-restore` branch)
Depends on: the proven primitives in `../NOTES.md` (atomic runsc checkpoint/
restore on EKS gVisor nodes; S3 round-trip via Pod Identity).

## Why this shape (recap of the corrected model)

The spike proved the *primitives* but disproved the "cold pod from snapshot"
topology: a checkpoint references a gofer-backed (9p) rootfs, and a freshly
created pod's mount namespace can't reconstruct it (`vfs.CompleteRestore …
9p failed to walk`). Substrate's answer — which we adopt — is a **persistent
worker pod** that owns the runsc `-root`, gofer, and netns, and does
`runsc create` + `runsc restore` *into its own environment* on demand. The pod
is the durable substrate; the sandbox state teleports onto it.

So we are building a minimal "ateom-for-us": a warm worker that hosts one AIO
sandbox at a time and can checkpoint it to S3 and restore it from S3.

## Scope of this design

- **In:** the warm worker pod itself — how it's shaped, how it drives runsc
  out-of-band, checkpoint→S3 and S3→restore, and the control surface the broker
  will call.
- **Out (later):** broker integration, MCP session routing/continuity, quota,
  the WorkerPool controller/autoscaling, multi-container sandboxes.
- **Assumption to keep testing:** the AIO image runs correctly under gVisor
  (separate gate; not proven yet — see Risks).

## Components

```
┌────────────────────────── warm worker pod (privileged, gvisor node) ─────────┐
│  runrunner (our agent, Go or Python)          state on hostPath / emptyDir   │
│   - HTTP/gRPC control API (create/checkpoint/restore/delete/status)          │
│   - drives /usr/local/bin/runsc out-of-band against its OWN -root <dir>      │
│   - fetches/pushes checkpoints from S3 (Pod Identity SA)                      │
│   - owns the OCI bundle (rootfs + config.json) for the AIO sandbox           │
│                                                                               │
│   runsc -root /run/ckpt --overlay2=root:self  create|start|checkpoint|restore │
│        └── gVisor sandbox = the AIO image (MCP hub :8080)                     │
└───────────────────────────────────────────────────────────────────────────────┘
        ▲ control (broker → worker)              │ MCP (broker → sandbox :8080)
        │                                          ▼
     broker (existing)                        AIO MCP hub
```

### Worker pod spec (from substrate's proven shape)
- `securityContext.privileged: true`, `runAsUser: 0` (runsc needs it).
- `nodeSelector: sandbox=gvisor` + toleration (our gVisor node group; runsc
  release-20260622.0 already on-node at `/usr/local/bin/runsc`).
- hostPath (or large emptyDir) for the runsc `-root` state + overlay upper +
  checkpoint staging. substrate uses hostPath `/var/lib/ateom-gvisor`.
- ServiceAccount with Pod Identity → S3 (our `ckpt-spike` role, generalized).
- Mount the node's runsc binary (hostPath `/usr/local/bin/runsc`) OR bundle a
  pinned runsc in the image. **Version must match across checkpoint/restore**
  (gVisor hard-errors on mismatch) → pin it.
- CPU-feature pinning: keep all worker nodes on one instance family (c7a today)
  or set `dev.gvisor.internal.cpufeatures` so restore doesn't abort.

### runrunner control API (the seam the broker calls)
Minimal verbs, each mapping to proven runsc calls against the worker's `-root`:
- `POST /sandbox`        → `runsc create -bundle <B> --fs-restore… ` + `start`
                           (cold start of a fresh AIO sandbox)
- `POST /checkpoint`     → `runsc checkpoint -image-path <dir>`; then upload
                           `<dir>` to `s3://…/<sandbox-id>/` ; then `delete`
- `POST /restore`        → download `s3://…/<sandbox-id>/` → `runsc create`
                           + `runsc restore -image-path <dir> -background -detach`
- `DELETE /sandbox`      → `runsc delete -force`
- `GET /status`          → `runsc state`

All the exact flags are in `../NOTES.md` (overlay2, no `-direct` on tmpfs,
`restore -bundle -pid-file -background -detach`).

## The two hard problems (honestly)

1. **Networking across restore.** gVisor rebuilds the netstack from the restore
   host's netns; connected sockets break. The MCP hub listens on a port inside
   the sandbox — after restore the worker must re-expose that port. Substrate
   builds a point-to-point veth (pod netns ↔ per-actor interior netns) at
   worker start and the actor keeps its interior IP across restore. We will
   likely need the same: a stable interior netns/veth the sandbox binds to, so
   the broker's route to `:8080` survives a checkpoint/restore cycle. **This is
   the biggest unsolved piece and should be the next spike after a bare
   in-pod restore works.**

2. **Bundle/rootfs identity across restore.** The restore `create` must use a
   bundle whose rootfs is equivalent to the checkpoint's (gVisor validates the
   OCI spec shape). Simplest: the worker always uses the *same* pinned AIO
   bundle for both the original create and the restore create. Overlay
   (`root:self`) keeps the writable layer in the checkpoint.

## Warm vs. per-restore

- A worker hosts **one sandbox at a time** (matches substrate: a Worker hosts
  at most one Actor). "Warm" = the pod is pre-started and idle, ready to
  `create`+`restore` fast — not that it holds a running sandbox.
- Per-user durable model (from the substrate PRD): the broker keeps a
  user→checkpoint(S3) mapping; on resume it picks a warm worker, tells it to
  restore that user's checkpoint. On idle/disconnect it tells the worker to
  checkpoint→S3 and frees the worker back to the warm pool.

## Incremental build plan (each step independently verifiable)

- **W1 — in-pod restore (no S3, no net):** one persistent privileged pod;
  inside it, `create`+`start`+`checkpoint`+`delete`+`create`+`restore` against
  its own `-root` (exactly 1a, but *inside a running pod* rather than via
  node-shell). Proves the 9p issue is gone when create+restore share the pod's
  mount ns. **This is the direct next experiment.**
- **W2 — S3 in the loop:** worker checkpoints → S3, a *second* warm worker pod
  restores from S3. Proves teleport across pods (the thing cold-restore
  couldn't do), given both are persistent workers that create their own gofer.
- **W3 — networking:** make the sandbox's `:8080` reachable and survive a
  checkpoint/restore cycle (veth/interior-netns like substrate). Hardest.
- **W4 — AIO under gVisor:** swap the busybox test workload for the real AIO
  image; confirm the 31 tools work under runsc, then checkpoint/restore it.
- **W5 — control API + broker seam:** wrap W1–W4 in the runrunner control API;
  leave broker integration for a later phase.

## Open questions

- Bundle the AIO rootfs into the worker image, or build the OCI bundle at
  worker start from the AIO image (like the spike does with busybox via `ctr`)?
- runsc binary: mount from node vs. bundle pinned — pinning is safer for the
  version-match constraint but must match what the node's containerd shim uses
  if we ever mix paths.
- Is `hostPID`/hostPath acceptable long-term on the shared prod cluster, or do
  we want a dedicated node group for workers?
