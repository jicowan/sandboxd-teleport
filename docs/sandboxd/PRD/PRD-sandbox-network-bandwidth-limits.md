# PRD — per-sandbox network bandwidth limits (egress/ingress rate caps)

Status: **Proposed / ready to implement.** Written 2026-08-13. Grounded in a read of the
live worker networking (`checkpoint-restore/sandboxd/microvm_net.go`, `ateomnet/`,
`runsc.go`, `microvm_boot.go`) and the CH VmConfig (`ch/createvm.go`) — the mechanism
below cites what exists today.

Related: [architecture-networking-lifecycle.md](../architecture-networking-lifecycle.md)
(interior-netns data path), [PRD-worker-memory-reserve.md](./PRD-worker-memory-reserve.md)
+ issue #38 guest CPU/mem sizing (the other resource dimensions — network is the one
still uncapped), [architecture-sandboxd.md](../architecture-sandboxd.md) (worker-vs-sandbox
model + teleport).

## 1. Problem

A sandbox's network traffic is **completely uncapped** on both runtimes. All of it
funnels through an **interior network namespace** sandboxd builds per sandbox: the
workload sits at the stable interior IP `169.254.17.2` behind a veth pair, and its
**egress SNATs out through the worker pod's real CNI IP** (masquerade in
`microvm_net.go`); ingress is `nftables`-DNAT'd from `worker-pod-IP:hostPort` →
`169.254.17.2:appPort`.

Because egress rides the pod IP with no shaping, **one greedy sandbox can saturate the
worker's NIC** — starving the pod's control traffic (router→worker `/status`, checkpoint
uploads to S3, image pulls) and competing with every other worker pod on the node for
node bandwidth. There is no per-sandbox, per-pool, or per-session limit. Kubernetes can't
help: the sandbox has **no CNI pod IP**, so it is invisible to any bandwidth mechanism
that operates on pods (CNI bandwidth plugin, NetworkPolicy). CPU and memory are now
sizeable from the template (issue #38 / the memory reserve); network is the last
resource dimension with no ceiling.

## 2. Goal / non-goals

**Goal:** let an operator cap a sandbox's **egress** and **ingress** bandwidth
(bytes/sec, with a burst allowance), per pool (and optionally per session), enforced
**host-side** on the sandboxd-owned interior interface so it is **not guest-tamperable**
and **survives teleport** (re-applied on restore). Works identically for gVisor and
microVM.

**Non-goals:**

- **Not** packet-level QoS / DSCP / priority classes / fair-queuing between sandboxes.
  A flat token-bucket cap per sandbox only. (HTB class hierarchies are a possible future
  refinement — out of scope.)
- **Not** per-flow or per-destination limits. The cap is aggregate for the sandbox.
- **Not** connection/PPS limits or SYN-flood protection (that's a security concern for a
  separate effort).
- **Not** a change to the data path itself — the veth/tap/`mirred` plumbing and the
  `169.254.17.2` model stay exactly as they are; we only attach shaping qdiscs to the
  interface that already exists.
- **Not** node-total bandwidth accounting or overcommit scheduling (the operator does not
  bin-pack by bandwidth; a limit is a per-sandbox ceiling, not a reservation).

## 3. Why this is clean (mechanism)

Every sandbox's traffic — **both** runtimes — crosses an interface sandboxd **creates
itself** in the interior netns, and **re-creates on restore**:

- **gVisor:** the `ateom0`/actor veth pair in the interior netns (`ateomnet`
  `SetupActorNetwork`).
- **microVM:** the same actor veth, whose peer is bridged to CH's tap via the existing
  `tc` **ingress qdisc + `mirred` redirect** (`microvm_net.go:80–102`).

We already drive `tc` via netlink on that interface (the `mirred` mirror). Adding a
**shaping qdisc on the same veth** is the same toolchain, the same netns, the same
lifecycle hook — no new device, no new privilege, no data-path change. Because the qdisc
lives on the **host side** of the sandbox's veth, the guest cannot see or remove it. And
because `createStart` and the restore path both call `SetupActorNetwork` to rebuild the
veth, re-applying the qdisc there makes the cap **teleport-stable** (a restored sandbox on
a different worker gets the same cap).

## 4. Design

### 4.1 Enforcement (worker)

In `ateomnet`'s network setup (shared by both runtimes) and the restore path, after the
actor veth exists, attach two shapers keyed off a `BandwidthConfig{EgressBPS, IngressBPS,
BurstBytes}`:

- **Egress cap** (workload upload): a **TBF** (token-bucket filter) qdisc on the
  **sandbox-side** veth's root (egress). `rate = EgressBPS`, `burst = BurstBytes`
  (default a small multiple of `rate`/HZ), `latency` a sane default. Egress shaping is
  the simple, well-supported direction.
- **Ingress cap** (workload download): ingress shaping needs redirection through an
  **`ifb`** (intermediate functional block) device — add `ifb<N>` in the interior netns,
  redirect the veth's ingress to it with a `tc` filter, and put a TBF on the `ifb`'s
  egress. (Alternative: a plain **ingress policer**, which is simpler but drops rather
  than queues — acceptable for a v1 if `ifb` proves fiddly; policing is TCP-hostile, so
  prefer `ifb`+TBF.)

All via `netlink.Qdisc`/`netlink.Filter`, mirroring the existing `Ingress`+`mirred`
calls. Zero applies when a dimension is unset (uncapped — today's behavior).

### 4.2 microVM defense-in-depth (optional, phase 2)

Cloud Hypervisor's `NetConfig` supports a per-NIC **`rate_limiter`** (token bucket for
bandwidth + ops). Add a `RateLimiter` field to `ch.NetConfig`/the add-net call
(`ch/createvm.go` has none today) and set it from the same `BandwidthConfig`. Advantage:
enforced **in the VMM, below the guest kernel** — strictly stronger than the veth qdisc
for microVM. Disadvantage: microVM-only (no gVisor parity) and it must round-trip
`vm.snapshot`/`vm.restore` cleanly (verify — the tap fds are re-attached on restore, so
the limiter config must be re-supplied at add-net time, which the restore path already
does). **Recommendation:** ship the veth `tc` path first (parity, one mechanism); add the
CH limiter as opt-in hardening for microVM once the `tc` path is proven.

### 4.3 API surface

Per-pool, on the `SandboxTemplate` (consistent with `resources`), and on the
`AppTemplate` for generic pools (bandwidth is a workload property, mirroring the
workload subset those two CRDs share):

```yaml
spec:
  network:
    egressMbps: 100      # sandbox → outside (upload); 0/unset ⇒ uncapped
    ingressMbps: 100     # outside → sandbox (download); 0/unset ⇒ uncapped
```

**Transport = the `/run` + `/restore` request body (per-session), NOT worker env.**
The caps thread the same way `iamRoleArn` and `ports` already do: the operator resolves
the template (`toTemplateSpec` → `resume.TemplateSpec.{Egress,Ingress}Mbps`), the resume
workflow carries them through `bindSpec`, and `startAndBind` sets `EgressMbps`/`IngressMbps`
on the `sbxapi.RunRequest`/`RestoreRequest`. This was chosen over worker env
(`SANDBOXD_NET_*` on the Deployment, the #38 sizing pattern) so the cap is a per-session
value that needs no pod-template churn/rollout to change, and so a future per-session
override drops in without a new transport. The worker converts Mbit/s → bytes/sec
(`bandwidthFromMbps`) and applies `ateomnet.ApplyBandwidth` on the interior veth.

The burst size is not an API knob; the worker derives it from the rate (default rate/8,
min 32 KiB) inside `newTbf`.

Values are Mbit/s at the API (operator-friendly), converted to bytes/sec at the worker.

### 4.4 Lifecycle / teleport

- **Cold boot** (`createStart` → `SetupActorNetwork`): apply the shapers after the veth
  is up, before the workload starts.
- **Restore** (`microvm_checkpoint.go` restore → `SetupActorNetwork`): re-apply
  identically, so the cap follows the sandbox to the new worker. This is the critical
  teleport-safety property and must be tested (suspend a capped sandbox, resume, confirm
  the cap still holds).
- **gVisor parity:** the same `ateomnet` hook serves both, so gVisor gets it for free.

## 5. Backward compatibility

- **Unset ⇒ uncapped**, byte-identical to today. The feature is opt-in per pool; existing
  templates are unaffected and their worker Deployments do not change at all (the caps ride
  the per-session `/run`+`/restore` request — omitted fields default to 0/uncapped — so
  there is zero pod-template churn / rollout, and the worker only touches `tc` when a
  non-zero cap arrives via `bw.Zero()`).
- **Snapshot compatibility:** the shaper is host-side netns state, not part of the guest
  memory/rootfs checkpoint, so old snapshots restore fine; the new worker simply applies
  whatever cap its template/request specifies at restore time. (A snapshot taken under one
  cap can be restored under a different cap — the cap is a property of the destination
  pool/session, not the frozen guest. Document this.)

## 6. Caveats / open considerations

- **Ingress shaping is approximate.** You can only shape what has already arrived at the
  worker NIC; a TBF on `ifb` smooths delivery into the guest but can't prevent upstream
  from sending. For TCP this is fine (backpressure via drops/delay); for UDP floods it
  bounds delivery to the guest, not arrival at the node.
- **Egress SNAT interaction.** Egress is shaped on the veth *before* the pod-IP
  masquerade, so the cap is on the sandbox's own traffic — correct. It does **not** cap
  the pod's control-plane traffic (good: sandboxd's own S3/`/status` traffic is not on the
  sandbox veth).
- **`ifb` availability.** Requires the `ifb` kernel module on the node AMI. The packer
  microVM node image must load/allow it; if absent, fall back to an ingress **policer**
  and `log()` the degradation (don't silently run uncapped).
- **Not a fairness mechanism.** Two capped sandboxes on one worker (only relevant if
  multi-sandbox-per-worker ever ships) each get their own ceiling but nothing arbitrates
  the shared NIC between them. Flat caps only.
- **Node bandwidth is still finite.** Per-sandbox caps bound one tenant; they don't stop N
  workers on a node from collectively saturating the ENI. Node-level bandwidth is an
  instance-type/scheduling concern, out of scope here.
- **Observability:** consider surfacing `tc -s qdisc` drop/overlimit counters via
  `/metrics` (sandbox_net_drops) so a throttled workload is diagnosable rather than
  mysteriously slow — mirrors the OOM-kill visibility from Phase 3.

## 7. Success criteria

1. A pool with `network.egressMbps: 100` boots a sandbox whose measured egress
   (e.g. `iperf3`/`curl` from inside the sandbox) is capped at ~100 Mbit/s ± burst, on
   **both** gVisor and microVM.
2. Ingress cap likewise bounds download throughput.
3. Unset ⇒ uncapped, and the worker Deployment for an unset pool is byte-identical to
   pre-feature (no rollout).
4. The cap **survives teleport**: suspend a capped sandbox → resume on a different worker
   → the cap still holds (re-applied by the restore path).
5. The guest **cannot remove or raise** the cap from inside (the qdisc is host-side).
6. No regression in the existing data path (ingress DNAT, egress SNAT, IAM cred-vendor
   pin, the `mirred` mirror) — the full e2e matrix still passes.
