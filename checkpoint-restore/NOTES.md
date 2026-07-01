# Checkpoint/Restore Spike — Notes

Goal: prove gVisor `runsc checkpoint`/`restore` (driven out-of-band, mirroring
substrate's `cmd/ateom-gvisor`) works on our EKS nodes — checkpoint a running
gVisor container, restore it with RAM+FS state intact, same-node then
cross-node (1a), then restore into a new pod via S3 (1b).

## Environment (confirmed 2026-07-01)

- gVisor nodes (labeled `sandbox=gvisor`, both **c7a.medium / AMD**, CPU-matched):
  - `ip-10-0-4-68.us-west-2.compute.internal`
  - `ip-10-0-5-176.us-west-2.compute.internal`
- `runsc` on node: `/usr/local/bin/runsc`, **version release-20260622.0**
  (spec 1.2.1) — the exact release substrate pins.
- containerd 2.2.4, Amazon Linux 2023, kernel 6.1.
- Node access: `kubectl debug node/<name>` (privileged; host at `/host`).
- Reference pattern: `~/GitHub/Projects/substrate-upstream/cmd/ateom-gvisor/runsc.go`
  (SHA 4aedeab). runsc calls:
  - create:     `runsc -root <dir> create -bundle <bundle> -pid-file <pf> <name>`
  - start:      `runsc -root <dir> start <name>`
  - checkpoint: `runsc -root <dir> checkpoint -image-path <path> <name>`
  - fscheckpt:  `runsc -root <dir> fscheckpoint -image-path <path> -path <mount> <name>`
  - restore:    `runsc [-allow-connected-on-save] -root <dir> restore -image-path <path> <name>`

## 1a — raw checkpoint/restore (node-local, no S3, no pod)

### Same-node MEMORY checkpoint/restore: WORKS ✅ (2026-07-01, node ip-10-0-4-68)

Proven: a gVisor container's in-RAM state survives checkpoint→restore. Test
workload increments an in-memory counter every 2s; after restore the counter
**continued from where it was checkpointed** (5 → 6 → 7…), not reset. Container
returns to `running`.

**Working recipe (memory-only), driven out-of-band (no containerd):**
```
R="runsc -root <stateDir> --network=none"
# build OCI bundle: busybox rootfs (ctr image mount in a private ns + tar) +
#   runsc spec, edit process.args to the workload, root.path=rootfs
$R create  -bundle <bundle> -pid-file <pf> demo
$R start   demo
# ... let it run / accumulate state ...
$R checkpoint -image-path <memdir> demo          # -> checkpoint.img, pages.img, pages_meta.img
$R delete -force demo
$R create  -bundle <bundle> -pid-file <pf2> demo2
$R restore -bundle <bundle> -image-path <memdir> -pid-file <pf2> -background -detach demo2
```

**Gotchas learned (cost several iterations):**
- **No `-direct`** on restore: `/tmp` (tmpfs/overlay) rejects O_DIRECT →
  `opening pages file ... invalid argument`. Substrate uses `-direct` but on an
  O_DIRECT-capable fs.
- `restore` needs `-bundle` + `-pid-file -background -detach` (mirrors
  substrate's `cmdRestore`); a bare `restore -image-path` fails.
- Drive it all against your own `-root <dir>` — never touch containerd's
  `/run/containerd/runsc/k8s.io` (the live python-warmpool gVisor pods live
  there).

### filesystem+memory combined: NOT yet working ⚠️
`fscheckpoint -leave-running` (fs, `all-tmpfs`) + `checkpoint` (mem) into
**separate dirs** (they both emit `pages.img` — same dir collides), then
restore via `create -fs-restore-image-path=<fsdir>` + `restore -image-path=<memdir>`
→ gVisor **panics in `pgalloc/save_restore.go` (nil deref), reports EOF**. Likely
cause: fscheckpoint and checkpoint are taken at *different instants* (container
keeps running between them under `-leave-running`), so fs and mem images are
inconsistent. Deferred — for the real use case the sandbox's durable filesystem
is a PVC/rootfs, not tmpfs, so this specific combination may not be the path we
need. Revisit when designing the real component.

### Operational lessons
- Background `runsc restore &` inside node-shell can wedge the node-shell
  (nsenter) pod in Terminating and block new node-shell pods. Use `-detach`,
  bounded sleeps, and an EXIT trap that force-deletes `demo`/`demo2` and
  `pkill -f "root=<stateDir>"`.
- Run the whole sequence as ONE node-shell invocation writing to a result file,
  then read the file — don't interleave background procs across invocations.

### Order of operations (reference)

**Checkpoint (out-of-band, no containerd):**
```
R="runsc -root <stateDir> --network=none"
# (container already created+started via: $R create -bundle <bundle> -pid-file <pf> <name>; $R start <name>)
# 1. filesystem checkpoint (tmpfs mounts), leave the container running:
$R fscheckpoint -leave-running -image-path <FSDIR> -path all-tmpfs <name>
# 2. memory checkpoint (into a SEPARATE dir — both emit pages.img and collide if shared):
$R checkpoint -image-path <MEMDIR> <name>
# 3. $R delete -force <name>
```
Artifacts: FSDIR = `fscheckpoint.json multitar.img pages.img pages_meta.img`;
MEMDIR = `checkpoint.img pages.img pages_meta.img`.

**Restore:**
```
# 1. create the container, pointing at the filesystem image:
$R create -bundle <bundle> -fs-restore-image-path <FSDIR> -pid-file <pf> <name>
# 2. restore memory (NO -direct on tmpfs/overlay; needs -bundle -pid-file -background -detach):
$R restore -bundle <bundle> -image-path <MEMDIR> -pid-file <pf> -background -detach <name>
```

**Status of each:**
- MEMORY-only checkpoint→restore: **works** (counter continuity proven).
- FS+MEMORY combined: **panics** in gVisor `pgalloc/save_restore.go` (nil deref /
  EOF) — fs and mem images taken at different instants under `-leave-running`
  are inconsistent. OPEN. Likely need to checkpoint mem first (freezes the
  process) then fscheckpoint the now-frozen fs, or take both atomically; TBD.
  For the real component the durable fs is a PVC/rootfs, so this exact tmpfs
  combination may not be the path we ship.

### cross-node restore: (deferred into 1b — needs S3; node-shell stdout truncates
multi-MB blobs, confirming a shared object store is required. Both gvisor nodes
are identical c7a, so the CPU-match precondition already holds.)

## 1b — restore into a new pod + S3

### S3 + Pod Identity foundation: DONE ✅ (2026-07-01)

- **Bucket:** `aio-checkpoint-spike-820537372947-us-west-2` (us-west-2, private
  / public-access-block on, versioning on).
- **IAM role:** `aio-checkpoint-spike-role` — trust `pods.eks.amazonaws.com`
  (`sts:AssumeRole`+`sts:TagSession`); inline policy `s3-checkpoints`:
  `s3:ListBucket` on the bucket + `s3:GetObject/PutObject/DeleteObject` on
  `/*`. Least-privilege, scoped to the one bucket.
- **ServiceAccount:** `default/ckpt-spike` (dedicated — NOT the sandbox SA).
- **Pod Identity association:** `default/ckpt-spike -> aio-checkpoint-spike-role`
  (id `a-x8250humihcnivrdk`).
- **Verified:** a pod using `ckpt-spike` wrote/listed/deleted in the bucket via
  the AWS default cred chain (Pod Identity), no static keys. `S3_OK`.

**Design note — who touches S3:** in the out-of-band model the SANDBOX pod does
NOT do checkpoint I/O; the component driving runsc does (a privileged
node-agent/DaemonSet like substrate's `atelet`; for this spike, the
restore-shim pod). So S3 access binds to that component's SA (`ckpt-spike`),
NOT the sandbox's SA. Current sandbox pods run as `default` in `default` ns —
we deliberately did not grant `default` any S3 access.

### gVisor docs review — corrects our approach (2026-07-01)

Primary sources: gvisor.dev/docs/user_guide/checkpoint_restore + fs_snapshot,
and google/gvisor runsc integration tests (`runsc/container/container_test.go`).

**1. Our fs+mem panic was self-inflicted — use ONE atomic checkpoint.**
gVisor has two distinct filesystem models; we mixed them:
  - **`runsc checkpoint` alone**, on a rootfs that is a **disk-backed tmpfs
    overlay** (runsc default `-overlay2`), captures memory + filesystem
    **atomically in one snapshot**. ← the intended path; adopt this.
  - `runsc fscheckpoint` is a *separate, experimental* fs-only feature. Pairing
    it with a separately-timed `checkpoint` produces inconsistent images →
    the `pgalloc/save_restore.go` panic (an internal self-consistency check).
  **Action: drop the fscheckpoint dance; rely on a single `runsc checkpoint`
  with an overlay/disk-backed-tmpfs rootfs.**

**2. Restore into a NEW container/pod is officially supported — raw runsc.**
gVisor's own `TestCheckpointRestoreHostname` restores into a NEW container ID
with a fresh bundle + spec. Canonical sequence (matches ours):
`runsc create <new-id>` then `runsc restore --image-path=<dir> <new-id>`.
  - **Must match** across checkpoint/restore host: runsc **version** (hard
    error otherwise), **CPU features** (pin via annotation
    `dev.gvisor.internal.cpufeatures`, else restore aborts on the feature
    check), and the OCI **spec shape** (Mounts, Process, Namespaces,
    Annotations); container **name** must be stable (save/restore match by name).
  - **May differ:** `Root.Path` (bundle path), hostname, env, cgroupsPath,
    resources → so a different node/pod is fine. `RestoreSpecValidation` can be
    set to `warning` during bring-up to loosen matching.

**3. No containerd/Kubernetes restore path exists.** gVisor docs: "checkpoint/
restore functionality is currently available via raw `runsc` commands." No
shim/CRI handler. So driving runsc out-of-band is the ONLY way (validates the
substrate pattern), not a workaround. GKE pod-snapshot almost certainly wraps
this same primitive (no public confirmation).

**4. Networking is rebuilt from the new node's netns on restore** (hostinet
unsupported for save/restore; connected sockets break across nodes). Irrelevant
to the spike; matters for MCP session continuity in the real design.

### Atomic mem+fs checkpoint/restore: WORKS ✅ (2026-07-01, supersedes fscheckpoint)

Single `runsc checkpoint` on an **overlay rootfs** captures memory AND
filesystem atomically. Proven: counter continuity (RAM 5→6→7→8) AND file
continuity (`/state/log` went 5 lines pre → 8 lines post, i.e. the pre-
checkpoint lines survived and the restored process appended). No fscheckpoint,
no time-skew panic.

**Working recipe (atomic, the one we'll build on):**
```
R="runsc -root <stateDir> --network=none -overlay2=root:self"   # overlay = disk-backed tmpfs
# bundle: rootfs + config.json; write app state to a PLAIN dir in rootfs (e.g. /state),
#   NOT a tmpfs mount.
$R create  -bundle <bundle> -pid-file <pf> demo
$R start   demo
# ... accumulate state ...
$R checkpoint -image-path <MEMDIR> demo          # single snapshot: checkpoint.img pages.img pages_meta.img
$R delete -force demo
$R create  -bundle <bundle> -pid-file <pf2> demo2
$R restore -bundle <bundle> -image-path <MEMDIR> -pid-file <pf2> -background -detach demo2
```
Key: `-overlay2=root:self` on EVERY runsc call; app state in the rootfs
overlay (not a tmpfs mount); one `checkpoint` (drop fscheckpoint).

### restore-into-a-new-pod: (next — runsc create+restore with a spec-equivalent
bundle inside a pod, checkpoint pulled from S3)

## Findings / go-no-go

(pending)
