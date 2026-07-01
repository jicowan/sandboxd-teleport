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

### S3 data path: WORKS ✅ — but "cold new pod" restore model is WRONG ⚠️

**S3 round-trip proven:** a privileged shim pod (SA `ckpt-spike`, Pod Identity)
uploaded the checkpoint + bundle to S3 and a fresh consumer downloaded both
cleanly (`up/dl mem rc=0`, `up/dl bundle rc=0`). So moving checkpoints via S3
between pods/nodes works.

**But restore into a COLD fresh pod FAILED** with:
`vfs.CompleteRestore() failed: filesystem type "9p": failed to walk "tmp" in
mount "__no_name_0:/": no such file or directory`.

**Root cause (corrected model):** the checkpoint references a **gofer-backed
(9p) rootfs**. Restoring requires that gofer/rootfs mount environment to
already exist and be walkable. A cold `runsc create`+`restore` in a brand-new
pod (fresh mount namespace) can't reconstruct it → 9p walk fails.

**Substrate does NOT create a new pod per restore.** Re-reading
`cmd/ateom-gvisor/main.go:386-440`: the ateom **worker pod is persistent** and
owns the runsc `-root` state dir, the netns/veth (`actorVethIP`), and the gofer
lifecycle. On resume it does `cmdCreate` (which stands up the gofer/rootfs in
the worker's OWN consistent mount namespace) then `cmdRestore` into it. Two
scopes: `DATA` = `create --fs-restore-image-path` + `start` (fs only);
`FULL` = `create` then `restore` (memory). Our same-node FULL restore (1a)
matched this and worked precisely because create+restore shared one `-root`/
mount ns.

**Corrected 1b goal:** not "cold pod from S3 snapshot." Instead: **a
persistent/warm worker pod that, on resume, does `runsc create` + `runsc
restore` into its own `-root`**, with the checkpoint image fetched from S3.
The pod is the durable substrate; the sandbox state teleports onto it. This
is exactly substrate's WorkerPool model ("warm pods ready to receive resumed
actor states").

### W1 — in-pod restore: WORKS ✅ (2026-07-01) — THE gate

The whole warm-worker model hinged on this: can a checkpoint be restored
*inside a running pod* (vs the cold-pod attempt that 9p-failed)? **Yes.**

Ran the full atomic cycle entirely inside the persistent `ckpt-shim` pod,
**pod-local paths** (`/work`, the container's own fs — not `/host`), one
`-root`, one bundle:
create → start → (accumulate) → `checkpoint` → delete → `create` (new id) →
`restore`. Result: `checkpoint rc=0`, `restore rc=0`, `state=running`,
**RAM counter continued 5→9** and **/state/log kept its pre-checkpoint lines
(5→9)**. No 9p error (only benign rseq/SA_RESTART/seccomp notices).

**Conclusion:** the earlier cold-pod 9p failure was purely a mount-namespace
mismatch (create on node / restore in a fresh pod). When ONE persistent pod
does both create and restore in its OWN mount ns, the gofer-backed rootfs
resolves and restore succeeds — validating the warm-worker design. "Restore
into a container in a running pod" is proven; "create a NEW pod from a
snapshot" remains the wrong framing (and unnecessary).

Next: W2 (S3 teleport between two warm workers — each creates its own gofer,
so each has a consistent mount ns), then W3 reconnect, W4 AIO-under-gVisor.

### W2 — S3 teleport: WORKS ✅ (2026-07-01)

Checkpoint travels through S3 and restores into an independent worker context.
Producer: create+start+accumulate → atomic `checkpoint` (**732K**) → upload to
`s3://…/w2ckpt/`. Consumer (independent `-root`, own gofer): download ckpt from
S3 → `create`+`restore` against the node-local bundle. Result: `restore rc=0`,
`state=running`, RAM counter continued 5→9 AND /state/log carried 5→9 lines.

**Design insight (important): only the CHECKPOINT travels via S3 (~732K); the
bundle/rootfs stays node-local.** First attempt uploaded the whole 585M rootfs
file-by-file → 28k S3 objects / 1.3GB, timed out. In the real design the worker
already has the AIO image locally; S3 only carries the per-session checkpoint.

**Scope caveats (honest):**
- Producer + consumer ran in the SAME pod with independent `-root` dirs (Karpenter
  node churn from the nodepool instance-size change blocked getting a clean
  second-node pod). W1 already established mount-ns independence — not node — is
  the variable that matters, and each `-root` has its own gofer, so this
  exercises the teleport mechanism. **True cross-node + cross-generation
  (c6a→c7a) CPU-match restore remains formally unproven** — worth a clean retest
  once a stable 2nd gvisor node is available.
- Bundle built from the shim container's own filesystem (585M) because `ctr
  image mount` was flaky on fresh nodes; a real worker would ship a lean AIO
  rootfs.

### Infra notes / cluster hygiene
- gvisor nodepool `requirements` had NO instance-size bound → Karpenter chose
  c7a.medium (8-pod cap; 6 are daemonsets → only ~2 usable slots). Added
  `instance-size In [xlarge,2xlarge]`; this triggered drift replacement of the
  old c7a.medium nodes (churn that evicted the first `ckpt-shim`).
- **Put `karpenter.sh/do-not-disrupt: "true"` on worker pods** so consolidation
  doesn't evict them mid-session (learned the hard way). ckpt-shim2 has it.
- `python-warmpool` SandboxWarmPool was deleted by the operator during this work.

### 1b (reframed): warm-worker-pod restore — W1 ✅, W2 ✅; next W3 (reconnect), W4 (AIO-under-gVisor)

## RESUME POINT (2026-07-01, context compaction)

**Proven so far:** atomic mem+fs checkpoint/restore on EKS gVisor nodes
(out-of-band runsc, `-overlay2=root:self`); in-pod restore (W1); S3 teleport of
the checkpoint into an independent `-root` (W2). Only the ~732K checkpoint
travels via S3; bundle/rootfs stays node-local.

**Decision for next test:** AIO cross-node via anti-affinity — force AIO onto 2
gvisor nodes and test cross-node/cross-gen (c6a↔c7a) checkpoint/restore.

**UPDATE 2026-07-01 (later): superseded — see "IN-PLACE checkpoint" section at
end.** We confirmed AIO is NOT running under gVisor (template runtimeClassName
is empty; AIO pods are on runc m5 nodes). But the `python-sandbox` gVisor pod
IS containerd-managed gVisor, and we successfully checkpointed it IN PLACE with
host runsc against containerd's root — the clean path. So the out-of-band AIO
bundle detour is unnecessary for the checkpoint half; focus shifts to restore.

**Key facts / state:**
- AIO under gVisor = the **`aio-sandbox-warmpool` pods**, image
  `ghcr.io/agent-infra/sandbox:latest`. (Note: `kubectl` showed their
  `runtimeClassName` empty in one query — reconcile: confirm they truly run
  under gvisor before relying on it.)
- gvisor nodes: currently ONE — `ip-10-0-5-90` (c6a.xlarge). Need a 2nd
  (different gen ideally) for cross-node. Nodepool `gvisor` now pinned
  `instance-size In [xlarge,2xlarge]` (changing it churns nodes — that evicted
  the original `ckpt-shim`).
- `ckpt-shim2` pod (SA `ckpt-spike`, Pod Identity→S3, has
  `karpenter.sh/do-not-disrupt`) still running on `ip-10-0-5-90`; `/host/work/`
  has the working bundle + w2 scripts.
- S3 bucket `aio-checkpoint-spike-820537372947-us-west-2`; IAM role
  `aio-checkpoint-spike-role`; SA `default/ckpt-spike`.

**CORRECTION — earlier "can't checkpoint containerd-managed pods" was WRONG.**
That was an over-generalization from the 9p restore failure, which was actually
a mount-namespace/gofer inconsistency in the cold cross-pod attempt (checkpoint
in one `-root`, restore in a fresh pod) — NOT proof that containerd containers
can't be checkpointed. GKE snapshots/restores containerd-SCHEDULED gvisor pods,
so it's clearly possible.

Corrected understanding:
- `runsc` is a CLI over a state `-root`. Containerd's runsc shim manages gvisor
  containers against root **`/run/containerd/runsc/k8s.io`** (observed in the
  node process list: `runsc --root=/run/containerd/runsc/k8s.io ...`).
- So out-of-band `runsc -root /run/containerd/runsc/k8s.io checkpoint
  <containerd-container-id>` should be able to checkpoint a RUNNING warmpool
  pod's gvisor container — UNTESTED (we only ever used our own /tmp root). This
  is the cheap test to run next, NOT dismiss.
- The genuinely harder half is RESTORE: GKE coordinates pod re-creation +
  restore at the node level (podsnapshot controller). Raw `runsc restore` into
  containerd's root while containerd owns lifecycle is the untested tricky part.
- The 9p failure means: restore needs a consistent rootfs/gofer/mount-ns; it
  does NOT mean containerd containers are off-limits.

→ Revised plan: (1) try checkpointing a running `aio-sandbox-warmpool` gvisor
container in place via `runsc -root /run/containerd/runsc/k8s.io checkpoint
<id>` (find the containerd id from the shim process / crictl). (2) Figure out
the restore path (in-place resume vs. new pod) — this is where GKE's node
coordination matters and our design work concentrates. Anti-affinity (per the
AIO SandboxTemplate) forces 2 gvisor nodes for the cross-node/cross-gen leg.

**Concrete next steps:**
1. Ensure a 2nd gvisor node (anti-affinity on worker pods, or scale a warmpool,
   with do-not-disrupt to avoid churn).
2. Build an AIO OCI bundle (rootfs from `ghcr.io/agent-infra/sandbox:latest`)
   for out-of-band runsc — reuse the W2 "build rootfs" approach but lean.
3. Producer worker (node A): create+start AIO under our runsc, atomic
   checkpoint, upload 732K-class image to S3.
4. Consumer worker (node B, different gen): download, restore, verify AIO's
   MCP hub state survived.
5. Then W3 (reconnect: sandbox `:8080` reachable again post-restore).

**Cluster cleanup owed (if abandoning):** delete `ckpt-shim2`; revert gvisor
nodepool instance-size requirement; delete S3 bucket + IAM role + `ckpt-spike`
SA + Pod Identity association; scale/remove any extra gvisor nodes.

## IN-PLACE checkpoint of a containerd-managed gVisor pod: WORKS ✅ (2026-07-01)

**This is the clean path — no image pull, no hand-built bundle.** You CAN
checkpoint a live containerd-scheduled gVisor pod by running `runsc` on the host
against containerd's OWN root. Proves the earlier "can't checkpoint
containerd-managed pods" worry fully wrong.

**What we checkpointed:** `sandbox-python-example` (a real gVisor SandboxTemplate
pod, `runtimeClassName: gvisor`) on gvisor node `ip-10-0-5-141` (c6a.xlarge).
Note: the **AIO** warmpool pods are NOT gVisor — `aio-sandbox-template` has
`runtimeClassName: ""`, so they land on plain runc m5.large nodes (no runsc
container to checkpoint). The `python-sandbox` template IS gVisor; used it.

**On the node (via `kubectl node-shell`):**
```
RROOT=/run/containerd/runsc/k8s.io
# find the workload container (the one with lisafs:self overlay), NOT the pause/sandbox container:
runsc --root=$RROOT list        # two ids per pod: 620b… = pause (2 mounts), a10f… = python-sandbox workload
C=a10f2e4dce06…                 # the container-type=container one (io.kubernetes.cri.container-name=python-sandbox)
runsc --root=$RROOT checkpoint --image-path=/var/lib/ckpt-inplace/py --leave-running $C
```
Result: **rc=0**, artifacts `checkpoint.img` (427K) + `pages.img` (32M) +
`pages_meta.img`. **`--leave-running` kept the pod up** — pod stayed `1/1
Running`, container `status: running` after checkpoint. Checkpoint staged to
`/work/inplace-py/` on the shim's hostPath for S3 upload.

**Key learnings:**
- Drive it against containerd's root `/run/containerd/runsc/k8s.io` (NOT our own
  /tmp root) to hit the live pod. runsc is just a CLI over whatever `-root` you
  point at; containerd's shim uses that path.
- A gVisor *pod* = 2 runsc containers: the pause/sandbox (`container-type=sandbox`)
  and the workload (`container-type=container`, has the `lisafs:self` writable
  overlay). Checkpoint the **workload** container.
- No `--leave-running` would stop the container; with it the pod keeps serving.
- Node disk: AL2023 default root is 20Gi — too small once you cache big images
  (the AIO 3.4Gi pull earlier drove the node into `DiskPressure`, Karpenter
  killed it). Bumped gvisor `EC2NodeClass` blockDeviceMapping /dev/xvda → 100Gi.

**Still open (the hard half): RESTORE of an in-place checkpoint.** Checkpoint is
easy; restoring into a containerd-managed lifecycle (containerd owns create/
delete) is the untested part — GKE coordinates this at the node level. Options
to explore: (a) restore out-of-band into our own -root from these images
(mount-ns must be consistent — W1 showed one pod doing create+restore works);
(b) whether containerd/CRI can be told to restore. Next.

## AIO under gVisor + in-place checkpoint: WORKS ✅ (2026-07-01)

**Corrected the deployment, then proved it.** AIO WAS running on runc because
`aio-sandbox-template` had `runtimeClassName: ""` (empty ≠ gVisor; the `gvisor`
RuntimeClass is NOT the cluster default, so pods landed on plain runc m5 nodes —
verified on-node: zero runsc procs, handler `io.containerd.runc.v2`, runsc not
even installed). Fix: **patch the SandboxTemplate `podTemplate.spec.runtimeClassName:
gvisor`** (agent-sandbox copies the pod spec verbatim; you MUST set runtimeClassName
explicitly — it is not defaulted). The `gvisor` RuntimeClass has a `scheduling`
block ({nodeSelector sandbox=gvisor, toleration}) so that ONE field also pins the
pods to the gvisor node — no manual nodeSelector/toleration needed.

Deleted the 2 stale runc warmpool pods; warmpool recreated them from the updated
template → new pods `runtimeClassName: gvisor` on gvisor node ip-10-0-5-141,
**Running**. Confirmed genuinely runsc (runsc list shows them; runsc-sandbox boot
procs).

**AIO is fully healthy under gVisor** (the big compat unknown — it bundles Chrome):
MCP hub HTTP 200 on :8080; supervisord + python MCP hub + Node REPL + code-server
+ JupyterLab + **mcp-server-browser w/ Chrome CDP :9222** + Xvnc + openbox all up.
Chrome-under-gVisor works.

**In-place checkpoint of the AIO gVisor pod:**
```
RROOT=/run/containerd/runsc/k8s.io
C=<aio workload container id>   # container-type=container, name=aio-sandbox (NOT the pause/sandbox)
runsc --root=$RROOT checkpoint --image-path=/var/lib/ckpt-inplace/aio --leave-running $C
```
Result: **rc=0 in ~1 second**. Artifacts: checkpoint.img 5.9M + pages.img **624M**
(live RAM: Chrome/Node/Jupyter/code-server/Xvnc/MCP hub) + pages_meta.img 129K =
**601M**. Wrote a marker file `/home/gem/ckpt-marker.txt` first. Post-checkpoint the
pod stayed **Running, ready, 0 restarts**, marker intact, MCP hub still 200 — the
`--leave-running` container was untouched.

## RESTORE of the in-place AIO checkpoint: BLOCKED on pod-granularity ⚠️ (2026-07-01)

Attempted out-of-band restore of the AIO workload checkpoint into our own -root.
Got very close, and the failure is diagnostic, not fatal — it tells us the
correct granularity.

**Iteration (each error taught the next fix):**
1. `restore` straight → `cannot load sandbox: open <sandbox-id>_sandbox…state:
   no such file` — the checkpoint's CRI annotations
   (`container-type=container`, `sandbox-id=0c33…`) make runsc look for the POD
   sandbox's sentry state in our root. Fix attempt: strip `io.kubernetes.cri.*`
   annotations + drop namespace `path` bindings from config.json.
2. rootfs via symlink → `setting up FS: mounting /etc/hosts … expected to open
   …bundle/rootfs/etc/hosts but found …/d990…/rootfs/etc/hosts` (gVisor mount
   safety rejects the symlinked rootfs). Fix: `mount --bind` the original
   overlay rootfs into a REAL bundle/rootfs dir (original pod still alive →
   lowerdirs shared).
3. Then restore **rc=0, sandbox `status: running`** — but `exec` → `container
   "aio-r" not started`; async page loader shows all 611M paged in (122 MB/s in
   5s, then 0 B/s = DONE, not stalled), yet no `CompleteRestore`.

**ROOT CAUSE (confirmed): the AIO container is a NON-ROOT sub-container of its
pod sandbox.** A k8s gVisor pod = ONE sentry shared by 2 containers: the pause/
sandbox container (`0c33…`, the pod-sandbox ROOT) and the workload (`d990…`).
Proof: both share the SAME sandbox PID (14378) in `runsc list`. The restore load
logs `{ContainerID:aio-r, RootContainer:false}`. gVisor won't mark a non-root
container "started" until its pod-ROOT (pause) container is restored first — and
we only checkpointed the workload, so it waits forever. Hence sandbox=running,
container=not-started, no CompleteRestore.

**Implication / correct path:** checkpoint & restore at **pod-sandbox
granularity**, not single-container. Either (a) checkpoint BOTH the pause and
workload containers and restore the pause (root) first then the workload — the
way containerd/GKE coordinate a pod; or (b) run the workload as its own runsc
ROOT container (out-of-band bundle, our earlier W1/W2 busybox model) where there
is no separate pause container, so the workload IS the root and restores
cleanly. W1/W2 worked precisely because busybox was a standalone root container.

**So:** in-place *checkpoint* of a real k8s gVisor pod = easy (proven). In-place
*restore* needs pod-level (multi-container, root-first) orchestration — exactly
what GKE's podsnapshot controller does at the node level. Single-container
out-of-band restore only works when the container is its own root.

Live AIO pod verified UNAFFECTED by all restore attempts (Running, ready, 0
restarts) — we worked in a separate -root and bind-mounted (not moved) its
rootfs.

### Container-vs-pod checkpoint granularity — TESTED (2026-07-01)

Q: do you have to checkpoint the whole pod, or can you checkpoint one container
and restore just that? Empirical answer: **the unit is the pod-sandbox, because
gVisor runs ONE sentry per pod shared by all its containers.**
- Checkpoint the WORKLOAD sub-container (`d990…`) → image = 601M (its memory).
  Restore-in-isolation STALLS: loads RootContainer:false, waits for the pod-root
  (pause) container that isn't there → never "started".
- Checkpoint the ROOT/pause container (`0c33…`, container-type=sandbox) →
  image = **694M** — LARGER, because it captured the ENTIRE sandbox (pause +
  workload memory) in one snapshot, ~1.7s. This is the whole pod in one image.
So: you can *make* a single-container image, but you can't *restore* it standalone
— restore requires the sandbox root. Checkpoint/restore at pod (sandbox-root)
granularity is the correct unit. Restoring a multi-container sandbox still needs
each sub-container's bundle/gofer (root restored first, then members joined) —
this is the pod-level orchestration GKE's podsnapshot controller does. NOT yet
run end-to-end; the whole-sandbox image (694M at /var/lib/ckpt-inplace/aiopod)
is captured and ready for that test.
Exception: a workload that is its OWN root container (no separate pause — the
out-of-band W1/W2 busybox model) restores standalone cleanly.

### Whole-pod restore attempt — NEW failure detail (2026-07-01)

(The restore *model* — restore INTO a warm/persistent pod not a new one; only
fs-delta + memory travel via the DATA/FULL two-scope `create
--fs-restore-image-path` + `restore -image-path` — was ALREADY established
above: see "S3 data path … 'cold new pod' restore model is WRONG" and the
substrate two-scope note. Not repeating it; this section only records the new
empirical failure and what it pins down.)

Tried whole-pod restore into a bare -root: restore pause/ROOT (rc=0, running)
then the workload member → got PAST the RootContainer:false stall to a NEW,
later error: `vfs.CompleteRestore() … 9p failed to walk "home" … in mount
"aio-sandbox:/"`.

**New root cause pinned:** the workload's real rootfs (`/home/gem`) lives in the
sentry's `lisafs:self` **overlay upper** (captured in the checkpoint). On restore
the gofer must re-present the lower tree with the SAME `gofer-mount-confs`
(`lisafs:self,none,none,none,none`). My hand-built bundle came up with the wrong
mount-conf (`lisafs:none,none`) → gofer didn't present the expected rootfs → 9p
walk failed. **Confirms WHY you must restore into the real pod's gofer, not a
hand-built bundle:** reconstructing each sub-container's gofer + mount-conf +
fd-donation by hand = reimplementing containerd's runsc shim.

**New fact:** off-the-shelf containerd CRI does NOT expose pod checkpoint/restore
(runsc shim binary contains checkpoint/restore symbols AND
unimplemented/"not implemented" strings). So "hook into containerd to restore
from S3" = custom controller/shim work — matches the already-noted "no
containerd/Kubernetes restore path exists" and why GKE built a node-level
podsnapshot controller.

**Next experiment (unchanged from the warm-worker plan):** restore INTO a warm
pool pod's existing sandbox/gofer using `-fs-restore-image-path` (delta) +
`-image-path` (memory), not a bare -root — the persistent-worker path already
scoped above.

## Findings / go-no-go

- **Checkpoint of a live containerd gVisor pod on EKS: PROVEN** — both a small
  python sandbox (32M) AND the full AIO sandbox incl. Chrome (601M, ~1s), in
  place, host runsc against containerd's root, pod survives `--leave-running`.
- **AIO runs correctly under gVisor: PROVEN** (previously an open compat gate).
- **How to make agent-sandbox pods gVisor: set `runtimeClassName: gvisor` in the
  SandboxTemplate's podTemplate.spec** (explicit; RuntimeClass scheduling pins
  the node).
- **Restore of an in-place k8s-pod checkpoint: needs POD granularity** (root/
  pause container restored before the non-root workload). Single-container
  restore stalls at `not started` (RootContainer:false). Out-of-band restore
  works only when the workload is its OWN root container (W1/W2). This is the
  next design fork: (a) pod-level restore orchestration, or (b) run AIO as a
  standalone root container out-of-band.
