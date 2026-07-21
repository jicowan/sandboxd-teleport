# Runbook — Checkpoint a sandboxed (gVisor) container and restore it onto another container via S3

Precise, reproducible steps for what was proven on 2026-07-01 (spike "T2"). This
checkpoints the **memory + filesystem state of a single gVisor container** (not a
Kubernetes pod), ships it through S3, and **restores that state into a different,
freshly-created container** — the source container is destroyed in between, so the
state exists only in S3 at the hand-off. This is the substrate "teleport" model.

> **Scope honesty — read this first.**
> - The container is a **standalone runsc ROOT container** that *we* drive
>   out-of-band (our own `-root`, our own OCI bundle). It is **not** a
>   containerd/kubelet-managed pod. We proved (spikes T1/T1c) that you cannot
>   restore onto a kubelet-managed pod's container out-of-band — kubelet recreates
>   it, and gVisor won't inject a restored sub-container into an already-running
>   sandbox. Single-root standalone containers avoid both problems.
> - **Same-node caveat:** in the proven run, source and target were on the **same
>   gVisor node**. The state genuinely traveled through S3 (source fully deleted
>   first), so it is node-*independent*, but a truly **different node** additionally
>   requires: identical `runsc` version (hard error otherwise) and CPU-feature
>   compatibility (pin instance family or set
>   `dev.gvisor.internal.cpufeatures`). See "Cross-node deltas" at the end.

## Prerequisites (what the environment had)

- A gVisor node with `runsc` at `/usr/local/bin/runsc`, version
  `release-20260622.0`, containerd 2.2.4, AL2023. (`sandbox=gvisor` label +
  taint; disk bumped to 100Gi via the Karpenter EC2NodeClass.)
- **Node command execution via AWS SSM Run Command** (NOT `kubectl node-shell`).
  node-shell wedges when `runsc restore` backgrounds and truncates output; SSM is
  async and captures stdout/stderr reliably. Instance i-000d00ffa9964e5b3.
- **S3 bucket + Pod Identity:** `aio-checkpoint-spike-111122223333-us-west-2`;
  IAM role `aio-checkpoint-spike-role` (trust `pods.eks.amazonaws.com`), scoped
  to the bucket; ServiceAccount `default/ckpt-spike`.
- **A privileged helper pod** (`ckpt-shim2`, SA `ckpt-spike`) on the same node
  with `hostPath: /` mounted at `/host`. It does the **S3 I/O** (via Pod
  Identity). The node's own instance role deliberately has NO S3 access.
  → Division of labor: **`runsc` ops run on the node via SSM; S3 upload/download
  runs inside the shim pod; they share the node filesystem** (the shim sees the
  node's `/var/lib` at `/host/var/lib`).

Two `runsc` behaviors that make it work:
- `-overlay2=root:self` on **every** `runsc` call → the container's writable
  rootfs is a disk-backed overlay, so a **single `runsc checkpoint` captures
  memory AND filesystem atomically**.
- `restore` must be preceded by `create` (new id), and run with
  `-bundle … -pid-file … -detach` and **no** foreground pipe (a `| tail` on a
  backgrounded restore is what wedged node-shell). **No `-direct`** on tmpfs/
  overlay (`/tmp`/`/var/lib`) — O_DIRECT is rejected there.

---

## Part A — Build the container's OCI bundle (rootfs + spec)

Done once per node; `busybox:1.36` used as the trivial stateful workload.

1. Get a real rootfs. `ctr images mount` was flaky (stuck snapshot leases), so
   the reliable path is to let containerd prepare the snapshot by **creating a
   throwaway container**, then mount that snapshot:
   ```sh
   ctr -n k8s.io container create --snapshotter overlayfs \
       docker.io/library/busybox:1.36 bb-src sleep 3600
   # get + run the overlay mount command for that snapshot into the bundle rootfs:
   ctr -n k8s.io snapshots --snapshotter overlayfs mounts /var/lib/teleport/bundle/rootfs bb-src > /tmp/mnt.sh
   sh /tmp/mnt.sh        # mounts an overlay at .../bundle/rootfs (has /bin/busybox)
   ```
2. Generate and edit the OCI spec:
   ```sh
   cd /var/lib/teleport/bundle && runsc spec       # writes config.json
   # edit config.json:
   #   process.args = the workload (see below)
   #   process.terminal = false
   #   root = { "path": "rootfs", "readonly": false }
   ```
   Workload used (in-RAM counter + append to a file on the overlay rootfs — proves
   BOTH memory and filesystem continuity):
   ```sh
   /bin/sh -c 'n=0; mkdir -p /state; echo boot >> /state/log;
     while true; do n=$((n+1)); echo "count=$n" >> /state/log; echo "ram=$n"; sleep 2; done'
   ```

---

## Part B — Source container (worker A): run, accumulate, checkpoint

All `runsc` on the node via SSM. `S=/var/lib/teleport`.

3. Create + start worker A as a standalone ROOT container in its own `-root`:
   ```sh
   R="runsc -root $S/rootA --network=none -overlay2=root:self"
   $R create -bundle $S/bundle -pid-file $S/wA.pid wA
   $R start wA
   # let it run so it has real state (counter climbs, /state/log grows)
   ```
4. Checkpoint (single atomic image = memory + fs, because of the overlay rootfs):
   ```sh
   mkdir -p $S/img
   $R checkpoint --image-path=$S/img wA
   # -> $S/img/{checkpoint.img, pages.img, pages_meta.img}   (~360K for busybox)
   # (without --leave-running the container goes to state "stopped" after)
   ```
   Record the last counter value (e.g. `count=21`) to verify continuity later.

---

## Part C — Ship to S3 (in the shim pod, via Pod Identity)

5. Upload the checkpoint. The shim sees the node fs at `/host`:
   ```sh
   # kubectl exec ckpt-shim2 -n default -- sh -c '...'
   aws s3 cp --recursive /host/var/lib/teleport/img/ \
       s3://aio-checkpoint-spike-111122223333-us-west-2/teleport/img/
   ```
   Only the small checkpoint travels (base rootfs stays node-local / comes from
   the image on each node).

---

## Part D — Destroy the source (state now lives only in S3)

6. On the node (SSM), delete worker A **and** the local checkpoint copy, so the
   only remaining copy of the state is in S3:
   ```sh
   runsc -root $S/rootA delete -force wA
   pkill -f "root=$S/rootA"
   rm -rf $S/rootA $S/img
   ```

---

## Part E — Target container (worker B): download from S3, restore

7. Download the checkpoint from S3 into a **fresh** directory (shim pod):
   ```sh
   # kubectl exec ckpt-shim2 ...
   mkdir -p /host/var/lib/teleport/img-b
   aws s3 cp --recursive \
       s3://aio-checkpoint-spike-111122223333-us-west-2/teleport/img/ \
       /host/var/lib/teleport/img-b/
   ```
8. Give worker B its **own** rootfs (a separate snapshot) and an **identical**
   OCI spec (the restore spec shape must match the checkpoint's):
   ```sh
   ctr -n k8s.io container create --snapshotter overlayfs \
       docker.io/library/busybox:1.36 bb-srcB sleep 3600
   ctr -n k8s.io snapshots --snapshotter overlayfs mounts \
       $S/bundle-b/rootfs bb-srcB > /tmp/mntB.sh ; sh /tmp/mntB.sh
   cd $S/bundle-b && runsc spec       # then apply the SAME process.args / root edits as Part A
   ```
9. Restore into worker B — a **separate `-root`** — with `create` then `restore`:
   ```sh
   RB="runsc -root $S/rootB --network=none -overlay2=root:self"
   $RB create  -bundle $S/bundle-b -pid-file $S/wB.pid wB
   $RB restore -bundle $S/bundle-b -image-path=$S/img-b -pid-file=$S/wB.pid -detach wB
   # verify:
   $RB state wB                       # "status": "running"
   $RB exec  wB cat /state/log        # counter CONTINUES from checkpoint (e.g. 21->22->23...), NOT reset to 1
   ```

**Proven result:** worker B's counter resumed `19→20→21→22→23` and kept advancing
(`24,25`) — the exact value at checkpoint, not reset. RAM + filesystem state
teleported from a destroyed container, through S3, into a new container.

---

## Order-of-operations cheat sheet

```
[node/SSM]  build bundle (rootfs snapshot + runsc spec)
[node/SSM]  R="runsc -root <A> --network=none -overlay2=root:self"
[node/SSM]  $R create -bundle <B> -pid-file <pfA> wA ; $R start wA ; (accumulate state)
[node/SSM]  $R checkpoint --image-path <IMG> wA          # atomic mem+fs, ~KBs
[shim pod]  aws s3 cp --recursive <IMG> s3://…/teleport/img/
[node/SSM]  runsc -root <A> delete -force wA ; rm -rf <A> <IMG>   # SOURCE GONE
[shim pod]  aws s3 cp --recursive s3://…/teleport/img/ <IMG_B>
[node/SSM]  (build worker B's own rootfs + identical spec)
[node/SSM]  RB="runsc -root <Broot> --network=none -overlay2=root:self"
[node/SSM]  $RB create -bundle <Bbundle> -pid-file <pfB> wB
[node/SSM]  $RB restore -bundle <Bbundle> -image-path <IMG_B> -pid-file <pfB> -detach wB
[node/SSM]  $RB exec wB cat /state/log     # continuity proven
```

## Gotchas (each cost iterations)

- **Use SSM, not `kubectl node-shell`.** node-shell's nsenter pod wedges
  (Terminating) on a backgrounded `runsc restore` and truncates multi-line output.
- **No foreground pipe on `runsc restore`.** `runsc … restore … | tail` hangs
  even with `-detach`. Redirect to a file, read the file separately.
- **`-overlay2=root:self` on every call** — else the checkpoint won't include
  filesystem state, and rootfs handling gets inconsistent.
- **No `-direct`** on tmpfs/overlay-backed paths (`invalid argument` on the pages
  file). Substrate uses `-direct` because its image path is O_DIRECT-capable.
- **`restore` needs a preceding `create`** (new id) + `-bundle -pid-file -detach`.
- **`ctr images mount` is flaky** (stuck "bucket already exists" snapshot leases).
  Prefer `ctr container create` + `ctr snapshots mounts` to get a real rootfs.
- **Spec shape must match** across checkpoint→restore (Mounts, Process, root); the
  container is matched by name/id. Bundle path and node MAY differ.

## Cross-node deltas (what changes for a genuinely different node — NOT yet proven, "T3")

The steps are identical; the target node just downloads from S3 and restores. The
extra requirements for a *different* node are:
- **Same `runsc` version** on both nodes (gVisor hard-errors on mismatch). Pin it.
- **CPU-feature compatibility:** the restore host must have all CPU features the
  checkpoint host had. Keep workers on one instance family, or set the
  `dev.gvisor.internal.cpufeatures` annotation. (This is why cross-gen c6a→c7a is
  called out as a separate test.)
- **Lean rootfs on the target:** the base image must already be present on the
  target node (only the checkpoint travels via S3). In this spike the rootfs came
  from the busybox image cached per-node.

## Why standalone-root and not a Kubernetes pod (context)

A gVisor **pod** = pause (sandbox root) + workload, sharing ONE sentry; the
workload is a non-root sub-container. We proved you can checkpoint such a pod
in-place, but you cannot restore it onto a warm kubelet-managed pod out-of-band:
kubelet recreates the killed workload (T1), and gVisor refuses to restore a
sub-container into an already-started sandbox (T1c). Running the sandbox as its
own **standalone root container that a privileged worker owns** (this runbook)
sidesteps both — which is exactly why agent-substrate's worker owns its own
`runsc -root` instead of using containerd/`runtimeClassName`. See
[`ARCHITECTURE.md`](./ARCHITECTURE.md) and [`../NOTES.md`](../NOTES.md).
