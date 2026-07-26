# Engineering history — worker networking & teleport bring-up

> **Historical record, not current documentation.** This is the dated engineering
> log from the sandboxd worker bring-up (2026-07): the problems hit while getting a
> real workload (the AIO all-in-one image) to boot, network, checkpoint, and
> teleport as a nested gVisor sandbox — and how each was resolved. It is kept for
> the rationale behind decisions that are now simply *the architecture*.
>
> For how the worker works **today**, read
> [architecture-networking-lifecycle.md](../docs/sandboxd/architecture-networking-lifecycle.md)
> (the networking data path + sandbox lifecycle) and
> [architecture-sandboxd.md](../docs/sandboxd/architecture-sandboxd.md) (the worker
> + control plane).
> Version numbers (vNN) and dates below are point-in-time and will not track code.

---

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

### FULL E2E TELEPORT (fresh pull → mutate → checkpoint → restore on another worker): PASS ✅✅✅ (2026-07-02, v36)

Definitive end-to-end test with a stateful server proving RAM+FS+process identity:
1. RUN node:20-slim — FIRST-TIME PULL via node containerd — succeeded FIRST attempt.
2. Stateful server (RAM counter + /data/counter file, boot id). Mutated over the
   network (9 HTTP POSTs to worker A pod IP) → ram=9, file=9, boot=T.
3. CHECKPOINT (compressed) → S3 (~4MB); FREED worker A (state only in S3).
4. RESTORE on worker B (different pod) — FIRST attempt.
5. Queried B: **ram_counter=9, file_counter=9, boot=T (IDENTICAL boot id)** — same
   PROCESS resumed, not a fresh boot; a further POST advanced 9→10 cleanly.

Validated the whole session's work at once: node-containerd image cache (fresh
pull), --overlay2=root:dir= fix (reliable first-attempt run AND restore, filestore
race gone), DNS-into-rootfs, compression, veth/nftables data path surviving teleport.

### DNS fix correction (v36): write resolv.conf INTO rootfs, not OCI bind mount
The v35 OCI-bind-mount approach failed on restore: runsc requires a bind mount's
TARGET file to pre-exist in the rootfs, but /etc/resolv.conf doesn't → "open
.../rootfs/etc/resolv.conf: no such file or directory". Fix: write /etc/resolv.conf
directly into the (writable containerd-overlay) rootfs on run AND restore. /etc
exists in real images; the rootfs is writable. Simple + robust.

### Confirmed design invariants (user, 2026-07-02)
- ONE sandbox per worker → worker-global nftables/interior-IP/teardown are correct;
  no per-sandbox network isolation needed within a worker.
- Different sandboxes expose DIFFERENT ports → handled: each /run|/restore carries
  its own `ports`, DNAT built per-sandbox; persisted in metadata. On restore the
  CALLER (control plane) passes the ports (a fresh worker has no prior metadata).

### VARIED E2E TELEPORT TESTS: all PASS ✅ (2026-07-02, v37, compression default ON)

Three diverse workloads, each: run on worker A (fresh pull where noted) → mutate
→ checkpoint(compressed)→S3 → free A → restore on worker B → verify.

1. **redis:7-alpine** (fresh pull; real server on :6379; `--save ""` so state is
   PURE IN-RAM keyspace). SET name=sandboxd, count→44 over the network on A;
   after teleport, worker B returned name=sandboxd count=44, incr→45. In-RAM
   server state teleported.
2. **caddy:2-alpine** (fresh pull; Go web server on :80). Mutated the served file
   /srv/state.txt → "v1-teleported" on A; after teleport, worker B SERVES
   "v1-teleported". Running web server + fs state teleported.
3. **python:3.11-alpine, pure-RAM** (in-memory list, NO disk writes at all).
   Added 4 items on A; after teleport, worker B returned the same 4 items +
   IDENTICAL boot id. Cleanest proof of live-memory teleport (no fs involved).

Plus the earlier node:20-slim RAM+FS test. All served traffic on a port via the
veth/nftables path and resumed correctly on a different worker.

Compression is now DEFAULT ON (v37; SANDBOXD_COMPRESS=0 to opt out).

Known intermittency still present: the fresh-image first-run race (filestore/
mount) occasionally needs 1-2 retries on a COLD worker (seen on redis 1st-try OK,
caddy 2nd try, python-ram 3rd try). Once the image is warm on the node it's
first-try. Retry-on-run/restore or a stronger settle would remove the retries.

### On "hostPath / persistent cache" (explained) — NOT needed
Concern was: sandboxd's /work is the pod's EPHEMERAL fs, so a worker restart
wiped the pulled/flattened image → re-pull. RESOLVED by reusing the node
containerd store (containerd.go): image layers live in /var/lib/containerd on the
NODE — shared across all worker pods + survive pod restarts already. A fresh
worker does a fast snapshot Prepare, no re-pull (proven by the fresh-worker E2E
tests). Only per-sandbox bundles/overlays under /work stay ephemeral, which is
correct (transient; state goes to S3). So no hostPath cache is needed.

---

## ForkSet fan-out bugs surfaced by the RL example (2026-07-24, operator v39→v42)

Building the RL parallel-rollout example (`examples/forkset/`) and running it live
surfaced four control-plane bugs that only manifest under a **large concurrent
ForkSet fan-out** — many sessions claiming/releasing workers on one pool at once,
plus the pool autoscaling underneath. All four are fixed with regression tests; the
example was reworked to `activation: Lazy` + checkpoint-when-done (temporal
oversubscription), which both fits real RL usage and avoids the burst that made #1/#4
fire. Symptom first seen: an eager fan-out of 8 snapshot forks wedged at "6/8 Running"
and, after deleting the ForkSets, 14 workers stayed `busy` forever and the pool never
scaled down.

**#1 — Worker double-booking (lost update between two writers of worker state).**
The `SPOP`-based claim (`assign.go` `claimWorkerScript`) is atomic and correct in
isolation, so two claims can never pop the same idle member. The bug was a *second,
un-serialized* writer: worker-discovery (`worker_discovery.go`) re-registers a pod as
idle on every pod event (readiness flap, `pod-deletion-cost` patch from the fan-out's
own minIdle autoscaling, informer resync), via `UpsertWorker` →
`upsertWorkerScript`, which did a **blind `SET` + `SADD`** with no CAS. When such an
event fired on a pod in the window between its atomic claim (`SET busy`) and the
session starting on it, discovery overwrote `busy`→`idle` and **re-added the pod to
the idle set** — then a second fork's `SPOP` legitimately claimed the same worker, and
the second wedged in `Resuming` (one-sandbox-per-worker). *Live evidence:* 8 forks
landed on 6 distinct workers. **Fix:** `upsertWorkerScript` now refuses to demote a
`busy` worker to idle (idle-registration is a no-op on state when the stored entry is
already busy). Legit busy→idle stays on the `releaseWorkerScript` / suspend paths.
Regression: `TestDiscoveryUpsertCannotResurrectBusyWorker`.

**#2 — CR deletion strands the worker binding (no finalizer).** Deleting a `Session`
CR — directly, via ForkSet ownerRef cascade, or ForkSet scale-in — was a **silent
no-op against Valkey**: `SessionReconciler` had no finalizer, so nothing ran
`DeleteSession` + `ReleaseWorker`. The `session:<sid>` entry stayed `Running` and
`worker:<pod>` stayed `busy`, so workers never freed and the pool never scaled down.
The self-healing sweeps couldn't recover it because they all key on the *KV entry*
(which the delete leaves intact): GC-abandoned is suppressed by `workerHolds` (live
busy worker) + fresh `lastActiveAt`; GC-orphan-CR fires only when the KV entry is
*absent* (opposite direction); worker-reclaim classifies a Running-bound-here entry
as healthy (its "orphan" reason keys on a missing KV entry, not a missing CR). **Fix:**
a Session finalizer (`session_controller.go`) calls `Suspender.ReleaseForDelete`
(`suspend.go`) on delete — release the worker to idle + delete the KV entry (reset
semantics, no checkpoint: a deleted session is discarded). Regressions:
`releases the KV footprint on delete via the finalizer`, `keeps the finalizer (retries)
if KV release fails`.

**#3 — Per-fork idle policy ignored (worker never freed on the ForkSet's terms).** A
ForkSet sets `lifecycle.idleAction` + `idleTimeoutSeconds` on each child Session, but
the operator dropped both: the idle sweeper's `policyFor` (`resume_glue.go`) honored
only the timeout, never `SessionLifecycle.IdleAction`; and cold-start
(`resume.go`) wrote the *template's* idle timeout into the KV entry, which is what
seeds the `suspend:due` deadline that schedules the O(due) sweep. So a fork asking for
`reset@60s` inherited the template's `suspend@300s` and its worker didn't free when
expected. **Fix:** `applyLifecycleOverride` (`resume_glue.go`) overrides both action
and timeout from `spec.lifecycle`; `SessionPlan.IdleTimeoutOverride` threads the
per-session timeout into the KV entry on cold-start. Regressions: the
`applyLifecycleOverride` specs. *Live evidence after fix:* KV shows
`idleTimeoutSeconds:60`; idle-reset freed all 4 workers at ~60s.

**#4 — Phantom idle-set member → negative `busy` count.** `releaseWorkerScript`
(rollback of a failed claim) re-added a worker to `pool:<pool>:idle` **even when its
`worker:<pod>` entry no longer existed** (the `if cur then` guarded the `SET` but not
the `SADD`). A failed fork materialization racing pool scale-in could thus leave a pod
in the idle set with no backing entry — invisible to `PruneStaleWorkers` (which scans
`worker:*` keys), claimable by `SPOP` (a dead pod), and skewing `CountWorkers`
(`busy = SCARD(all) − SCARD(idle)`) **negative**. *Live evidence:* `pool:idle` had 5
members, `pool:all` had 4 → `busy=-1`. **Fix:** `releaseWorkerScript` only re-adds to
the idle set when the worker entry exists; otherwise it `SREM`s any stale membership.
Regression: `TestReleaseDoesNotResurrectDeletedWorker`.

**Through-line:** #1 and #4 are the same class — worker-state writes (unlike session
writes) were **not** CAS/existence-guarded, so a concurrent or out-of-order writer
could resurrect a claimed/deleted worker into the idle set. #2 and #3 are missing
delete-time and per-session-override plumbing that only a fan-out (many short-lived,
declaratively-managed sessions) exercises hard. Fixed in operator **v42**; verified
live end-to-end (app fork + snapshot fork, distinct workers, idle-free, clean delete).

## Robustness issues surfaced during the memory-reserve battery (2026-07-26, operator v50→v53, worker v66)

Running the full teleport/OOM/forkset battery for the worker-memory-reserve feature
(agent OOM-protection) surfaced several control-plane robustness gaps. The ones fixed on
the `feat/robustness-followups` branch are below; genuinely-out-of-scope items are logged
as follow-ups at the end. NOTE: much of the *noise* during this battery was environmental
— a transient **SPIRE-agent disruption** (~21:45) on one node crashed the v66 workers at
startup (`mTLS init: X509Source ... context deadline exceeded`, exit 1), and a restarted
worker loses its `/work` scratch + in-flight state, cascading into empty-S3 suspends,
`checkpoint.img: no such file` restore failures, orphaned worker bindings, and intermittent
Valkey/CoreDNS timeouts. Those were NOT code bugs; they made the real bugs harder to see.

**#1 — Split-brain: KV wedged `Resuming` when a cold-start outran the resume deadline.**
`startAndBind` (`resume.go`) sets the KV entry `Resuming`, calls the worker `/run` (or
`/restore`), waits `WaitReady`, then sets `Running`. If the cold-start (image pull + boot;
the AIO image is ~2m36s vs the 90s default `--resume-deadline-seconds`) outran the
deadline, the operator's context expired and it returned 502 **without** writing
`Running` — but the worker kept going and the sandbox actually came up. The KV entry then
sat `Resuming` forever: every `/_warm` 502'd though the workload was up, and — worse — a
subsequent on-demand suspend (`SuspendNow`) is idempotent-by-state and used to treat *any*
non-`Running` state as "already satisfied", returning nil so `SessionReconciler` advanced
`status.lastSuspendHandled` — **falsely reporting "suspend completed"** for a session that
was never checkpointed. **Fixes:** (a) `SuspendNow` now returns `ErrSuspendTransient` for
transitional states (`Resuming`/`Suspending`); the reconciler treats it as `SuspendPending`
→ requeue **without** advancing the watermark, so it never lies (issue #1b). (b) A new
`ResumingHealer` sweeper (`selfheal.go`, `ResumingHealSweeper` in `resume_glue.go`, wired
in `cmd/main.go`, grace = 2×resume deadline) reconciles a stuck‑`Resuming` entry against
the worker's real `/status`: adopt→`Running` if the sandbox is actually running, else (see
#2) roll back. Regressions: `TestSuspendNowTransientOnResuming`, `...SatisfiedStates`,
`TestHealerPromotesRunningSandbox`, `...IgnoresNonResuming`. *Live evidence:* forced a
>90s AIO cold-start, watched the entry wedge `Resuming` while the sandbox ran, then the
healer adopt it to `Running` and the withheld suspend complete honestly.

**#2 — Failed restore left the entry stuck `Resuming` → snapshot-fork re-resume broke.**
The nastier consequence of #1's state machine. When a *restore* (`resumeFromSnapshot`)
failed (a transient docker.io image‑tag resolve / S3 download hiccup), the error path
released the worker but **never rolled the entry back** — it stayed `Resuming` with its
`snapshotURI` intact. The next resume checks `State==Suspended && SnapshotURI!=""` for the
restore branch; seeing `Resuming` it **skipped restore and fell through to the cold-start
plan**, which for a snapshot-fork child (poolRef=generic pool, `forkFrom` but no `appRef`)
errored `pool "X" is generic ... needs an appRef; a poolRef-only session has nothing to
run`. So a durable (`idleAction: suspend`) snapshot-fork couldn't teleport-resume after its
first idle-suspend — but only when a restore had failed first. The error was a *symptom of
the un-rolled-back Resuming state*, not the re-resume/plan logic. **Fix (two layers):**
(a) inline — `resumeFromSnapshot` calls `rollbackToSuspended` (CAS `Resuming`+snapshotURI →
`Suspended`, clear worker) on failure, so the next request retries the restore; (b)
backstop — the `ResumingHealer` rolls a stuck‑`Resuming`‑**with‑snapshotURI** entry back to
`Suspended` when the worker `/status` is 404/unreachable (previously it only "left for
retry"). Regressions: `TestResumeRollsBackToSuspendedOnRestoreFailure`,
`TestHealerRollsBackFailedRestore`, `TestHealerRollsBackNotRunningWithSnapshot`. *Live
evidence:* healer logged `rolled back to Suspended for restore retry` on the worker 404,
then the fork re-resumed and restored its **preserved diverged state** (sum=50, same boot).

**#3 — A health-less workload wedged `Resuming` forever (readiness had no PROCESS mode).**
The supervisor (`supervisor.go`) only set `hs.ready` when `Probe∈{tcp,http} && len(Ports)>0`;
a workload with no probe/port never became ready, so `WaitReady`/resume timed out and the
KV entry wedged `Resuming`. Initial instinct was an admission rule *requiring* a probe, but
that wrongly outlaws legitimate **portless/batch/exec/headless** workloads (they have no
service to probe). **Fix:** worker readiness now has two modes — PROBE (tcp/http + port) vs
PROCESS ("running == ready" when there's no usable probe), via `usesProbe(health,nPorts)`.
Portless workloads reach `Running` instead of wedging. A **consistency-only** CEL rule on
AppTemplate + SandboxTemplate rejects only the genuinely-broken case (a tcp/http probe with
**no** `probePort`), never the absence of a probe. Regressions: `TestUsesProbe` (worker),
`health_cel_test.go` (envtest accept/reject), and the existing fixtures gained health where
they model dedicated/service workloads. *Live evidence:* a portless busybox AppTemplate
warmed to `Running` (previously 502/`Resuming`); CEL rejected a portless-but-http-probe
template at admission.

**#4 — Benign optimistic-concurrency 409s logged at ERROR with a stack trace.**
`SessionReconciler` and `BaseSnapshotReconciler` returned the standard k8s 409 ("the object
has been modified; please apply your changes to the latest version") from their
finalizer/status `Update` calls as a plain error, so controller-runtime logged it at ERROR
with a full stack trace on every CR-churn race — alarming noise that repeatedly looked like
a real failure during testing. **Fix:** detect `apierrors.IsConflict` on those Update paths
and requeue quietly (`ctrl.Result{Requeue:true}`) instead of returning the error. (Several
status writes already did this; extended to the finalizer-Update + basesnapshot delete
paths.) Not a correctness bug — a requeue always resolved it — purely log hygiene.

**Not platform bugs (recorded for honesty):** direct `redis-cli DEL` of a `session:` key
orphans the running sandbox (no legitimate actor does this — use CR delete); reusing
session ids while duplicates ran; aggressive client timeouts interrupting cold-starts. All
self-inflicted during exploratory testing.

**Follow-ups (NOT fixed here — separate branches):**
- **Image restore re-resolves the base image TAG against the registry every time.** A
  restore must pull the base OCI image for the rootfs (the checkpoint holds only RAM +
  overlay diff), and `prepareRootfsContainerd` calls containerd `Pull` with a **tag**, so
  containerd does a registry manifest resolve **even when the layers are cached** — the
  intermittent docker.io `failed to resolve reference ... i/o timeout` seen throughout.
  Fix: pin base images by **digest** in the checkpoint/`BaseSnapshot` restore path (fully
  cache-served, no registry round-trip); or a registry mirror; or an offline-first
  skip-pull-when-unpacked check.
- **Session finalizer `ReleaseForDelete` wedges when the bound worker is gone.** Deleting a
  Session whose KV entry points at a now-absent worker retried `delete session ... context
  deadline exceeded` forever (had to strip the finalizer manually). It should treat an
  unreachable/absent worker as already-released and proceed with KV cleanup + finalizer
  removal.
- **Intermittent worker-pod DNS/egress + SPIRE-SVID acquisition reliability.** The SPIRE
  agent restart that crashed the v66 workers, plus one-off CoreDNS UDP timeouts, point at
  worker-node network/SPIRE robustness worth hardening (NodeLocal DNS cache? SVID fetch
  retry/backoff before the worker exits 1?).

**Verified live end-to-end after the fixes** (operator v53, worker v66, memory-limited
pools): agent OOM-protection (bomb killed in its own cgroup, agent survives), teleport of
the `everything` app and the **AIO** sandbox (in-`/home/gem` marker byte-identical across
workers; the 159 MB AIO checkpoint restored on a different worker; per-sandbox mem limit
`1879048192` intact through teleport), app-fork + snapshot-fork (identical golden state,
independent divergence; suspended snapshot-fork re-resume preserved its diverged state),
and the four robustness fixes above each verified on the cluster.
