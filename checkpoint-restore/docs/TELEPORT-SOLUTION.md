# Teleport Solution — running & migrating an arbitrary nested gVisor container

Status: Design + prototype (2026-07-02), `checkpoint-restore` branch.
Builds on the proven mechanics in [`../NOTES.md`](../NOTES.md) and
[`RUNBOOK-nested-cross-pod.md`](./RUNBOOK-nested-cross-pod.md).

## What we're building (the user's ask)

A solution where a user can:
1. **Run an arbitrary container image** as a gVisor sandbox **nested inside a
   worker pod** (the image is the user's choice, not baked in).
2. Have the solution **checkpoint** that nested sandbox (memory + filesystem).
3. **Restore** that state onto **another worker pod** — "teleport."

The user never touches `runsc`. They talk to a control surface; a **worker agent**
inside each worker pod does the runsc + S3 work.

## Roles

```
 user / broker
      │  (HTTP control API)
      ▼
 ┌──────────────────── worker pod (privileged, runc, gvisor node) ─────────────┐
 │  sandboxd  (worker agent — HTTP API + drives runsc + S3)                    │
 │    POST /run       {image, cmd, env}      → pull image, build bundle, runsc  │
 │    POST /checkpoint {sandboxId}           → runsc checkpoint → upload to S3  │
 │    POST /restore    {sandboxId, from:S3}  → download → runsc create+restore  │
 │    GET  /status     {sandboxId}           → runsc state                      │
 │    DELETE /sandbox  {sandboxId}           → runsc delete                     │
 │                                                                              │
 │    runsc -root /work/rt --overlay2=root:self  → [ nested gVisor sandbox ]    │
 └──────────────────────────────────────────────────────────────────────────────┘
      │  checkpoint images                                    ▲
      ▼                                                       │
   S3 (per-sandbox prefix: s3://…/sandboxes/<id>/<snapshotId>/)
```

- **sandboxd**: the worker agent. **Minimal surface area by design: a `scratch`
  (or static-distroless) image containing ONE statically-linked Go binary, plus a
  pinned `runsc` binary. No shell, no package manager, no OS** — the correct
  posture for a privileged pod (tiny attack surface, immutable). This is exactly
  substrate's `ateom-gvisor` shape.
  - It does everything as **libraries, not by shelling out**:
    - Pull + flatten any OCI image → `github.com/google/go-containerregistry`
      (the library crane wraps; no `crane`/`tar`/daemon needed).
    - S3 I/O → `aws-sdk-go-v2` (Pod Identity).
    - Build the OCI bundle (write config.json, lay down rootfs) → in Go.
    - Drive the sandbox → exec the pinned `runsc`.
  - **`runsc` is bundled + pinned** in the worker image (not just hostPath-mounted)
    so its version is guaranteed to match across all workers → restore-compat.
- **Teleport = checkpoint on worker A → S3 → restore on worker B.** Neither the
  user nor the control plane runs runsc; sandboxd does.

> **NOTE — the busybox spikes used a fat debian worker that `apt-get`ed curl and
> downloaded the `crane` CLI at runtime. That was throwaway scaffolding.** The
> real worker is scratch + one Go binary; go-containerregistry and aws-sdk are
> imported as libraries, not invoked as external tools.

## How "run an ARBITRARY image" works (the key new capability vs. the busybox spike)

The busybox spikes hardcoded the image. To make it arbitrary, `POST /run` does,
inside the worker pod, with **no host containerd and no daemon**:
1. `crane export <user-image> - | tar -x -C <bundle>/rootfs`  → flatten any OCI
   image's rootfs (crane pulls straight from the registry; works for private
   registries with creds).
2. `crane config <user-image>` → read the image's **Entrypoint/Cmd/Env/WorkingDir**
   so the OCI spec matches how the image is meant to run (this is what a real
   runtime does; the busybox spike hand-wrote args).
3. `runsc spec`, then patch: `root.readonly=false`, `-overlay2=root:self`, inject
   the image's process config (+ user overrides), essential mounts
   (`/etc/hosts`,`/etc/resolv.conf`,`/dev/shm`).
4. `runsc create + start` → the user's image now runs as a nested gVisor sandbox.

**Image identity across teleport:** the checkpoint records the image ref +
config digest. Restore on worker B re-`crane export`s the **same image ref** for
the base rootfs, drops in the **same config.json**, then `runsc create+restore`.
Only the checkpoint (memory + rw overlay delta) travels via S3; the base image
comes from the registry on each worker (cache-friendly).

## What travels vs. what's local (cost model, from measurements)

- **Travels via S3:** the checkpoint = `checkpoint.img` + `pages.img` +
  `pages_meta.img`. Busybox ~360K; AIO ~600–700M (RAM-dominated). This is the
  latency driver (AIO S3 round-trip ~7.6s measured).
- **Local to each worker:** the base image rootfs (from registry/cache). Never
  travels. → strong argument to keep memory images lean and, later, ship only fs
  deltas + touched pages.

## Control API (sandboxd) — prototype surface

| Verb | Request | sandboxd does | Returns |
|---|---|---|---|
| `POST /run` | `{image, cmd?, env?, sandboxId?}` | crane export+config → spec → `runsc create`+`start` | `{sandboxId, status}` |
| `POST /checkpoint` | `{sandboxId}` | `runsc checkpoint` → upload `s3://…/<id>/<snap>/` | `{snapshotId, uri, sizeBytes}` |
| `POST /restore` | `{sandboxId, image, uri}` | download from S3 → crane export base → `runsc create`+`restore` | `{sandboxId, status}` |
| `GET /status` | `{sandboxId}` | `runsc state` | `{status}` |
| `DELETE /sandbox` | `{sandboxId}` | `runsc delete -force` | `{ok}` |

State that must be recorded per sandbox (for teleport): **image ref**, **config
digest**, **latest snapshot URI**. Prototype: a JSON file per sandbox in the
worker; product: the control-plane store (substrate uses Redis).

## Teleport sequence (worker A → worker B), end to end

```
user → A:/run {image:X}            → sandbox running on A
user → A:/checkpoint {id}          → snapshot to s3://…/id/snap1/ ; (A may keep running or be freed)
control-plane picks worker B
user → B:/restore {id, image:X, uri:s3://…/id/snap1/}
                                    → B: crane export X → download snap → runsc restore
                                    → sandbox resumes on B with mem+fs intact
```

## Open questions to resolve in the prototype

1. **Arbitrary-image gVisor compatibility.** Not every image runs under gVisor
   (syscalls, `/proc` expectations, first-boot bootstrap like AIO's `groupadd`).
   `/run` must surface a clear "started / crashed" status. gVisor-incompatible
   images are a user-facing error, not a silent failure.
2. **CPU-feature / runsc-version match** across A and B (cross-node/gen). Pin the
   worker node family or set `dev.gvisor.internal.cpufeatures`; sandboxd records
   the runsc version in the snapshot manifest and refuses mismatched restores.
3. **Registry auth** for private images (crane supports it; needs creds wiring).
4. **Networking**: sandbox gets a fresh netns on restore (sockets don't survive);
   stable interior IP so reconnects work — a sandboxd/worker-networking concern.
5. **Privileged worker (DinD)** tradeoff stands (see NOTES A-vs-B). This solution
   is path (A); it's what lets a user run *arbitrary* nested images today.

## Known follow-ups (deferred, not blocking)

- **Nested gVisor logs not in `kubectl logs`.** The sentry/gofer `--debug-log`
  files are captured and reachable via `GET /logs?sandboxId=` (used for
  debugging), but they do NOT stream to the pod's stdout. Reason: the detached
  `runsc run/restore` must use `/dev/null` stdio (a tee/pipe made cmd.Run() block
  forever). To surface them in `kubectl logs` later: a background goroutine that
  tails the `--debug-log` dir → sandboxd stdout (decoupled from the exec'd
  process, so no fd inheritance / blocking). Low priority; logs are available.

## Prototype plan (this session)

- **P1** — one worker pod runs sandboxd; `POST /run` an **arbitrary image**
  (e.g. `alpine`, `python`) as a nested gVisor sandbox; `GET /status` shows it.
- **P2** — `POST /checkpoint` → S3 (Pod Identity), returns a snapshot URI.
- **P3** — second worker pod; `POST /restore {uri}` → sandbox resumes with state
  (prove with an in-sandbox counter/file, as in the busybox runbook).
- **P4** — pick a non-trivial arbitrary image and see how far it gets (compat).

## Prototype status (2026-07-02) — P1/P2/P3 PROVEN ✅

End-to-end teleport of an ARBITRARY image works through the sandboxd HTTP API:
- **P1 /run**: sandboxd pulled `alpine:3.20` (library, no daemon), built the OCI
  bundle, ran it as a nested gVisor sandbox → `status=running`.
- **P2 /checkpoint**: `runsc checkpoint` (~75ms, ~315KB) + uploaded to
  `s3://…/sandboxes/<id>/<snap>/` incl. the exact `config.json` (~250ms).
- **P3 /restore on a DIFFERENT worker pod**: pulled the same base image, DL'd the
  checkpoint from S3, restored → `status=running` in ~420ms; counter continued
  (43→48…), NOT reset. Memory + filesystem teleported A → S3 → B.

Bugs found + fixed on the way (all in sandboxd, ECR 820537372947/sandboxd v1→v9):
1. `runsc start` blocks (foreground) → use `run -detach`.
2. detached run/restore inherits stdio → cmd.Run() hangs on pipes; use /dev/null
   stdio + rely on `--debug-log` files.
3. restore rebuilt spec from image defaults → `RestoreSpecValidation=enforce`
   mismatch; fix: persist the exact config.json in S3 with the checkpoint, reuse.
4. directfs needs /proc/self/uid_map (absent nested) → `--directfs=false`.
5. **separate `create` before `restore` orphans a sandbox** ("Watchdog.Start()
   not called within 30s", stuck `created`) → `restore` ALONE establishes+restores.

Logging: rich step/timing logs on checkpoint+restore; `GET /logs?sandboxId=`
surfaces the nested gVisor sentry/gofer debug logs (used to root-cause all of the
above). Known follow-up: also stream them to `kubectl logs` (see above).
