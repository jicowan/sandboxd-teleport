# sandboxd — Networking Data Path & Sandbox Lifecycle (design)

Status: Design (2026-07-02), `checkpoint-restore` branch. Guides the next
sandboxd work. Builds on [`TELEPORT-SOLUTION.md`](./TELEPORT-SOLUTION.md)
(teleport proven) and the resolved model in [`ARCHITECTURE.md`](./ARCHITECTURE.md).

## Why (the three review questions)

1. **Traffic to the nested sandbox** — today sandboxes run `--network=none`; there
   is NO data path to the workload. We can only reach sandboxd's API, never the
   process inside.
2. **Health-check + restart** — we can see `runsc state` (liveness) but cannot
   probe the *app* (readiness) without a network path; no restart policy.
3. **Reset / reuse a worker** (substrate-style) — when the sandbox is idle/done,
   free the worker and reuse it for another sandbox. The density model.

Through-line: **(1) networking unblocks (2) readiness/idle, which enables (3)
reset/reuse.** (3)'s "when/which worker" is the control plane (deferred).

---

## Part 1 — Networking data path

### Goal
A client (broker/router, eventually Claude/MCP) can reach a TCP port exposed by
the process inside the nested gVisor sandbox, via the worker pod's routable IP.
Reconnect model (sockets do NOT survive C/R — proven for gVisor & substrate).

### Design (mirrors substrate's veth + stable interior IP)

```
 client ──▶ worker-pod-IP:HOSTPORT ──▶ sandboxd proxy ──▶ 169.254.17.2:APPPORT
 (CNI, routable)                        (in worker netns)    (nested sandbox, interior netns)
```

- **Stop using `--network=none`.** Give the sandbox a network. Two options:
  - **(A) sandbox netstack on a veth pair** into an interior netns with a STABLE
    interior IP (substrate: `169.254.17.2/30`, gateway `.1`). runsc reads the
    addresses/routes off the interior netns links. Survives restore because the
    IP is fixed and re-created fresh each time.
  - **(B) simpler bring-up first:** runsc `--network=sandbox` (default netstack)
    with a host-port mapping, OR a userspace TCP proxy from a worker port to the
    sandbox. Start here to get *a* data path, then move to (A) for stability.
- **sandboxd owns a per-sandbox proxy/port map**: `POST /run` (and `/restore`)
  optionally take `ports: [{container: 8080, host: 0}]`; sandboxd allocates a
  worker-pod port, proxies host:port → sandbox interior IP:container-port, and
  returns the mapping. Recorded in sandbox metadata (so it survives reconcile and
  is re-established on restore).
- **On restore, re-establish the SAME interior IP + proxy** so the address a
  client dials is stable across teleport (only the session reconnects).

### EMPIRICAL FINDING (2026-07-02) — host networking is incompatible with C/R

Tested both halves:
- **`--network=host` gives a working data path** ✅ — with the OCI spec NOT
  declaring a `network` namespace (so the sandbox inherits the worker pod's
  netns), a python `http.server` bound `:8080` inside the sandbox was reachable
  from another pod at `worker-pod-IP:8080` (got the real directory listing).
- **BUT `--network=host` BREAKS checkpoint** ❌:
  `checkpoint not supported when using hostinet`. hostinet uses host sockets the
  sentry can't serialize.

→ **Host networking and teleport are mutually exclusive.** The MVP must use
`--network=sandbox` (gVisor's OWN netstack, which IS checkpointable) and reach
into it another way. Options to reach a `sandbox`-netstack workload:
  (a) **veth pair** worker-netns ↔ sandbox interior netns + stable interior IP
      (substrate's model) — sandboxd routes worker-pod-IP:hostport → interior IP.
  (b) **gVisor port-forwarding**: `runsc port-forward` / a gofer-mediated proxy.
  (c) sandboxd dials the sandbox via the control socket / a userspace hookup.
Decision: pursue (a) — it's the proven-portable substrate design and survives
restore (fresh veth each time, fixed interior IP). Host-net stays available as a
NON-checkpointable "fast path" option (env SANDBOXD_NETWORK=host) for workloads
that don't need teleport.

### SUBSTRATE'S MECHANISM (extracted from source; what we reimplement)

Pure kernel networking — **veth + nftables, NO proxy, NO port-forward**. Survives
C/R because the interior netns is referenced by PATH in the OCI spec (not
`--network=host`), the IP is fixed, and the veth is rebuilt fresh each restore.

- **Interior netns**: `netns.NewNamed("ateom:<podUID>")` (vishvananda/netns) —
  persistent per worker, reused across activations. Bind-mounted at
  `/run/netns/<name>`.
- **veth pair** (vishvananda/netlink): host end `ateom0` stays in the POD netns
  @ `169.254.17.1/30`; peer moved into the interior netns
  (`netlink.LinkSetNsFd`), renamed `eth0`, @ `169.254.17.2/30`, default route via
  `.1`. `/30` point-to-point.
- **OCI spec**: `Linux.Namespaces[] {type: network, path: /run/netns/<name>}` —
  runsc joins that netns (NO `--network` CLI flag; NOT host). gVisor reads eth0's
  addr/routes from the netns.
- **nftables (google/nftables), in the POD netns** — table `ateom_actor`:
  - PREROUTING DNAT: `podIP:PORT` → `169.254.17.2:PORT` (inbound).
  - POSTROUTING MASQUERADE: src `169.254.17.2` → podIP (egress).
  - FORWARD ACCEPT.
- **`/proc/sys/net/ipv4/ip_forward=1`** in the pod netns (route between ateom0 and
  the pod's eth0).
- **Data path**: request → podIP:PORT → DNAT → ateom0 → veth → interior eth0 →
  gVisor injects into guest. No userspace hop.
- **C/R**: network NOT set up during checkpoint; torn down after; **re-setup
  (fresh veth) on restore**; interior netns reused; fixed IP → survives.

### sandboxd implementation plan (mirror the above)

Go deps: `vishvananda/netns`, `vishvananda/netlink`, `google/nftables`.
1. On sandboxd start (or first /run): create the interior netns once
   (`sbx-net`), enable ip_forward in the pod netns.
2. `/run` (+ `ports`): create veth (`sbx0` ↔ peer→`eth0` in interior netns @
   .2/.1), set OCI spec network namespace `path` = the interior netns, install
   nftables DNAT podIP:hostPort→169.254.17.2:containerPort + masquerade +
   forward, runsc run (netns via spec, no --network=host).
3. `/checkpoint`: checkpoint (netstack IS checkpointable now), then tear down the
   veth (keep nftables? — rebuild on restore for cleanliness).
4. `/restore`: re-create veth into the (reused) interior netns, re-install
   nftables, restore. Same interior IP → reachable at the same podIP:hostPort.
5. record port map in metadata; expose in /status + /sandboxes.

### Constraints / notes
- gVisor `--network=none` was used to avoid netns setup during the C/R spikes;
  adding networking must not break checkpoint/restore (netstack IS
  checkpointable; connected sockets are not — that's the reconnect model).
- The worker pod already has a routable CNI IP; we expose sandbox ports *through*
  it (like a NodePort/hostPort but at the pod level).
- Interior IP is link-local and per-netns, so every worker can reuse
  `169.254.17.2` without collision (it's inside each sandbox's own netns).
- MVP scope: **one TCP app port per sandbox** (AIO's `:8080`, a server's port).
  Multi-port later.

### sandboxd changes (Part 1)
- runsc flags: drop `--network=none` for networked sandboxes; set up the
  interior netns + veth (or use netstack + proxy for the MVP).
- `POST /run`/`/restore`: accept `ports`; allocate + record; start a proxy
  goroutine (`net.Listen` on the worker port → `net.Dial` interior:port).
- `GET /status` / `/sandboxes`: include the port mapping + interior IP.

---

## Part 2 — Health check & restart

### Liveness (available now)
- sandboxd already knows `runsc state <id>` (running/stopped/paused). A
  supervisor goroutine polls tracked sandboxes on an interval; on `stopped`
  (unexpected crash) it triggers the restart policy.

### Readiness (needs Part 1)
- Once a sandbox has a reachable port, sandboxd can run a configurable probe
  (TCP connect or HTTP GET path) against the interior IP:port, exactly like a
  k8s readinessProbe. `GET /status` reports `ready: true/false`.
- `POST /run`/`/restore` accept an optional `readiness: {tcp|http, port, path,
  intervalSeconds, failureThreshold}`.

### Restart policy (the C/R superpower)
- On liveness failure, sandboxd's supervisor can:
  - **restore-on-crash**: re-`restore` from the sandbox's last checkpoint (warm,
    resumes state) — unique to this design; OR
  - **cold restart**: re-`run` from the image (fresh).
- Policy per sandbox: `restartPolicy: {none|cold|restore}` (default `none` for
  now; the control plane will usually own restart decisions).
- Bounded retries + backoff; emit metrics (`restarts`, `restart_failures`).

### sandboxd changes (Part 2)
- A `supervisor` goroutine: poll `runsc state` + readiness probe for each tracked
  sandbox; act on restartPolicy.
- Metadata: add `readiness`, `restartPolicy`, `lastProbe`, `restarts`.
- `GET /status`: add `ready`, `restarts`.

---

## Part 3 — Reset / reuse a worker (substrate density)

### The reuse loop
```
run/restore ─▶ [busy: one sandbox] ─idle/done─▶ checkpoint→S3 ─▶ delete+cleanup ─▶ [idle worker]
                                                                                       │
                                                          control plane assigns next ◀─┘
```
- **A worker hosts ONE sandbox at a time** (substrate model; avoids spatial
  oversubscription / scheduler reimplementation — see ARCHITECTURE). Reuse is
  temporal: suspend idle → free worker → restore next.
- **Reset primitive (sandboxd, mostly have it):** `delete` runsc container +
  `cleanupArtifacts` (bundle/img) + `forget` metadata → worker back to a clean
  `-root`. Add an explicit endpoint + a "reset to idle" that optionally
  checkpoints first.

### Idle detection
- **(a) network-activity based** (needs Part 1): sandboxd's proxy tracks
  last-byte time; no traffic for `idleTimeout` → mark idle.
- **(b) app-reported**: the workload calls a sandboxd "still busy"/"done"
  endpoint (later; needs app cooperation).
- MVP: (a) via the proxy, plus a manual `POST /suspend` for the control plane.

### Suspend-on-idle (substrate's whole value)
- `POST /suspend {sandboxId}`: checkpoint → S3 → delete+cleanup → worker idle.
  (Compose of existing `/checkpoint` + `/sandbox` delete.)
- Automatic: supervisor sees idle → same sequence → emit "worker freed" (the
  control plane consumes this to reassign).
- `GET /status`: add `idle: true/false`, `lastActivity`.

### Worker pool state
- sandboxd exposes `GET /capacity` → `{busy: bool, sandboxId?}` so a scheduler
  can find idle workers. (Control plane will track this centrally later; expose
  it now so the pieces compose.)

### sandboxd changes (Part 3)
- `POST /suspend` (checkpoint+free), `POST /reset` (free without checkpoint),
  `GET /capacity`.
- Proxy-based activity tracking + `idleTimeout` in the supervisor.
- Metadata: `lastActivity`, `idle`.

---

## Build order (this session)

1. **Part 1 networking** — MVP data path: sandbox gets a reachable port; sandboxd
   proxies worker-port → sandbox; prove with a server image (nginx/python http)
   that a client can curl through the worker, and that it survives a
   checkpoint/restore teleport (reconnect).
2. **Part 2 sandboxd pieces** — supervisor: liveness poll, readiness probe,
   restart policy (restore-on-crash); `/status` reports ready/restarts.
3. **Part 3 sandboxd pieces** — `/suspend`, `/reset`, `/capacity`, proxy-based
   idle detection.

Control-plane scheduling (which worker, when) stays deferred — sandboxd exposes
the primitives; the controller orchestrates them next phase.

## AIO under sandboxd — status (2026-07-02)

Ran the real AIO image (`ghcr.io/agent-infra/sandbox:latest`, 3.4GB) through
sandboxd `/run` with `network=sandbox` + ports. Results:
- **AIO boots nested under sandboxd** — the earlier `groupadd` bootstrap failure
  is GONE, because sandboxd uses the image's REAL entrypoint (`/opt/gem/run.sh`)
  + full env from the OCI config (vs. the old hand-built spec). Process tree
  shows gem.sh, supervisord, jupyter, code-server, mcp-server-browser, tigervnc,
  nodejs-repl, python-server all RUNNING.
- **BUT `:8080` (nginx front door) never comes up** → not reachable. Root cause
  from `supervisorctl status`: **`websocat: failed to lookup address
  information: Try again`** = DNS resolution failing in the sandbox. Our OCI
  spec has **no `/etc/resolv.conf`** and the interior netns has no resolver, so
  AIO services doing hostname lookups fail (EAI_AGAIN) and nginx stalls waiting
  on upstreams. A few X11 services (openbox, autocutsel) FATAL — non-critical.

**FIX (next):** provide DNS in the sandbox:
1. bind-mount/write `/etc/resolv.conf` into the sandbox (e.g. the pod's resolver
   or a public one) — add to the OCI spec mounts.
2. ensure the veth masquerade allows DNS egress (UDP/TCP 53) — the POSTROUTING
   masquerade should already cover it; verify.
3. possibly set a hostname/`/etc/hosts` entry.
This is a config gap, not a gVisor incompatibility — AIO nearly fully booted.
Big images also make `/run` block minutes on pull (2m44s for AIO 3.4GB) → make
image pull/flatten async or pre-warm the base image on workers.

**UPDATE (added resolv.conf, v20):** sandboxd now writes `/etc/resolv.conf`
(copied from the worker pod's resolver) into the sandbox rootfs on networked
run. AIO still not fully healthy: it boots (`status: running`) but the MCP hub
(`:8080`) never becomes ready, and the container appears to exit/become
inconsistent after a while. Iteration is BLOCKED by ergonomics:
- **Slow pull:** each AIO attempt re-pulls+flattens 3.4GB (~3 min) synchronously
  in `/run`, and the HTTP request blocks past client timeouts. MUST fix before
  productive AIO iteration:
  1. **async `/run`** (return a sandboxId immediately; pull/boot in background;
     poll `/status`), AND
  2. **pre-warm / cache the base image** on the worker (don't re-flatten 3.4GB
     every run) — e.g. keep an unpacked rootfs template per image digest.
- Debug logging (`SANDBOXD_DEBUG=1`) floods stdout, burying sandboxd's own log
  lines — turn off once AIO is stable, or route gVisor logs to a separate sink.

**Next-session plan for AIO:** (a) make `/run` async + cache the base rootfs so
iteration is seconds not minutes; (b) then debug why AIO's `:8080`/services
don't fully come up (DNS now provided — check remaining: does AIO need specific
/dev, cgroup, or larger memory; compare the working containerd-gVisor AIO pod's
spec/mounts to ours); (c) once healthy, checkpoint a "golden" booted AIO and
teleport from it (avoids re-running first-boot).

### Image rootfs cache — DONE ✅ (2026-07-02, v22)

Implemented a disk cache of the FLATTENED rootfs per image digest
(`<work>/imgcache/<digest>/rootfs`, cache.go) — going beyond substrate (which
only caches the pulled tarball in-memory and re-untars per actor; it has a TODO
for exactly this disk cache). Per-sandbox rootfs = a HARDLINK copy (cp -al) from
the cache; safe because runsc `-overlay2=root:self` copy-up keeps writes in the
sandbox's overlay filestore, never touching the shared cached inodes. sandboxd's
own direct writes to the rootfs (e.g. /etc/resolv.conf) do remove-then-create to
break the hardlink and avoid mutating the cache.

Measured (AIO, flattened rootfs = 8.9GB):
- cache MISS (first run): pull+flatten ~3+ min (also added pull RETRY — a 3.4GB
  single-stream fetch hit transient "connection reset by peer").
- cache HIT (2nd run): **~17s** to running (hardlink copy + runsc boot). ~10x.
Now iteration on AIO is fast. Note: `/run` is still synchronous on the cache-MISS
(first pull blocks minutes) — async /run is still a nice-to-have but no longer
blocking day-to-day since the cache is warm after one pull.

## Non-goals (now)
- Control plane / scheduler, multi-port, private-registry auth, API auth
  (deferred), multi-sandbox-per-worker (explicitly avoided).

### AIO MCP server VERIFIED working under sandboxd (2026-07-02)

Full AIO under sandboxd's nested gVisor, reached via worker-pod-IP:8080 through
the veth+nftables data path:
- REST: GET /v1/sandbox -> 200 (full environment payload).
- MCP: POST /mcp `initialize` -> 200, serverInfo "Sandbox MCP Tools" v2.14.7,
  protocol 2024-11-05; `tools/list` -> real tool set (browser_get_info,
  browser_gui_screenshot, browser_gui_execute_action, ...) with JSON schemas.
The `/tmp` tmpfs fix (v23) was the last blocker (python-server crash loop).
Next: teleport AIO (checkpoint mem+fs -> S3 -> restore on another worker) and
verify the MCP hub + a written marker survive.

### TODO (post-teleport): cache on host, not worker fs (user idea, 2026-07-02)
The imgcache currently lives in the worker pod's ephemeral fs (<work>=/work), so
a worker restart/rollout LOSES it -> every deploy re-pulls the 3.4GB base (~3min)
per worker. Move the cache to a **hostPath volume** on the node:
- survives worker pod restarts/rollouts;
- SHARED across all worker pods on the same node (pull once per node, not per pod);
- pairs well with pre-warming the base image at node/pool provisioning.
Keep the per-sandbox rootfs as a hardlink copy from the hostPath cache (hardlinks
work within one filesystem, so cache + bundles must be on the same volume).

### TODO (optimization): incremental / delta checkpoints (user idea, 2026-07-02)
Today each checkpoint captures the FULL memory image (AIO pages.img = 783MB) +
fs. Investigate capturing only DELTAS to shrink the S3 payload + speed
suspend/resume:
- Memory: does runsc support incremental/differential checkpoints (dirty-page
  tracking, base + delta layers)? Check runsc checkpoint flags / gVisor docs
  (e.g. compression exists: --compression=flate-best-speed; look for
  incremental/diff options or resume-from-base).
- Filesystem: with -overlay2=root:self the writable delta is the overlay UPPER
  only — capture/ship just that, not the whole rootfs (base is already node-local
  via imgcache). Substrate's DATA scope = fs-delta only (create
  --fs-restore-image-path) vs FULL = +memory. Consider a "fs-delta-only" scope
  for snapshots that don't need live memory.
Goal: much smaller/faster snapshots (esp. for the golden-checkpoint model where
the base memory image is shared and only per-session deltas travel).

### FEASIBILITY FINDINGS for the two TODOs (investigated 2026-07-02)

**(A) hostPath cache — FEASIBLE, low effort, high value. RECOMMENDED.** Move
imgcache to a hostPath volume shared by all worker pods on the node. Constraint:
cache + per-sandbox bundles must be on the SAME filesystem (hardlink copy needs
it) → mount the hostPath and set SANDBOXD_WORK to it (both imgcache and bundles
live under <work>). Benefits: survives worker rollout (kills the repeated ~3-min
re-pull we hit every deploy), shared per-node (pull once per node). Cross-process
safety with multiple worker pods: the atomic `.tmp`→rename + `.done` marker
already handles it (readers only see committed entries; last writer wins). The
per-digest in-process lock stays as a same-pod optimization.

**(B) incremental/delta MEMORY checkpoints — NOT feasible with runsc.** Confirmed
via runsc flags + substrate source: runsc has NO incremental/differential/dirty-
page memory checkpoint. Substrate also accepts full memory dumps for gVisor (only
its microVM/cloud-hypervisor path does userfaultfd deltas — N/A to us). The only
memory-size levers runsc exposes: `-compression=flate-best-speed` and
`-exclude-committed-zero-pages`. MEASURED (python workload, 17.5MB pages.img):
- plain: 17.5MB / 110ms
- exclude-zero-pages: 17.5MB (~0%; this workload had few zero pages) / 64ms
- **compression: 2.7MB (~6.5x smaller) / 141ms**
- both: 2.7MB / 128ms
→ **compression is a big, cheap win (~6.5x here, +~30ms)**; for AIO's 783MB
pages.img it could cut the S3 payload to ~150-250MB. Zero-page exclusion is
workload-dependent. CAVEAT: `restore -background` says "requires uncompressed
checkpoint" — compression likely forces foreground/eager page-in, trading a
smaller+faster S3 transfer for slower local restore. Since the S3 leg dominates
for AIO, net likely positive — add as SANDBOXD_COMPRESS and A/B the end-to-end
(S3 up + down + restore) time.

**Filesystem delta — ALREADY LEAN here.** Substrate's DATA scope fscheckpoints
only DurableDir VOLUMES and rebuilds rootfs from the image. In our model
`-overlay2=root:self` keeps the writable delta in the overlay UPPER, and we
already ship ONLY the checkpoint (the 8.9GB base rootfs never travels — it comes
from imgcache). So the fs story is done; the memory image is what travels. The
"golden checkpoint" (shared base, per-session divergence) is the remaining dedup
lever, orthogonal (planned for AIO).

**Recommendation:** implement (A) hostPath cache (clear win, low risk); add
optional checkpoint compression to shrink the S3 payload. Drop memory-delta
(infeasible with runsc).

### SUPERSEDED by: reuse the NODE containerd image cache (decided 2026-07-02)

Better than a hostPath-flatten cache: sandboxd currently pulls via
go-containerregistry and FLATTENS an 8.9GB rootfs into its own imgcache — a
second copy of what the node's containerd ALREADY has. Node inspection: the
containerd overlay snapshot store is 12G of unpacked, deduped, node-shared
layers; socket at /run/containerd/containerd.sock.

Decision: **mount the containerd socket into the worker and use the containerd Go
client** to Pull+Unpack (node-level, shared across all worker pods, survives
worker restart natively) and `snapshotter.Prepare` a per-sandbox snapshot mounted
as the bundle rootfs. runsc `-overlay2=root:self` layers on top (writes go to
runsc's filestore; shared lower snapshot stays clean). This is the actual "use
the k8s node image cache" — no parallel flatten, no 8.9GB walk/hardlink, and
containerd handles pull retry/resume/auth/dedup.

Compression: verified compressed checkpoint RESTORES fine (busybox: 108K image,
restore rc=0, counter continued) — it only forgoes `-background` (eager page-in),
which we don't use. So compression is safe to add (SANDBOXD_COMPRESS), separate
from the cache work.

Tradeoffs accepted: mounting the containerd socket widens the (already-privileged,
DinD) worker's blast radius; couples to the node's containerd/snapshotter
(overlayfs); early spikes saw occasional ctr mount-lease flakiness — use the Go
client's lease API to avoid. Replaces cache.go's go-containerregistry+flatten path.

### AIO TELEPORT end-to-end: PROVEN ✅✅✅ (2026-07-02)

Full AIO sandbox teleported between workers via S3, functional after restore:
1. AIO running on worker A (nested gVisor, network=sandbox, MCP hub on :8080).
2. Wrote marker `/home/gem/marker.txt = TELEPORT-MARKER-42` via AIO's shell tool.
3. `/checkpoint` -> S3: 790MB (pages.img 783MB live RAM: Chrome/Jupyter/MCP hub +
   checkpoint.img 6.5MB + config.json). Worker A freed.
4. `/restore` on worker B (different pod) with ports -> status running.
5. Verified on B's pod IP: MCP `initialize` -> serverInfo "Sandbox MCP Tools"
   v2.14.7 (MEMORY state resumed); marker.txt -> TELEPORT-MARKER-42 (FS state
   survived). Reachable via veth+nftables at B's pod IP.

Fixes that made it work this session: writable /tmp tmpfs (python-server crash),
DNS resolv.conf, S3 ops on a BACKGROUND context (opCtx) so a client timeout
doesn't cancel a 790MB up/download mid-flight, image rootfs cache (~10x faster
runs). The substrate-style veth networking survives the teleport (fresh veth +
fixed interior IP on restore).

This realizes the whole thesis: an arbitrary heavy image (AIO: Chrome + full MCP
hub) runs as a nested gVisor sandbox, is reachable over the network, and
teleports (RAM+FS) between workers via S3 — driven entirely by the sandboxd API.

### NODE containerd image cache — IMPLEMENTED ✅ (2026-07-02, v27)

Replaced go-containerregistry pull + 8.9GB flatten + per-pod imgcache with the
node's containerd image cache (containerd.go). sandboxd (containerd Go client
over the mounted socket) Pull+Unpack into the shared k8s.io overlayfs store, then
Prepare a per-sandbox snapshot (key `sandboxd-<id>`) and mount it as the bundle
rootfs; runsc -overlay2=root:self layers on top. Dropped the go-containerregistry
dep entirely (removed cache.go; trimmed bundle.go/spec.go).

Deploy: mount /run/containerd/containerd.sock + /var/lib/containerd (Bidirectional)
into the worker.

Fixes found: (1) the snapshot mount must be made **rshared** (MS_SHARED|MS_REC)
so it propagates into runsc's gofer mount namespace — else "filestore file ...
no such file or directory". (2) empty-rootfs guard catches not-yet-unpacked
images. (3) earlier AIO failures were a stale/partial unpack from before this
change; a clean WithPullUnpack fixed it.

MEASURED: AIO run **~1.5s** (pull+flatten 565ms) vs **~3min** with the old
flatten path — and NO 8.9GB duplicate copy, shared per-node, survives worker
restart (it's containerd's store). busybox + AIO both run AND checkpoint fine
(containerd-snapshot rootfs + overlay2=root:self are compatible). AIO healthy:
supervisord stable, MCP hub `Sandbox MCP Tools v2.14.7` reachable via pod IP.

This is the "use the k8s node image cache" answer — supersedes the hostPath cache.

### Uncached images + fresh-unpack race (2026-07-02, v28)

Q: "what if a worker wants an image not cached on the node?" → containerd Pulls
it from the registry (WithPullUnpack), so uncached images WORK (nginx/httpd
pulled in ~2s). One wrinkle found: the FIRST run of a JUST-pulled image could hit
"filestore file ... no such file or directory" (the snapshot/overlay briefly
presents an incomplete tree right after unpack); a retry always succeeded. Fixed:
prepareRootfsContainerd now retries Prepare+mount (up to 5x, backoff) until the
rootfs is populated. Verified: a fresh httpd:2.4-alpine succeeds on the FIRST
attempt now. (This was also the true cause of the earlier "AIO fails first time"
confusion.)

Compression: verified restorable but NOT yet wired into sandboxd checkpoints
(checkpoints are uncompressed). Still an optional TODO (SANDBOXD_COMPRESS).

### COMPRESSION A/B — verdict: DEFAULT ON for suspend (2026-07-02, v29-v33)

Wired compression into checkpoint (SANDBOXD_COMPRESS env default + per-request
`compress`): `-compression=flate-best-speed -exclude-committed-zero-pages`.
Restore is unaffected (we use `-detach`, not `-background`, so compressed images
restore fine — verified earlier with busybox).

A/B on the SAME AIO sandbox (suspend side, conclusive):
| metric | uncompressed | compressed |
| checkpoint image | 739 MB | **172 MB (4.3x smaller)** |
| runsc checkpoint | 1.5 s | 2.8 s (+1.3s) |
| S3 upload | 6.7 s | **1.6 s (-5.1s)** |
| **checkpoint+upload** | **8.2 s** | **4.4 s (~1.9x faster)** |
Compressed S3 DOWNLOAD also ~4x faster (1.9s vs 7.7s for the 172MB vs 739MB).

Verdict: **compression should be DEFAULT ON.** The +1.3s checkpoint cost is
dwarfed by the S3 up/down savings, and S3 transfer dominates real teleport time.
The one theoretical cost — eager (non-`-background`) page-load on restore — we
never used `-background` anyway. (Set SANDBOXD_COMPRESS=1 to default on;
per-request `compress:false` to override.)

### ⚠️ TOP KNOWN ISSUE: containerd-snapshot rootfs mount race (blocks reliable run/restore)

Since moving to the node-containerd rootfs (v27+), `runsc run`/`restore`
INTERMITTENTLY fails: `creating gofer filestore files: failed to create filestore
file inside "…/rootfs": no such file or directory`. Frequency varies (sometimes
1st-try OK, sometimes several fails in a row); a fresh worker is worse. This
BLOCKED the restore-side compression timing today (download works — 1.9s — but
runsc restore fails the race).

Root-cause hypothesis: we `snapshotter.Prepare` a **read-write** containerd
snapshot and mount it as the rootfs, then runsc adds ANOTHER overlay via
`-overlay2=root:self` and writes `.gvisor.filestore` INTO that rootfs. The
containerd rw-overlay + runsc overlay + the gofer's `--setup-root` (new mount ns,
pivot) interact badly / with a propagation-timing race. MS_SHARED remount +
writability probe + prepare-retry REDUCED but did NOT eliminate it; a
runsc-run-level retry did NOT help (re-run against the same mount still fails).

Candidate fixes (next session, pick one):
1. Use a **`View` (read-only)** containerd snapshot as the rootfs LOWER and let
   runsc's `-overlay2=root:self` own the writable layer (avoids stacking two
   writable overlays — most promising).
2. Pass the containerd mounts to runsc/OCI as the root mount spec (let runsc do
   the mounting in its own ns) instead of pre-mounting in sandboxd's ns.
3. Fall back to the (proven-stable) flatten-to-dir rootfs for correctness and
   keep containerd only as the pull/cache source.
This is the #1 thing to fix — it gates reliable run and teleport.

### RESEARCH → ROOT CAUSE + FIX for the filestore race (2026-07-02)

Researched web + gVisor source (runsc/container, runsc/boot, runsc/cmd/gofer,
sandboxsetup) + the containerd runsc shim + substrate. Findings converged:

**Root cause.** With `--overlay2=root:self`, runsc writes the overlay upper's
backing file `.gvisor.filestore.<sandboxID>` INSIDE `spec.Root.Path` (the
rootfs). Critically it is created by the **runsc PARENT process reaching into the
GOFER's mount namespace** at `/proc/<goferPid>/root/<rootfs>/.gvisor.filestore…`
(gVisor PR #11304 / issue #9834 — done so filestore FDs don't pin host mounts).
The gofer's mount namespace is a **COPY taken at clone() time**, root mounted
`MS_SLAVE`. So our containerd overlay snapshot (mounted in sandboxd's ns) is only
visible in the gofer's copy if it was mounted BEFORE the gofer forked AND on a
SHARED (propagating) mount. Ours raced the fork / wasn't propagating → the
`open(O_CREAT)` parent dir is missing → ENOENT (worst right after a fresh unpack).

**Fix (option c — confirmed best): move the filestore OUT of the rootfs** via
`--overlay2=root:dir=/abs/path`. runsc writes the overlay upper to a plain host
dir we control, never into the containerd overlay, so the filestore open no
longer depends on the snapshot being propagated into the gofer ns at the exact
instant. Syntax (runsc/config): `--overlay2={root|all}:{memory|self|dir=/abs}[,size=N]`;
`dir=` must be absolute. `--directfs`/`--root` are unrelated to the filestore.

**Also keep mount hardening:** carrying mount rshared (MS_SHARED|MS_REC) +
rootfs-populated probe before exec — fixes propagation/ordering; `dir=` removes
the write-into-overlay coupling. Both together = robust.

**Bonus:** overlay-on-overlay is NORMAL (standard containerd+runsc runs `self`
on the overlayfs `merged` snapshot, issue #12476) — so the containerd-snapshot
rootfs is the RIGHT approach; we just used the wrong medium.

Sources: runsc/container/container.go (createGoferFilestores /
createGoferFilestoreInSelf), runsc/boot/vfs.go (SelfFilestorePath),
runsc/cmd/sandboxsetup/gofer_mount.go (MS_SLAVE), PR #11304, issues #9834/#12476;
runsc/config (overlay2 dir=); containerd runtime-v2 shim pre-mount.

**IMPLEMENTED: `--overlay2=root:dir=<work>/overlays/<id>` per sandbox.**

### FIX VERIFIED ✅ (2026-07-02, v34): --overlay2=root:dir=<work>/overlays/<id>

Implemented option (c). runsc's `base()` now emits
`-overlay2=root:dir=<work>/overlays/<id>` (per-sandbox, threaded through all
per-container ops: run/checkpoint/restore/state/delete so it's consistent); the
dir is created before use and removed on delete. Kept MS_SHARED + the writability
probe.

Results:
- **Reliability: 6/6 fresh first-attempt runs succeeded** (was intermittent
  ~1-2/4 with root:self). Restore also succeeded on the FIRST attempt.
- **Correctness: checkpoint/restore state continuity intact** — busybox counter
  checkpointed on A, restored on B, /state.log continued (c=8→…→13). The overlay
  upper living in the external dir= is captured+shipped+restored correctly.

The intermittent gofer-filestore race is RESOLVED. This was the #1 known issue;
run + teleport are now reliable.
