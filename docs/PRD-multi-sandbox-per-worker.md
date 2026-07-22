# PRD — multiple gVisor sandboxes per worker (density via per-sandbox limits)

Status: **Proposed / not scheduled.** Fleshed out 2026-07-22 (supersedes the earlier
stub). Grounded in a read of the live worker (`checkpoint-restore/sandboxd/*.go`) and
the operator assignment code — the feasibility analysis below cites what actually
exists today, not guesses.

Related: [[sandboxd-generic-pools]], [PRD-arbitrary-image-sessions.md §13](./PRD-arbitrary-image-sessions.md),
[architecture-sandboxd.md](./sandboxd/architecture-sandboxd.md) (worker-vs-sandbox model).

## 1. The idea & why it's compelling

Today sandboxd runs **exactly one nested gVisor sandbox per worker pod**. kube-scheduler
sizes the worker pod via `SandboxTemplate.spec.resources`; the single sandbox inside
effectively gets the whole pod. When a session is small (a tiny redis on a 500m/1Gi
worker) the pod's spare capacity is stranded.

If each sandbox got its own **cgroup limit**, one (larger) worker pod could host **many**
independent sandboxes, each capped — raising density the way Agent Substrate multiplexes
actors onto workers, but on EKS. This is the natural next lever after generic pools:
generic pools let *many apps* share a pool's *worker shape*; this lets *many sandboxes*
share a single *worker pod*.

## 2. Feasibility — what already exists (better than expected)

Reading the worker, several things are **already multi-sandbox-ready**:

- **State is a map, not a singleton.** `server.sb map[string]*sandbox` keyed by sandbox
  id; `/run` takes a per-request `SandboxID`; `/sandboxes` lists all; `state.go`
  persists/reloads the whole set. The worker already tracks N sandboxes.
- **Each sandbox already has its own cgroup.** `runsc.go`'s teardown kills a
  *per-container* cgroup (`cgroup.kill`), so runsc already runs each sandbox in its own
  cgroup v2 subtree. Adding a **limit** to that cgroup is an OCI-spec field, not new
  infra.
- **OCI spec is per-sandbox.** `writeOCISpec` builds a fresh bundle per sandbox; adding
  `spec.Linux.Resources` (CPU quota/shares, memory limit) is localized. runsc honors
  standard OCI `LinuxResources`.
- **Overlay/rootfs, checkpoint prefix, cred vendor registration are already per-id**
  (`overlayDir(id)`, `sandboxes/<id>/…`, `dropCred(id)`).

So the "run N containers on one worker" plumbing is largely present. The blockers are
**three specific singletons**, below.

## 3. The real blockers (specific, not vague)

### 3.1 Networking is a single per-worker interior netns + IP (the hard one)

`network.go` builds **one** persistent interior netns (`interiorNetNSName = "sbx-net"`,
`interiorNetNSPath = /run/netns/sbx-net`) with **one** sandbox IP
(`interiorIP = 169.254.17.2`), one host veth (`sbx0`, `169.254.17.1/30`), and one nftables
table (`sbx_net`) that DNATs `podIP:hostPort -> 169.254.17.2:containerPort`.
`setupSandboxNet(podIP, ports)` / `teardownSandboxNet()` take **no sandbox id** — they
operate on that single netns. Two port-exposing sandboxes on one worker would collide on
the IP, the netns, and the `podIP:hostPort` DNAT.

**What it needs:** per-sandbox networking — a netns + interior IP + veth + nftables
chain **per sandbox id**. Concretely:
- `interiorNetNS(id)` → `/run/netns/sbx-net-<id>`; `setupSandboxNet(id, podIP, ports)` /
  `teardownSandboxNet(id)`.
- An **interior IP allocator** (a /30 or a subnet per sandbox out of `169.254.17.0/24`,
  or a larger private range) instead of the fixed `.2`.
- **host-port allocation** on the pod IP: N sandboxes can't all claim `hostPort ==
  containerPort`. Either allocate a unique host port per sandbox (and report it back so
  the router dials the right one), or give each sandbox its own pod-routable path. The
  DNAT chain becomes per-sandbox.
- The **checkpoint/restore** path (`restore` re-establishes the veth with the *same*
  interior IP so the address survives teleport) must record + restore the per-sandbox IP
  and host port. The interior IP already travels conceptually with the session; make it
  explicit per sandbox.

This is the bulk of the work and the main risk.

### 3.2 The credential vendor address is a single AWS-allow-listed IP

`credVendorIP = 169.254.170.2` is fixed — and it *must* be, because AWS SDKs only
allow-list loopback / `.170.2` / `.170.23` for `AWS_CONTAINER_CREDENTIALS_FULL_URI`.
With one sandbox per worker the vendor binds `.2` on the host veth and every request is
that sandbox's. With N sandboxes sharing the vendor address, the vendor must **identify
which sandbox** a request came from (today the per-session HMAC token disambiguates the
role, but the *source* is assumed single). Options: keep the shared `.2` but key the
vendor's response on the **HMAC token → sandbox/role** (the token is already per-session
and teleport-safe), and ensure each sandbox's netns routes `.2` to the vendor. Needs
care but the token indirection already exists.

### 3.3 The assignment model is idle/busy binary (operator side)

`ClaimIdleWorker` does `SPOP` from a per-pool idle set and marks the worker `busy` with
one `sid`; `/capacity` returns `busy = len(list) > 0` ("one sandbox per worker" — its own
comment). The WarmPool `minIdle`/replica math counts *workers*. Multi-tenant workers
break all of this:
- KV needs **per-worker capacity accounting** — remaining CPU/mem or N free "slots" —
  not a boolean. Claim = "find a worker with a free slot big enough," decrement; release
  = increment.
- `pool:<pool>:idle` (a set of fully-idle workers) becomes a **capacity index** (workers
  with room, perhaps a sorted set by free capacity for best-fit).
- `minIdle` changes from "N idle workers" to "N free slots / X free CPU."
- The **worker-binding reclaim** sweep and `pod-deletion-cost` become **per-slot**: a
  worker is only reclaimable/cheap-to-delete when **all** its sandboxes are gone.
- `/capacity` returns per-worker free capacity + list of sandbox ids.

### 3.4 Checkpoint-on-terminate / teleport with co-tenants

When a worker pod terminates, checkpoint-on-terminate today suspends its one session.
With N, it must suspend **all** N within the grace period (or they die). And teleport of
*one* sandbox off a shared worker (e.g. to rebalance) must move just that sandbox's
netns/checkpoint while the others keep running — the per-sandbox networking (3.1) is a
prerequisite. Draining a shared worker is N checkpoints racing one grace window.

## 4. Where the per-sandbox limit lives (CRD)

Per-sandbox CPU/mem is a **workload** property with no placement implication, so it
belongs on the **`AppTemplate`** (e.g. `spec.resources`), NOT the `SandboxTemplate`
(which stays the pool's worker-shape). This is the clean split established by the
generic-pool work:
- `SandboxTemplate.spec.resources` = the **worker pod** size (scheduler bin-packing).
- `AppTemplate.spec.resources` = the **per-sandbox** cgroup cap (NEW).
The operator threads the AppTemplate resources into the `/run` request; the worker puts
them in the OCI spec's `Linux.Resources`.

## 5. Proposed shape (phased)

- **Phase 0 — per-sandbox limits, still one sandbox per worker.** Add
  `AppTemplate.spec.resources` → `RunRequest.Resources` → OCI `Linux.Resources`. No
  density yet, but ships the cgroup-limit mechanism and is independently useful
  (predictable per-sandbox caps, noisy-neighbor protection even at 1:1). Low risk;
  touches only spec plumbing.
- **Phase 1 — per-sandbox networking.** The 3.1 rework: per-id netns/IP/veth/nftables +
  IP allocator + host-port allocation + restore of per-sandbox IP/port. The keystone;
  do it behind the 1:1 model first (each worker still gets one, but via the per-id path)
  to de-risk before packing.
- **Phase 2 — multi-tenant assignment.** Operator KV → capacity/slot model; claim
  best-fit; per-slot reclaim + deletion-cost; `/capacity` reports free capacity;
  `minIdle` → free-slot target. Cred vendor per-sandbox (3.2).
- **Phase 3 — co-tenant lifecycle.** Drain = suspend-all-within-grace; per-sandbox
  teleport/rebalance; density tuning + overcommit policy.

## 6. Open questions

1. **Density target & worker sizing** — how many sandboxes per worker; fixed slots vs
   free-capacity best-fit; do we overcommit CPU (requests<limits)?
2. **Host-port strategy** — allocate a unique hostPort per sandbox (router must learn
   it), or a per-sandbox pod-routable scheme? This shapes the router contract (the
   router currently dials `workerPodIP:hostPort` from the session entry — already
   per-session, so likely just "record the allocated port," minimal router change).
3. **Interior IP range** — `169.254.17.0/24` is small; pick a range sized to max density.
4. **Blast radius** — N tenants share one privileged worker pod. gVisor is still the
   isolation boundary, but is co-tenancy on one pod acceptable for the multi-tenant
   goal, or should packing be within a single trust domain only?
5. **Is it worth it vs. just smaller workers?** Quantify: what density/cost win over
   "run more, smaller worker pods and keep 1:1"? (The 1:1 model with right-sized workers
   may capture much of the benefit with none of 3.1–3.4's risk — this must be justified.)
6. **Relationship to Substrate** — this is essentially Substrate's multiplexing bet on
   EKS; worth revisiting what they learned.

## 7. Recommendation

**Phase 0 is a cheap, independently-valuable win** (per-sandbox cgroup limits on the
AppTemplate — noisy-neighbor protection, predictable caps) and de-risks nothing else, so
it can ship on its own regardless of whether we pursue density. **Phases 1–3 are a real
project** gated on answering Q5 (is the density win worth the networking + assignment
rework?). Recommend: do Phase 0 when convenient; treat Phases 1–3 as a separate,
scoped effort only if per-worker density becomes a measured goal.
