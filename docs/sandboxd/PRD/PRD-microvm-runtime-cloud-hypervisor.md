# PRD — a second sandbox runtime: Cloud Hypervisor microVMs (direct-drive)

Status: **IMPLEMENTED (Phases 0–3), live-validated 2026-08-12.** Written 2026-08-11; updated
2026-08-12 with implementation results (see §6 for per-phase status + fixes, §8 for answered
open questions). On branch `feat/microvm-runtime` / PR #30. Grounded in a read of the
live worker (`checkpoint-restore/sandboxd/{main,runsc,network,spec,bundle,containerd}.go`),
the wire contract (`checkpoint-restore/shared/sbxapi/sbxapi.go`), the CRDs
(`checkpoint-restore/controlplane/api/v1alpha1/*`), and the pod-construction path
(`internal/controller/warmpool_controller.go`) — plus Agent Substrate's multi-runtime
prior art at `~/GitHub/Projects/substrate-upstream` (`cmd/ateom-microvm`, `internal/ch`,
`internal/kata`, `docs/architecture.md`). Feasibility below cites what exists today, not
guesses.

Related: [[sandboxd-control-plane-state]], [PRD-snapshot-fork.md](./PRD-snapshot-fork.md)
(teleport/forkset contract), [PRD-worker-memory-reserve.md](./PRD-worker-memory-reserve.md)
(OCI-spec seam), [PRD-multi-sandbox-per-worker.md §3.1](./PRD-multi-sandbox-per-worker.md)
(per-worker networking singletons), [architecture-sandboxd.md](../architecture-sandboxd.md)
(worker-vs-sandbox model).

## 1. The idea & why it's compelling

sandboxd today runs **exactly one runtime**: a nested gVisor sandbox launched by driving
a pinned `runsc` binary. gVisor is a userspace kernel — strong syscall-interception
isolation, no hardware-virtualization requirement, and (critically for us) a built-in,
checkpointable netstack and `runsc checkpoint`/`runsc restore` that the entire teleport /
suspend / forkset story is built on.

What gVisor does **not** give us:

- **A hardware isolation boundary.** Some workloads (untrusted multi-tenant code, specific
  compliance postures, kernel-feature-hungry workloads that gVisor's syscall surface
  doesn't fully cover) want a *real* VM with its own guest kernel, not a shared userspace
  kernel.
- **Kernel compatibility.** gVisor implements a subset of Linux syscalls; workloads that
  need an unsupported syscall, a specific kernel module, nested containers with full
  cgroup control, etc., break under runsc but run unmodified in a microVM.

Agent Substrate — the project sandboxd's teleport model is modeled on — solved exactly
this by supporting **two sandbox classes**: `gvisor` and `microvm`. Its microVM path is
**Kata-agent + Cloud Hypervisor (CH)**, and it teleports microVMs the same way sandboxd
teleports gVisor sandboxes: pause → snapshot → object storage → restore on any warm
worker. This PRD proposes bringing that second runtime to sandboxd on EKS, **Cloud
Hypervisor first**, driven directly by the worker (not via a containerd runtime shim).

## 2. Why Cloud Hypervisor, and why "direct-drive" (the two load-bearing decisions)

These two choices are the spine of the design; everything downstream follows from them.

### 2.1 Cloud Hypervisor over Firecracker (first target)

Both are `rust-vmm`-based microVM monitors with mature **pause → snapshot → restore →
resume** and **userfaultfd demand-paging** on restore. The deciding factors for sandboxd:

| Factor | Cloud Hypervisor | Firecracker |
|---|---|---|
| **Rootfs model** | **virtio-fs** — a host dir served into the guest. Preserves sandboxd's existing containerd-overlay bundle (`containerd.go` prepares an overlay snapshot; the microVM serves that dir over virtio-fs). | **block device only** (ext4/devmapper). No virtio-fs for rootfs. Forces a new block-image assembler and a devmapper/thin-pool snapshotter — the k8s `kata-fc` example itself warns "a stock containerd with the default overlayfs snapshotter will fail at pod-sandbox creation." |
| **Snapshot/restore code** | **Already written and debugged in substrate** (`cmd/ateom-microvm/{checkpoint,restore}.go`, `internal/ch`): userfaultfd `OnDemand` restore, sparse-delta merge, fd-passed taps. Lift-and-adapt. | Native snapshot API is simple and well-documented, but the sandboxd integration is greenfield. |
| **Ecosystem** | Less "AWS-standard." | AWS heritage; the sig-k8s agent-sandbox Firecracker example uses it (via `kata-fc`). |

**Decision: Cloud Hypervisor first.** It keeps sandboxd's overlay rootfs model intact and
lets us port substrate's working C/R rather than build it. Firecracker stays a *possible
second microVM driver* behind the same interface (§7), gated on a block-rootfs redesign.

> **Note — they are NOT snapshot-compatible.** A Firecracker snapshot cannot be loaded by
> Cloud Hypervisor or vice-versa (distinct device-state serialization, distinct APIs:
> FC `PUT /snapshot/create|load`, CH `PUT /api/v1/vm.snapshot|vm.restore`). "Use CH but
> borrow Firecracker's snapshotting" is not a thing — each VMM only snapshots the VMs it
> runs. The good news is CH already has the identical capability set, so we lose nothing.

### 2.2 Direct-drive over Kata-via-containerd (`kata-fc` / `kata-clh` RuntimeClass)

There are two ways to run a microVM under Kubernetes:

1. **RuntimeClass + kata-deploy** (`runtimeClassName: kata-clh`): the kata containerd shim
   boots the VM. Standard, K8s-native, minimal bespoke code. **But the shim does not
   expose a pause/snapshot/restore control surface to us** — teleport, on-demand suspend,
   checkpoint-on-terminate, and forkset all vanish. That is the entire reason sandboxd
   exists. Dead end for our use case.
2. **Direct-drive** (this PRD): the worker pod shells out to the `cloud-hypervisor` +
   `virtiofsd` binaries and drives the kata-agent in-guest itself — exactly as sandboxd
   already "drives a pinned runsc binary" (`main.go:1-5`), and exactly as substrate's
   `ateom-microvm` drives CH over its REST API. Keeps full C/R control.

**Decision: direct-drive.** It is the only option consistent with sandboxd's architecture
and its teleport guarantee, and it maximizes reuse of substrate's implementation.

## 3. Feasibility — the abstraction seam is narrow, and mostly already clean

The best news from reading the code: **gVisor coupling is concentrated in one file**, and
the layers above it are already runtime-agnostic in shape.

### 3.1 The control-plane → worker contract is already runtime-neutral

`shared/sbxapi/sbxapi.go` is a plain HTTP/JSON contract — `RunRequest`, `CheckpointResponse`,
`RestoreRequest`, `SuspendRequest`, etc. — with **only one runtime-specific field**:
`RunscVersion` (on `CheckpointResponse` and `RestoreRequest`). This maps almost 1:1 onto
substrate's 3-verb `Ateom` gRPC service (`RunWorkload` / `CheckpointWorkload` /
`RestoreWorkload`). The operator's `sandboxdclient` has **zero** gVisor references. So the
wire is ready; it needs one generalization (§5.2).

### 3.2 The worker already isolates the runtime behind one driver type

`runscDriver` in `runsc.go` is the *entire* runtime integration: `createStart` (→
`runsc run -detach`), `checkpoint`, `restore`, `state`, `delete`, `version`. Every HTTP
handler in `main.go` calls `s.runsc.<verb>`. Extracting a `RuntimeDriver` interface from
this concrete type is the keystone refactor (§5.1) and touches only method-call sites.

### 3.3 The OCI-spec and rootfs paths are OCI-generic, not gVisor-specific

`spec.go`'s `ociSpec()` builds a standard OCI spec ("that runsc accepts", but it's generic
OCI). `containerd.go` pulls the image via node containerd and prepares an **overlay
snapshot** as the bundle rootfs. The gVisor-*specific* flags (`--network=sandbox`,
`-overlay2=root:dir=`, `--directfs=false`) live in `runsc.go base()`, **not** in the spec.
CH serves that same overlay dir over virtio-fs, so this path is largely reusable — the
biggest reuse win from choosing CH.

### 3.4 substrate hands us the hard 20%

The genuinely hard, bug-prone microVM work — userfaultfd `OnDemand` restore, the
sparse-snapshot delta-merge back onto the base memory-ranges (CH's next snapshot after an
OnDemand restore is sparse), fd-passed taps via `SCM_RIGHTS`, the tmpfs-overlay rootfs
that lets a memory-only VM snapshot also capture guest disk writes — is **already
implemented** in `substrate-upstream/cmd/ateom-microvm` and `internal/ch`. We port and
adapt it to sandboxd's worker rather than inventing it.

## 4. The real blockers (specific, not vague)

### 4.1 Networking is a per-worker singleton hard-wired for gVisor netstack (the big one)

`network.go` builds **one** interior netns (`sbx-net`), **one** interior IP
(`interiorIP = 169.254.17.2`), one host veth (`sbx0`), one nftables table (`sbx_net`), and
the gVisor sandbox **joins that netns** via the OCI spec's network-namespace path. runsc's
own netstack terminates the veth. A microVM does **not** join a netns that way — CH's
virtio-net wants a **tap device**, and the guest kernel (not a userspace netstack) runs
the TCP/IP stack.

**What it needs (substrate's `ateom-microvm/net.go` is the template):**
- Keep the *same addressing scheme* (`169.254.17.1/.2`, table `sbx_net`) so the router's
  DNAT-to-`podIP:hostPort` contract is unchanged — substrate deliberately uses identical
  IPs across both runtimes for exactly this reason.
- Instead of moving the veth peer into a netstack netns, build a **tap + TC-mirror**
  cross-connected to the veth, and hand the tap fds to CH's virtio-net (fd-backed) via
  `SCM_RIGHTS`.
- Configure the guest-side `eth0`/routes/ARP **through the kata-agent** (not netlink on
  the host peer).
- Use **fixed MACs** (host-veth + guest) so a restored guest's frozen ARP/neighbor state
  stays valid across teleport (substrate does this precisely to survive C/R).
- This is per-runtime code; it does not replace `network.go`, it's a sibling
  (`microvm_net.go`). The single-interior-IP singleton is acceptable at sandboxd's current
  **1:1 sandbox-per-worker** model (same as gVisor today); multi-sandbox-per-worker is a
  separate, shelved effort — see [PRD-multi-sandbox-per-worker.md §3.1](./PRD-multi-sandbox-per-worker.md).

### 4.2 Snapshot mechanics are entirely different below the contract

gVisor: `runsc checkpoint -image-path <dir>` writes `checkpoint.img` + pages; restore is
`runsc restore -image-path`. The overlay upper (guest fs writes) is captured because runsc
checkpoints the overlay. **All of that is gVisor-internal.**

CH: `vm.pause` → `vm.snapshot file://<dir>` produces `config.json` + `state.json` +
`memory-ranges`; restore relaunches a bare VMM, rewrites per-actor socket paths in
`config.json`, reconstructs the virtio-fs RO lower from the (re-pulled) OCI image, rebuilds
the tap + passes fresh fds, then `vm.restore` with `memory_restore_mode: OnDemand`
(userfaultfd) + `vm.resume`. Guest **fs writes live in a guest tmpfs upper**, so they are
captured *inside the memory snapshot* — nothing rootfs-specific ships. **The
"write files to a dir, report their names, the worker ships them to S3" contract is the
only thing shared** with the gVisor path — which is already exactly how `main.go`'s
`/checkpoint` and `/suspend` work (they `uploadDir(imgDir)` then record an S3 prefix). So
the worker's S3-shipping code is reusable as-is; only what lands *in* `imgDir` changes.

### 4.3 The version-guard is gVisor-shaped and must become a `{runtime, version}` tuple

`main.go:433-435` refuses a restore when `req.RunscVersion != s.runsc.version()`, and the
snapshot metadata carries `RunscVer`. CH snapshots are **also** binary-version-pinned
(a CH snapshot only restores on a matching CH build + compatible HW). But a CH snapshot
must **never** be restored by a runsc worker or vice-versa — a naive version-string
compare would let a `""`-versioned mismatch slip through. We generalize the guard to a
`{runtime, version}` pair (§5.2) and make cross-*runtime* restore a hard 409, independent
of version.

### 4.4 The worker image + node requirements diverge per runtime

gVisor workers need the pinned `runsc` binary (COPY'd into `Dockerfile.worker`,
`SANDBOXD_RUNSC` env) and run fine on any node. A CH worker needs `cloud-hypervisor` +
`virtiofsd` + a guest **kernel** + kata-agent-bearing guest **image** assets, **and
`/dev/kvm`** (hardware virtualization → nested-virt-capable EC2, e.g. `*.metal` or
nested-virt instance families; a node label + `/dev/kvm` device). So a CH pool needs a
different worker image *and* different node placement than a gVisor pool. This is why
runtime selection lands at the **pool** granularity (§5.3), not per-session.

### 4.5 Teleport caveats are sharper for a VM than for gVisor

Substrate + the CH/FC snapshot docs flag these; sandboxd already handles some:
- **Network state not guaranteed across restore.** sandboxd already *rebuilds* the veth/tap
  fresh on restore (`main.go` re-runs `setupSandboxNet`), so this matches our model — good.
- **Clock/wall-time freezes at snapshot.** Guest sees time jump on resume; may need a
  guest-side ntp/clock fixup for long-suspended sessions. (gVisor has a milder version of
  this.)
- **Entropy / unique-identity replication on fork.** Forkset fan-out from one CH snapshot
  clones PRNG/entropy state across children — CH exposes a VMGenID-style mechanism to
  reseed the guest on resume; forkset (§6, Phase 3) must wire it. gVisor forkset has an
  analogous concern already noted in [PRD-snapshot-fork.md](./PRD-snapshot-fork.md).
- **Block/disk consistency is the caller's job.** N/A for us — rootfs writes are in the
  tmpfs-in-snapshot upper, and durable state is out-of-band (S3), so there's no external
  block device to flush.

## 5. Design

The abstraction is lifted from substrate's proven shape: **a narrow driver interface, one
implementation per runtime, selected out-of-process at the pool granularity, unified only
by the existing HTTP contract + a generalized version/runtime token.**

### 5.1 Extract a `RuntimeDriver` interface (worker-internal)

In `checkpoint-restore/sandboxd/`, introduce:

```go
// RuntimeDriver is the sandbox runtime seam. runscDriver is the gVisor impl;
// chDriver (Cloud Hypervisor) is the second. main.go's handlers call only this.
type RuntimeDriver interface {
    // CreateStart boots a fresh workload from a prepared bundle, detached.
    CreateStart(id, bundle string) error
    // Checkpoint writes an atomic snapshot into imageDir. leaveRunning keeps the
    // sandbox live (periodic checkpoint); compress trades restore speed for size.
    Checkpoint(id, imageDir string, leaveRunning, compress bool) error
    // Restore establishes + resumes the sandbox from imageDir in one step.
    Restore(id, bundle, imageDir string) error
    // State returns "running"|"stopped"|... for the supervisor/health path.
    State(id string) (string, error)
    // Delete tears the sandbox down fast and robustly (frees the worker).
    Delete(id string) error
    // Runtime identifies the family ("gvisor"|"microvm") for the restore guard.
    Runtime() string
    // Version is the pinned engine version, recorded in snapshot metadata.
    Version() string
}
```

`runscDriver` already satisfies all of this (rename its methods to exported / add thin
wrappers; `Runtime()` returns `"gvisor"`). `server.runsc *runscDriver` (`main.go:24`)
becomes `server.rt RuntimeDriver`, selected at startup from an env
(`SANDBOXD_RUNTIME=gvisor|microvm`, default `gvisor`). **No handler logic changes** — they
already call one driver object. This refactor ships first, unchanged behavior, gVisor-only
(§6 Phase 0).

> Note on the interface boundary: `base()`, `overlayDir()`, the console/log tailing, and
> the gVisor debug-log surfacing are runsc-specifics that stay **inside** `runscDriver`.
> The CH driver has its own equivalents (CH serial console, CH API-socket log). Only the
> seven methods above cross the seam.

### 5.2 Generalize the version-guard to `{runtime, version}`

- `sbxapi`: add `Runtime string` alongside the existing `RunscVersion` on
  `CheckpointResponse` and `RestoreRequest` (keep `RunscVersion` for wire-compat; a CH
  snapshot populates `Runtime="microvm"` and puts the CH version in a new generic
  `EngineVersion`, deprecating the runsc-named field over one release). Simplest: add
  `Runtime` + `EngineVersion`, treat empty `Runtime` as `"gvisor"` and empty
  `EngineVersion` as `RunscVersion` for back-compat.
- `main.go` restore guard becomes: **409 if `req.Runtime != rt.Runtime()`** (cross-runtime,
  hard), **then** 409 if `EngineVersion` mismatches (as today). This makes it impossible to
  restore a CH snapshot on a runsc worker or vice-versa.
- The `sandbox` metadata struct (`main.go:41-52`) records `Runtime` + `EngineVersion`
  instead of just `RunscVer`.
- CRD: `BaseSnapshot.status` gains `runtime` next to the existing version field so forkset
  pools are matched on runtime, not just version (`forkset` already says the pool "must be
  runsc-compatible with the base" — generalize to "runtime + engine-version compatible").

### 5.3 Runtime selection is declarative, at the pool (matching substrate)

Runtime is a **worker-shape + placement** property (different image, needs `/dev/kvm`,
different nodes), so it belongs on the **`SandboxTemplate`** (the pool's worker blueprint),
NOT the `AppTemplate`/`Session` (the workload). This mirrors the clean split the
generic-pool work established and substrate's `WorkerPool.spec.sandboxClass`.

Add to `SandboxTemplateSpec`:

```go
// runtime selects the sandbox engine for this pool's workers: "gvisor" (default,
// nested runsc) or "microvm" (Cloud Hypervisor). This picks the worker image's
// engine AND the pod shape (microVM needs /dev/kvm + KVM-capable nodes). A session
// only ever teleports within its own pool, so a pool is single-runtime by
// construction — which is also required, since a snapshot is not restorable across
// runtimes (see the restore guard).
// +kubebuilder:validation:Enum=gvisor;microvm
// +kubebuilder:default=gvisor
// +optional
Runtime string `json:"runtime,omitempty"`
```

The operator then, in `desiredDeployment` (`warmpool_controller.go:351-427`), applies a
**per-runtime pod shape** when `Runtime == "microvm"`:
- add `/dev/kvm` (device plugin request or a `hostPath` device mount + privileged — the
  worker is already `Privileged: true`),
- set `SANDBOXD_RUNTIME=microvm` in `workerEnv`,
- default the worker image to the operator's **microVM** worker image (a second global
  default, e.g. `--microvm-worker-image`, overridable via the existing
  `SandboxTemplate.spec.workerImage`),
- (placement) the KVM node requirement is expressed by the operator-authored
  `spec.scheduling` (nodeSelector/toleration for KVM-capable nodes) — this stays
  pass-through, consistent with the existing "operator injects NO scheduling defaults"
  contract (`common_types.go:98-127`). Document the required `nodeSelector`/toleration in
  the install guide, as we do for `sandbox: gvisor` today.

> Substrate has a `maybeApplyMicroVMPodShape` that hardcodes exactly this (`/dev/kvm` +
> node label) and carries its own TODO that per-class pod requirements should be
> data-driven rather than branched in the controller. **Heed it:** put the per-runtime pod
> deltas (device mounts, extra env, image key) in a small **table keyed by runtime**, not
> an `if microvm {}` block, so a third runtime (Firecracker) is a table row, not a new
> branch.

### 5.4 The Cloud Hypervisor driver (`chDriver`) — port from substrate

> **Source of truth (verified first-hand 2026-08-11 against `substrate-upstream`).**
> Correcting the earlier (subagent-summarized) paths: substrate's CH + kata packages are
> NOT top-level `internal/` — they live under the command tree at
> **`cmd/ateom-microvm/internal/ch/`** and **`cmd/ateom-microvm/internal/kata/`**; the
> vendored kata protos are at **`cmd/ateom-microvm/internal/third_party/kata/agentpb/`**
> (kata tag **3.31.0**, Apache-2.0, message types only — no ttrpc service stub; the driver
> calls agent RPCs by string method name). The ateom gRPC contract is at
> `internal/proto/ateompb/ateom.proto`. Module `github.com/agent-substrate/substrate`, Go 1.26.

New files under `checkpoint-restore/sandboxd/` (or a `runtime/microvm` subpackage):
- `ch_driver.go` — implements `RuntimeDriver`; owns a `running map[id]*vm` and serializes
  ops under a mutex (substrate's `AteomService.lock`).
- `ch/` — CH REST client over the per-sandbox api-socket. **Port from
  `cmd/ateom-microvm/internal/ch/`** (`ch.go`, `api.go`, `createvm.go`, `restorefds.go`,
  `merge.go`). CRITICAL detail: CH is driven **two ways over the same unix api-socket** —
  (a) stdlib `net/http` with a unix-dialing `Transport` + `DisableKeepAlives` for normal
  calls (`vm.create`/`vm.boot`/`vm.pause`/`vm.snapshot`/`vm.resume`/`vm.info`), and
  (b) **raw HTTP/1.1 over `net.DialUnix` + `WriteMsgUnix`/`SCM_RIGHTS`** whenever fds ride
  along (`vm.add-net` on boot, `vm.restore` on restore) — `net/http` can't attach ancillary
  data. You need both. `MergeDeltaIntoBase` (merge.go, `//go:build linux`, uses
  `SEEK_DATA`/`SEEK_HOLE`) reconstitutes a full snapshot after an OnDemand (sparse) restore.
- `kata/` — kata-agent ttrpc client + OCI→kata spec conversion + virtiofsd launch + overlay
  setup. **Port from `cmd/ateom-microvm/internal/kata/`** + `cmd/ateom-microvm/internal/
  third_party/kata/agentpb/`. NO kata shim, NO containerd: the driver dials CH's hybrid-vsock
  unix socket, does the plaintext `CONNECT 1024`/`OK` handshake itself, then `ttrpc.NewClient`
  and calls `grpc.AgentService`/`<Method>` by name (containerd/ttrpc v1.2.8). Rootfs =
  overlay(virtio-fs RO lower from the OCI image + guest **tmpfs** upper) so rootfs writes live
  in guest RAM and are captured by the memory snapshot.
- `microvm_net.go` — the veth-into-interior-netns + **tap/TC-mirror + fd extraction**
  networking (§4.1). **Port from `cmd/ateom-microvm/net.go`.** Uses `google/nftables` (not
  shelling `iptables`) and **fixed MACs** (`hostVethMAC`/`actorGuestMAC`) + constant
  `169.254.17.0/30` so the guest's frozen ARP/MAC survive restore.

Deps the port pulls in (from substrate go.mod): `containerd/ttrpc`, `google/nftables`,
`vishvananda/netlink`+`netns`, `opencontainers/runtime-spec`, `pelletier/go-toml/v2`,
`hashicorp/go-reap`, `golang.org/x/sys/unix`. All microVM files carry `//go:build linux`
with a `*_unsupported.go` stub, matching substrate.

Method mapping (RunWorkload boot order verified in `run.go:RunWorkload`):
- `CreateStart` → resolve runtime assets → `setupActorNetwork` (veth into interior netns) →
  build containers via `ensureKataCompatibleSpec` + reuse sandboxd's `containerd.go` for the
  overlay rootfs → `StartVirtiofsd` (subprocess, `--migration-mode find-paths`) → `LaunchVMM`
  (CH subprocess, NOT `CommandContext` — must outlive the RPC) → `CreateVM` → `setupRestoreTap`
  + `AddNetWithFDs` (SCM_RIGHTS) → `BootVM` → `DialAgent` (hybrid-vsock) → `CreateSandbox` →
  guest net (`UpdateInterface`/`UpdateRoutes`/`AddARPNeighbors`) → per-container carrier +
  overlay `CreateContainer`/`StartContainer` → `readyz.WaitAll`.
- `Checkpoint` → `vm.pause` → `vm.snapshot file://<imageDir>` → (if restored-from-OnDemand)
  `MergeDeltaIntoBase` → (leaveRunning ? `vm.resume` : teardown). The worker's existing
  `uploadDir(imageDir)` ships it (report the written file list, as substrate's `snapshot_files`).
- `Restore` → `LaunchVMM` bare → rewrite per-actor socket paths in snapshot `config.json` →
  reconstruct RO lower from re-pulled image (`ReconstructSharedDirFromImage`) → rebuild tap +
  fresh fds → `RestoreWithNetFDs {memory_restore_mode: OnDemand, net_fds}` (raw socket) →
  `vm.resume`. Keep the snapshot dir for the VM's whole lifetime (OnDemand demand-pages from it).
- `State`/`Delete`/`Version`/`Runtime` → CH `vm.info` state / VMM teardown + `CleanupSandboxState`
  / CH `--version` / `"microvm"`.

### 5.5 Touch list (by layer)

- **Worker (new):** `ch_driver.go`, `ch/*`, `kata/*`, `microvm_net.go` (ported).
- **Worker (edit):** `main.go` — `RuntimeDriver` field + startup selection + generalized
  restore guard + `Runtime`/`EngineVersion` on `sandbox` metadata; `runsc.go` — satisfy the
  interface (mechanical). No handler-body changes.
- **Wire:** `sbxapi.go` — add `Runtime` + `EngineVersion` (back-comat with `RunscVersion`).
- **CRD:** `sandboxtemplate_types.go` — add `Runtime` enum field; `basesnapshot_types.go` —
  add `runtime` to status; regenerate CRD YAML in both `charts/sandboxd/crds/` and
  `config/crd/bases/`.
- **Operator:** `warmpool_controller.go` `desiredDeployment` + `workerEnv` — per-runtime pod
  shape via a runtime table (image key, `/dev/kvm`, `SANDBOXD_RUNTIME`); forkset/resume
  pool-eligibility filters on `runtime` too.
- **Packaging:** a new `Dockerfile.worker.microvm` (or a build arg) bundling
  `cloud-hypervisor` + `virtiofsd` + guest kernel + kata-agent image; Makefile/CI targets to
  fetch/pin those assets (analogous to today's `fetch-runsc`); a second `--microvm-worker-image`
  operator default.
- **Docs:** install-guide (KVM node prep, node labels), architecture-sandboxd (two-runtime
  model), a runbook for microVM pools.

## 6. Proposed shape (phased)

- **Phase 0 — the seam, gVisor-only (no behavior change).** Extract `RuntimeDriver`; make
  `runscDriver` implement it; `server.rt RuntimeDriver` selected by `SANDBOXD_RUNTIME`
  (default gvisor). Generalize the wire to `{Runtime, EngineVersion}` with back-compat and
  make cross-runtime restore a hard 409. Add the `Runtime` CRD field (accepted, only
  `gvisor` wired). **Ships independently; de-risks everything; zero functional change.**
- **Phase 1 — CH boot + run (no teleport).**
  - **1a — activate the restore guard (DONE 2026-08-11).** Thread the snapshot's
    `{runtime, engineVersion}` end-to-end so the Phase 0 guard is live: worker `/suspend`
    (and `/checkpoint`) responses carry `runtime`+`engineVersion` (`SuspendResponse`,
    `CheckpointResponse`); the operator records them on `SessionEntry` + mirrors them into
    `Session.status` (lossless durable rebuild); resume replays them on `RestoreRequest`.
    A cross-runtime or version-mismatched resume now hard-409s at the worker. Prerequisite
    to any microVM pool coexisting with gVisor. (Unit test: `TestResumeReplaysRuntimeGuard`.)
  - **1b — CH boot + run (DONE 2026-08-12).** Ported `ch/` + `kata/` + `microvm_net.go` +
    overlay/virtiofsd; `chDriver.createStart`/`state`/`delete`. A microVM pool boots a
    workload and serves it (per-runtime pod shape + nested-virt KVM nodes). Validated
    isolation + rootfs + net model end-to-end live (redis in a CH microVM via the driver).
    Key fix: retry the kata-agent vsock CONNECT until the guest is up (CH creates the socket
    file at vm.create, long before the agent listens). Commits `b83aaaf`, `43a850b`.
- **Phase 2 — CH teleport (DONE 2026-08-12).** `chDriver.checkpoint`/`restore` (eager Copy
  restore on CH v53, which prefaults OnDemand; delta-merge path retained for a future
  non-prefaulting CH; fd-passed tap rebuild; snapshot socket-path rewrite). Wired through
  the existing `/checkpoint`, `/restore`, `/suspend` handlers (runtime-neutral contract, no
  handler changes) + the operator per-runtime pod shape (`/dev/kvm` + `SANDBOXD_RUNTIME`).
  **Live-proven**: RUN → checkpoint (2GiB snapshot → S3) → reset → restore (same id) →
  Running in ~11s, guest RAM resumed. Load-bearing fix: **virtiofsd v1.14.0**, sourced
  separately from upstream — kata-static 4.0.0's bundled v1.13.x has an old vhost-user that
  HANGS CH's snapshot/restore migration handshake. Commits `e28d21f`, `e7b4f72`.
- **Phase 3 — forkset + hardening (DONE 2026-08-12; density tuning deferred).** The ForkSet
  controller is runtime-neutral (it seeds each child Session `Suspended+snapshotURI` and the
  existing resume path restores it via `chDriver.restore`), so no new *fan-out* code was
  needed. But running the REAL forkset example live (golden Session → suspend → `BaseSnapshot`
  → `ForkSet{count:2}`, driven through the router) surfaced that "fork" exercises paths the
  Phase 1b/2 worker-local `/run` tests never did, and exposed three concrete gaps — all now
  fixed and live-validated (commits `21d3305`, `0fc3370`). **Fork proof:** both forks restore
  the IDENTICAL golden state (same `sum`, same `boot` uuid = frozen RAM+FS) yet return
  DIFFERENT `/dev/urandom` — the clones diverge only because of the entropy reseed.
  - **Entropy reseed (VMGenID analog).** After restore, `kata-agent ReseedRandomDev` injects
    fresh per-clone host entropy, so clones restored from one memory image don't share the
    frozen CRNG state and emit identical randomness (§4.5). This is the load-bearing fork
    correctness fix — proven by the divergent `/rand` above.
  - **Clock-fixup.** After restore, `kata-agent SetGuestDateTime` sets the guest wall clock
    to the host's current time (the guest's clock is frozen at snapshot time).
  - **Observability parity.** A per-VM background watcher loops on the agent's blocking
    `GetOOMEvent` and logs + counts guest-cgroup OOMs (invisible to the host cgroup the
    gVisor path scans). Both restore fixups are best-effort; the OOM watcher is started on
    boot/restore and stopped on delete.
  - **Inbound routing + IAM (the big one; commit `0fc3370`).** The router→guest datapath and
    per-session IAM creds were BOTH silently unwired for microVM: gVisor's `setupSandboxNet`
    installs the pod→sandbox DNAT *and* pins the credential-vendor IP (169.254.170.2) in the
    POD netns, but the microVM guest lives one veth hop away in the `sbx-microvm` INTERIOR
    netns, so a router request got `connection refused` and a guest's
    `AWS_CONTAINER_CREDENTIALS_FULL_URI` fetch never reached the vendor. The Phase 1b/2 tests
    missed this because they drove the sandbox over the worker's *own* localhost `/run`, never
    through the router. Fix (deliberately RETAINS gVisor's routing model rather than
    substrate's atunnel ingress): add `runtimeDriver.buildsOwnNetwork()` (gVisor=false,
    microVM=true); the microVM driver installs the SAME `installNft` DNAT (pod netns →
    169.254.17.2, the guest's IP over `ateom0`) and pins the cred-vendor IP on `ateom0`;
    `/run`, `/restore`, and the supervisor skip `setupSandboxNet` when the driver owns its
    network; `createStart`/`restore` gained a `ports []portMap` arg.
    - **IAM live-validated end to end.** A microVM sandbox with `iam.roleArn` set (on the
      AppTemplate/SandboxTemplate/Session) received real per-session STS credentials in-guest:
      the guest read `AWS_CONTAINER_CREDENTIALS_FULL_URI=http://169.254.170.2:8091/creds/<sid>`,
      reached the worker cred vendor over the `ateom0` pin, and got back temporary keys
      (an `ASIA…` AccessKeyId + session token, ~1h expiry) for the assumed session role —
      confirmed by a matching `sts:AssumeRole` event in CloudTrail. This is the same
      cred-vendor + container-credentials contract the gVisor path uses, now working across
      the microVM netns boundary. Two follow-ups from this validation are FIXED (commit
      `ce15b50`): (i) the vendor's `sts:AssumeRole` now runs under a fresh context, not the
      client request's, so a client that times out on the slow FIRST (cold) assume no longer
      cancels it — it completes and caches, and the retry hits the warm cache (this hardens
      BOTH runtimes, since the cred vendor is shared); (ii) the cred-vendor pin is now split
      out of the `len(ports)>0` guard in `setupInboundPorts`, so a PORTLESS IAM-only sandbox
      (an outbound-only agent — the shape that most needs IAM and has no reason to expose a
      port) still gets the pin. Verified live: a portless sandbox boots with no inbound DNAT
      yet the cred-vendor IP pinned on `ateom0`.
  - **Fork base-id (commit `0fc3370`).** A fork restores under a NEW sandbox id, but the
    guest's virtio-fs find-paths are frozen at the golden id's path
    (`SharedDir(baseID)/<baseID>/rootfs`), so `vm.restore` failed with a vhost-user
    `BackendInternalError`. Record the frozen base id at checkpoint (`clh-baseid`, from
    `chVM.baseID`) and reconstruct the RO lower + serve virtiofsd at that base path on
    restore; `baseID` is the sandbox's own id on cold boot and propagates across a fork
    lineage. (This is sandboxd's analog of substrate's `baseIDFile`.) A same-id resume has
    `baseID == id`, which is why Phase 2 teleport passed without it — only a fork exposed it.
  - **Workload log forwarding (commit `0fc3370`).** `streamLogsToStdout` now forwards the
    WORKLOAD container's stdout+stderr (kata-agent `ReadStdout`/`ReadStderr` for exec
    `<id>_ovl` via `NewStdioReader`) to worker stdout → `kubectl logs`, not the VM/kata-agent
    serial console. Opt-in per pool (`SANDBOXD_STREAM_CONSOLE=1` via
    `SandboxTemplate.streamConsole`). `recentLogs` still returns the serial console as a
    coarse `/logs` fallback.
  - **Deferred:** microVM density / cold-start tuning (measurement-driven, no concrete need
    yet); container-exit (non-OOM) surfacing via a `state()` agent query.
- **Sparse-aware S3 transfer (separate PRD).** The current `uploadDir` streams the whole
  dense memory-ranges file, so a small working set ships as the full guest RAM (a 154MiB
  working set uploaded/downloaded as 2GiB), dominating suspend/restore latency and S3 cost.
  Split into its own PRD: `PRD-sparse-checkpoint-s3-transfer.md` (answers §8 Q5).

## 7. Firecracker as a later, third driver (explicitly out of scope now)

The whole point of the `RuntimeDriver` seam + runtime table is that Firecracker becomes a
**fourth-verb-compatible third impl** (`fcDriver`, `Runtime()="microvm-fc"` or a distinct
class), not a rewrite. It reuses the interface, the version/runtime guard, the pool
selection, and the S3-shipping contract. What it needs *extra* is the block-device rootfs
model (§2.1) — a devmapper/ext4 image assembler replacing the virtio-fs overlay path — and
its own snapshot integration (`PUT /snapshot/create|load`, UFFD). Defer until (a) CH is
proven end-to-end and (b) there's a concrete driver for the AWS-alignment/block-rootfs
tradeoff. Keeping the seam runtime-plural from Phase 0 is what makes this cheap later.

## 8. Open questions

1. **KVM on EKS — ANSWERED.** Nested virtualization on standard Nitro instances (C7i/M7i/
   R7i/etc.) via Karpenter v1.14 `EC2NodeClass.spec.cpuOptions.nestedVirtualization: enabled`,
   which auto-filters to capable types — NO bare metal, NO ODCR. Live-validated: `/dev/kvm`
   present, CH boots + teleports. Bare-metal was a persistent capacity crunch and ODCRs were
   account-unusable in this environment (both dead ends). The KVM AMI (`packer/`) is reused
   as-is; the instance provides VT-x. See [[sandboxd-microvm-nested-virt]].
2. **Guest image supply chain — ANSWERED (consume kata assets, pin virtiofsd separately).**
   The worker image bundles the kata-static 4.0.0 guest kernel (`vmlinux.container`) + rootfs
   (`kata-containers.img`), fetched by `make fetch-microvm-assets`. CRITICAL FINDING:
   virtiofsd must be sourced SEPARATELY (`v1.14.0` from upstream, sha-pinned) — kata-static
   4.0.0's bundled v1.13.x has an old vhost-user (pre vhost-0.16 / vhost-user-backend-0.22)
   that HANGS CH's snapshot/restore migration handshake (diagnosed live: CH deadlocks in
   `futex_wait`/`unix_stream_data_wait`, virtiofsd at 0 CPU). Substrate pins v1.14.0 for the
   same reason.
3. **Cold-start budget — PARTIALLY MEASURED.** Cold boot (RUN → Running) is ~2–3s incl.
   image pull + CH boot + kata-agent handshake. A same-id teleport-restore is ~11s, dominated
   by the DENSE memory-image download (see Q5) — NOT CH itself. The vsock CONNECT handshake
   needs retrying until the guest agent listens (~4s into boot); a single early CONNECT gets
   EOF (fixed in `DialAgentRetry`). Per-runtime `minIdle` sizing + fine virtio-fs prep cost
   are still open (part of the deferred density tuning).
4. **Restore-dir lifetime — CONFIRMED + moot today.** With CH v53 (which prefaults OnDemand),
   restores use EAGER `Copy`, which reads the whole image up front and is self-contained — so
   the worker drops the staged memory image after restore and there is NO whole-lifetime
   demand-page dependency. The OnDemand delta-merge path + restore-source retention is kept in
   code for a future non-prefaulting CH, gated on `chVM.snapshotSelfContained`.
5. **Snapshot size / S3 cost — CONFIRMED as the main cost; own PRD.** A guest-RAM image is a
   sparse file (e.g. 2GiB logical / ~154MiB resident), but the worker's S3 transfer is
   hole-blind, so it ships + stores the full DENSE 2GiB and restore reads all of it (this is
   why restore needs a 10-min budget, not 120s). Split into
   `PRD-sparse-checkpoint-s3-transfer.md` (sparse-extent + zstd codec, lift from substrate).
6. **Is the isolation win worth the complexity?** STILL the open strategic question. The
   engineering is now DONE and proven (Phases 0–3, teleport + forkset at gVisor parity), so
   the cost side is de-risked and known; what remains is a product call — is there a workload
   that *requires* a real guest kernel / hardware boundary gVisor can't serve? Absent one on
   the roadmap, microVM is a proven, ready capability bet rather than a near-term need (§9).

## 9. Relationship to Substrate & recommendation

Substrate proves the pattern is sound and hands us the hardest code (CH C/R). But note
substrate's own `docs/architecture.md` flags much of its microVM path as partly
aspirational, and sandboxd's gVisor teleport is already live and proven
([[sandboxd-control-plane-state]]). So:

**UPDATE 2026-08-12: Phases 0–3 are DONE and live-proven** (branch `feat/microvm-runtime`,
PR #30). A microVM pool boots, serves via the router, teleports (checkpoint→S3→restore), and
forks (N clones from one base with divergent entropy) at gVisor parity, on nested-virt EKS
nodes. Q1/Q2/Q4 are answered above; Q5 (dense S3 transfer) is the one known-suboptimal area,
split into its own PRD. So the original recommendation is now largely settled:

- **Phase 0 (the seam) was a cheap, independently valuable cleanup** — it made the runtime
  pluggable, generalized the restore guard (a correctness win even for gVisor-only: a
  `""`-version snapshot no longer restores anywhere), and cost almost nothing.
- **Phases 1–3 turned out to be tractable** (the substrate port + a handful of load-bearing
  fixes: vsock-CONNECT retry, virtiofsd v1.14, base-id, inbound routing + IAM). The
  engineering risk is now retired. What remains is the STRATEGIC call in Q6 — is there a
  workload that *needs* a hardware boundary gVisor can't provide? With the capability now
  built and proven, adopting it for a given pool is a config flip (`runtime: microvm` on the
  SandboxTemplate), not a project; CH-direct-drive-ported-from-substrate was the lowest-risk
  path and it landed.
