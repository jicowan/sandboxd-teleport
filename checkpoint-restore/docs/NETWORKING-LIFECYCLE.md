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
