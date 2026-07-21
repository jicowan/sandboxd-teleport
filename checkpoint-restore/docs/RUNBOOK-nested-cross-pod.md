# Runbook — Checkpoint a NESTED gVisor sandbox in one pod, restore it in ANOTHER pod

Proven 2026-07-01. This is the "worker" model: a gVisor sandbox runs **nested
inside a normal Kubernetes pod** (not as a `runtimeClassName: gvisor` pod), the
pod's own process drives `runsc`, and the sandbox's memory+filesystem state is
checkpointed and **restored into a different worker pod** with state intact.

Unlike the host-level runbook (`RUNBOOK-container-teleport.md`, which ran `runsc`
on the node via SSM), here **everything happens inside pods** — no SSM, no
host-level runsc invocation. This is the shape a real worker would take.

> **What this proves / caveats**
> - The worker pod is a **normal runc pod, `privileged: true`** (gVisor-in-a-pod
>   = Docker-in-Docker-shaped; privilege is required for the nested runtime).
> - The nested sandbox is a single **runsc ROOT container** the pod owns — no
>   pause container, not visible to kubelet/containerd. This is why restore works
>   here but not onto a kubelet-managed gVisor pod (see NOTES T1/T1c).
> - Transfer here uses `kubectl cp` for clarity; **in production this is S3**
>   (proven separately in `RUNBOOK-container-teleport.md` / spike T2).
> - Same-node in this run; cross-node additionally needs matching runsc version +
>   CPU-feature compatibility (see the host runbook's "Cross-node deltas").

## 0. Worker pod spec (both A and B use this; `worker-nested.yaml`)

```yaml
apiVersion: v1
kind: Pod
metadata: { name: worker-nested, namespace: default,
            annotations: { karpenter.sh/do-not-disrupt: "true" } }
spec:
  nodeSelector: { sandbox: gvisor }          # so the node's runsc binary is mountable
  tolerations: [{ key: sandbox, operator: Equal, value: gvisor, effect: NoSchedule }]
  restartPolicy: Never
  containers:
    - name: worker
      image: debian:12-slim
      command: ["sleep","infinity"]
      securityContext: { privileged: true }   # REQUIRED for nested gVisor
      volumeMounts: [{ name: runsc, mountPath: /usr/local/bin/runsc, subPath: runsc }]
  volumes: [{ name: runsc, hostPath: { path: /usr/local/bin } }]  # node's runsc binary
```
Create two: `worker-nested` (A) and `worker-nested-b` (B, same spec, new name).

> The worker is `runtimeClassName: <none>` — a **plain runc pod**. It only borrows
> the node's `runsc` *binary*; it is NOT itself a gVisor pod. `uname -r` inside the
> nested sandbox returns `4.19.0-gvisor` (the sentry), confirming real isolation.

## 1. Build the sandbox's OCI bundle INSIDE worker A (from an OCI image)

No host containerd. Use `crane` to export a flattened rootfs from any OCI image:
```sh
# inside worker-nested:
apt-get update -qq && apt-get install -y -qq curl ca-certificates jq
cd /tmp
curl -sSL https://github.com/google/go-containerregistry/releases/latest/download/go-containerregistry_Linux_x86_64.tar.gz | tar xz crane
mkdir -p /work/oci-bundle/rootfs
./crane export docker.io/library/busybox:1.36 - | tar -x -C /work/oci-bundle/rootfs
```
Generate + fix the runsc spec. **CRITICAL: `runsc spec` defaults `root.readonly:
true`; the workload can't write state until you flip it to false.** (The debian
worker has **no python3** — use `sed`/`jq`, not a python patch.)
```sh
cd /work/oci-bundle && runsc spec
sed -i 's/"readonly": true/"readonly": false/' config.json
# set the workload (in-RAM counter + append to a file on the writable rootfs):
jq '.process.args=["/bin/sh","-c",
  "n=0; mkdir -p /state; echo boot >> /state/log; while true; do n=$((n+1)); echo count=$n >> /state/log; echo ram=$n; sleep 1; done"]
  | .process.terminal=false' config.json > c2 && mv c2 config.json
```

## 2. Run + checkpoint the nested sandbox in worker A

```sh
R="runsc -root /work/rt --network=none -overlay2=root:self"   # overlay = atomic mem+fs
$R create -bundle /work/oci-bundle -pid-file /work/n1.pid n1
$R start n1 &            # backgrounds; streams ram=N to stdout (ignore the noise)
sleep 6                  # let it accumulate (e.g. reaches count=7)
mkdir -p /work/img
$R checkpoint --image-path=/work/img --leave-running n1
# -> /work/img/{checkpoint.img, pages.img, pages_meta.img}   (~368K)
```
Record the counter at checkpoint (`$R exec n1 cat /state/log | tail -1` → e.g.
`count=7`). Verify no error and image files exist — **trust the on-disk artifacts,
not the streamed stdout.**

## 3. Transfer the checkpoint + spec from A to B

Production = S3 (worker A uploads, worker B downloads, via Pod Identity). For the
proof, `kubectl cp` through the local machine:
```sh
# pull out of A:
kubectl cp default/worker-nested:/work/img            ./img
kubectl cp default/worker-nested:/work/oci-bundle/config.json ./config.json
# B needs the SAME base rootfs (from the same OCI image) + the SAME config.json:
kubectl exec worker-nested-b -- bash -c \
  'mkdir -p /work/oci-bundle/rootfs /work/img && cd /tmp && \
   ./crane export docker.io/library/busybox:1.36 - | tar -x -C /work/oci-bundle/rootfs'
kubectl cp ./img/checkpoint.img  default/worker-nested-b:/work/img/checkpoint.img
kubectl cp ./img/pages.img       default/worker-nested-b:/work/img/pages.img
kubectl cp ./img/pages_meta.img  default/worker-nested-b:/work/img/pages_meta.img
kubectl cp ./config.json         default/worker-nested-b:/work/oci-bundle/config.json
```
The restore bundle's rootfs must be the **same image** and the **same
config.json** the checkpoint was made with (gVisor validates spec shape).

## 4. Restore in worker B (a different pod)

```sh
# inside worker-nested-b:
RB="runsc -root /work/rt --network=none -overlay2=root:self"
$RB create  -bundle /work/oci-bundle -pid-file /work/nX.pid nX
$RB restore -bundle /work/oci-bundle -image-path=/work/img -pid-file=/work/nX.pid -detach nX
$RB state nX                          # "status": "running"
$RB exec  nX cat /state/log | head    # boot, count=1..7, THEN 8,9,10,11...  <-- CONTINUITY
```

**Proven result:** worker B's `/state/log` shows `boot, count=1..7` (worker A's
pre-checkpoint history) then **continues `count=8,9,10…`**. Checkpoint was at 7 →
resume at 8. Both memory (`n`) and filesystem (`/state/log`) teleported from
worker A into a separate pod, worker B.

> **Reading it right:** "restored counter starts at 8" is SUCCESS, not a reset.
> A failed/fresh restore would show `boot, count=1`. Resuming at N+1 after a
> checkpoint at N is exactly the continuity we want.

## Cheat sheet

```
[worker A] crane export <img> -> rootfs ; runsc spec ; sed readonly=false ; jq set args
[worker A] R="runsc -root /work/rt --network=none -overlay2=root:self"
[worker A] $R create -bundle B -pid-file pf n1 ; $R start n1 & ; (accumulate)
[worker A] $R checkpoint --image-path /work/img --leave-running n1     # ~368K
[transfer] img/{checkpoint,pages,pages_meta}.img + config.json  A -> (S3) -> B
[worker B] crane export SAME <img> -> rootfs ; drop in SAME config.json
[worker B] $RB create -bundle B -pid-file pf nX
[worker B] $RB restore -bundle B -image-path /work/img -pid-file pf -detach nX
[worker B] $RB exec nX cat /state/log      # boot,1..7,8,9...  = continuity proven
```

## Gotchas (cost real iterations)

- **`runsc spec` defaults `root.readonly: true`.** Flip to false or the sandbox
  can't write state — you'll think C/R lost the filesystem when it never wrote it.
- **debian-slim has no python3.** Edit config.json with `sed`/`jq`. A python
  heredoc silently no-ops and leaves the broken default.
- **`start &` streams the workload stdout;** it drowns your own echoes. Verify via
  `runsc state`, on-disk `/work/img/*`, and reading `/state/log` — not the stream.
- **Restore needs `create` first + `-bundle -pid-file -detach`; no `-direct`** on
  overlay/tmpfs.
- **Same OCI image + same config.json** on the restore side (spec shape is
  validated; container matched by id/name).
- **privileged worker** is mandatory for nested gVisor (the DinD tradeoff).
- gVisor `runsc exec` opens a fresh process view; to inspect state read a file the
  workload wrote (`/state/log`), which is the durable proof of fs continuity.
```
