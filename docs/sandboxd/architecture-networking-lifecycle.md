# sandboxd — networking data path & sandbox lifecycle

How a nested gVisor sandbox on a worker gets a **network data path** (so a client
can reach the workload inside it), how the worker manages a sandbox's **lifecycle**
(readiness, restart, idle-suspend, reset), and why that model is **checkpoint/restore
(teleport) safe**. This is the detailed companion to
[architecture-sandboxd.md](architecture-sandboxd.md) (the worker + control plane at
a product level).

The design record of *how* this was arrived at — the AIO bring-up, the gofer
filestore-mount race and its fix, compression measurements, the containerd-cache
decision — lives in
[history-worker-networking-and-teleport.md](../../checkpoint-restore/history-worker-networking-and-teleport.md).
This document describes only the current design.

## The three concerns, and how they chain

1. **A data path to the workload.** A sandbox needs a reachable TCP port so a
   client (the router, ultimately an MCP client) can talk to the process inside —
   not just to the worker's own API.
2. **Lifecycle.** With a reachable port the worker can probe *readiness* (not just
   liveness), apply a restart policy, and detect idleness.
3. **Reset / reuse.** An idle sandbox suspends to S3 and frees its worker, which is
   then reused for another sandbox.

Networking (1) unblocks readiness/idle (2), which enables reset/reuse (3). Which
worker runs what, and when, is the control plane's job (see
[architecture-sandboxd.md](architecture-sandboxd.md)); the worker exposes the
primitives below.

---

## Part 1 — Networking data path

### Goal

A client reaches a TCP port exposed by the process inside the nested gVisor
sandbox, via the worker pod's routable CNI IP. Connections are **reconnect-on-
restore**: sockets do not survive checkpoint/restore, but the *address* is stable,
so a client reconnects to the same endpoint after a teleport.

```
 client ──▶ worker-pod-IP:hostPort ──▶ [nftables DNAT] ──▶ 169.254.17.2:appPort
 (routable CNI IP)                     (in the worker netns)   (nested sandbox, interior netns)
```

### Why not host networking

The sandbox uses gVisor's **own netstack** (`--network=sandbox`), not
`--network=host`. Host networking (`hostinet`) is not checkpointable — the sentry
cannot serialize host sockets (`checkpoint not supported when using hostinet`), so
host networking and teleport are mutually exclusive. gVisor's netstack **is**
checkpointable, so the worker reaches into it with kernel networking instead.

(A non-checkpointable host-networking "fast path" can be enabled per workload via
`SANDBOXD_NETWORK=host` for sandboxes that don't need teleport.)

### Mechanism — veth + nftables, no userspace proxy

Pure kernel networking. Survives checkpoint/restore because the interior netns is
referenced by **path** in the OCI spec (not a `--network` CLI flag), the interior
IP is fixed, and the veth is rebuilt fresh on each restore.

- **Interior netns** — a persistent named netns per worker, reused across sandbox
  activations, bind-mounted under `/run/netns/`.
- **veth pair** — the host end stays in the worker pod's netns at
  `169.254.17.1/30`; the peer is moved into the interior netns, renamed `eth0`, set
  to `169.254.17.2/30` with a default route via `.1`. A `/30` point-to-point link.
- **OCI spec** — declares the network namespace by path
  (`Linux.Namespaces[] {type: network, path: /run/netns/<name>}`); runsc joins that
  netns and reads `eth0`'s address/routes from it. No `--network=host`.
- **nftables** (in the worker pod netns):
  - PREROUTING **DNAT**: `podIP:hostPort → 169.254.17.2:appPort` (inbound).
  - POSTROUTING **MASQUERADE**: source `169.254.17.2 → podIP` (egress, incl. DNS).
  - FORWARD **ACCEPT**, and `net.ipv4.ip_forward=1` in the pod netns.
- **Data path** — request → `podIP:hostPort` → DNAT → veth → interior `eth0` →
  gVisor injects into the guest. No userspace hop.

Because the interior IP is link-local and per-netns, every worker reuses
`169.254.17.2` with no collision — it lives inside each sandbox's own netns.

### Port mapping

`POST /run` and `POST /restore` accept `ports: [{container, host}]`. The worker
installs the DNAT for each and records the mapping in sandbox metadata, so it
survives reconcile and is re-established on restore. On restore, the caller (the
control plane) supplies the ports again — a fresh worker holds no prior metadata.
`GET /status` and `GET /sandboxes` report the port mapping and interior IP.

### DNS inside the sandbox

The worker writes `/etc/resolv.conf` (copied from the worker pod's resolver)
directly into the sandbox rootfs on run **and** on restore, so workloads that do
hostname lookups resolve. (It is written into the writable rootfs rather than
bind-mounted, because a bind mount requires the target file to pre-exist in the
rootfs, which fails on restore.)

### Checkpoint/restore behavior

Networking is **not** set up during checkpoint and is torn down afterward; it is
**re-established fresh** on restore into the reused interior netns with the same
fixed interior IP. gVisor's netstack is captured in the checkpoint; connected
sockets are not — hence the reconnect model. The address a client dials is stable
across a teleport.

---

## Part 2 — Health check & restart

### Liveness

The worker knows each sandbox's `runsc state` (running / stopped / paused). A
supervisor goroutine polls tracked sandboxes on an interval; an unexpected
`stopped` triggers the restart policy.

### Readiness

Because a sandbox has a reachable port, the worker runs a configurable probe (TCP
connect or HTTP GET) against the interior IP:port, like a Kubernetes readiness
probe. `POST /run` / `/restore` accept an optional `readiness: {tcp|http, port,
path, intervalSeconds, failureThreshold}`; `GET /status` reports `ready`.

### Restart policy

On liveness failure the supervisor can, per the sandbox's `restartPolicy`:

- **`restore`** — restore from the sandbox's last checkpoint (warm; resumes state).
  This is unique to the checkpoint/restore design.
- **`cold`** — re-run from the image (fresh).
- **`none`** — take no action (the control plane usually owns restart decisions).

Retries are bounded with backoff; the worker emits `restarts` / `restart_failures`
metrics. `GET /status` reports `restarts`.

---

## Part 3 — Reset / reuse a worker

```
run/restore ─▶ [busy: one sandbox] ─idle/done─▶ checkpoint→S3 ─▶ delete+cleanup ─▶ [idle worker]
                                                                                       │
                                                          control plane assigns next ◀─┘
```

- **One sandbox per worker at a time.** Reuse is *temporal*, not spatial: suspend
  the idle sandbox, free the worker, restore the next one. This avoids spatial
  oversubscription and reimplementing a scheduler. (Consequently the interior IP,
  nftables table, and teardown are worker-global — no per-sandbox network isolation
  is needed within a worker.)
- **Suspend-on-idle** — `POST /suspend {sandboxId}` checkpoints to S3, then
  deletes and cleans up, leaving the worker idle. The supervisor does the same
  automatically when a sandbox goes idle, and signals that the worker is free so
  the control plane can reassign it.
- **Reset** — `POST /reset` frees a worker *without* checkpointing: delete the
  runsc container, clean up its bundle/overlay artifacts, and forget its metadata,
  returning the worker to a clean state.
- **Idle detection** — activity-based (no traffic on the data path for
  `idleTimeout`), plus the explicit `POST /suspend` for control-plane-driven
  suspend.
- **Capacity** — `GET /capacity` reports `{busy, sandboxId?}` so a scheduler can
  find idle workers.

---

## Rootfs & image cache (why the data path is reliable)

The worker mounts the sandbox rootfs from the **node's containerd image store**
(via the containerd socket): it pulls+unpacks into the shared overlayfs store, then
prepares a per-sandbox snapshot as the bundle rootfs. This reuses the node's
existing, deduped, node-shared image layers (surviving worker pod restarts) instead
of maintaining a second flattened copy, and lets containerd handle pull
retry/resume/auth. runsc then layers its own overlay on top.

Two details that make run/restore reliable:

- **The overlay upper lives outside the rootfs** — runsc is configured with
  `--overlay2=root:dir=<work>/overlays/<id>` (a plain per-sandbox host dir) rather
  than `root:self`. Writing the overlay filestore into the containerd snapshot
  raced the gofer's mount-namespace setup and intermittently failed with a missing
  filestore file; a dedicated `dir=` removes that coupling.
- **Mount propagation** — the snapshot mount is made rshared (`MS_SHARED|MS_REC`)
  and probed for writability before exec, so it propagates into runsc's gofer mount
  namespace.

Checkpoints are **compressed** by default (`-compression=flate-best-speed
-exclude-committed-zero-pages`): the S3 transfer dominates real teleport time, and
compression shrinks the checkpoint image several-fold, making suspend/resume
markedly faster overall (restore is unaffected — the worker uses `-detach`, not
`-background`). Set `SANDBOXD_COMPRESS=0` (or per-request `compress:false`) to opt
out.

---

## Worker API surface for this model

| Endpoint | Role in the model |
|---|---|
| `POST /run` | Start a sandbox; accepts `ports`, `readiness`, `restartPolicy`. |
| `POST /restore` | Restore from an S3 checkpoint; caller re-supplies `ports`. |
| `POST /checkpoint` | Checkpoint to S3, leave the sandbox running. |
| `POST /suspend` | Checkpoint to S3, then free the worker. |
| `POST /reset` | Free the worker without checkpointing. |
| `GET /status` | `ready`, `idle`, `restarts`, port mapping, interior IP. |
| `GET /sandboxes` | All tracked sandboxes + their port mappings. |
| `GET /capacity` | `{busy, sandboxId?}` for scheduling. |

See [api-reference-sandboxd-worker.md](api-reference-sandboxd-worker.md) for the
full request/response schemas.
