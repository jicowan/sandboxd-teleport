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

## Non-goals (now)
- Control plane / scheduler, multi-port, private-registry auth, API auth
  (deferred), multi-sandbox-per-worker (explicitly avoided).
