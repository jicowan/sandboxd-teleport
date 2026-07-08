# API reference — sandboxd worker

The sandboxd worker agent exposes a small HTTP API on **`:8090`** (configurable
via `SANDBOXD_ADDR`). It is the low‑level interface the operator drives to run,
checkpoint, restore, suspend, and inspect nested gVisor sandboxes on a worker pod.

> **Audience & trust.** This API is an **internal control interface**, not a
> user‑facing one. In normal operation only the operator (resume/suspend/checkpoint
> workflows) and the router (data‑plane proxy to the sandbox's *workload* port,
> not to `:8090`) talk to a worker. It has no authentication of its own — it relies
> on being reachable only in‑cluster (the planned P1.5 phase adds mTLS +
> NetworkPolicy). Documented here for operators debugging a worker directly and for
> anyone building against the control plane.

## Conventions

- **Base URL:** `http://<worker-pod-ip>:8090`.
- **Content type:** requests and responses are JSON (`application/json`) except
  `/logs` (`text/plain`) and `/healthz` (plain text).
- **Errors:** non‑2xx responses are `{"error": "<message>"}`. A malformed JSON body
  returns `400 {"error":"bad json: …"}`.
- **Methods are not enforced by the router.** The handlers document an intended
  verb (below); in practice what differentiates them is body‑vs‑query usage. Use the
  intended verb.
- **`sandboxId`** identifies a sandbox on this worker. The operator uses the
  session id; if omitted on `/run` the worker generates one.
- **S3 gating.** `/checkpoint`, `/restore`, and `/suspend` require the worker to be
  started with `SANDBOXD_BUCKET` set; otherwise they return `503 {"error":"s3 not
  configured"}`.
- **Networking gating.** Requests that declare `ports` require the sandbox network
  data path (`SANDBOXD_NETWORK=sandbox` + `SANDBOXD_POD_IP` set). Otherwise they
  return `400` ("sandbox networking unavailable").

## Endpoint summary

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/run` | Pull an image and start a new nested sandbox. |
| POST | `/checkpoint` | Checkpoint a running sandbox to S3 (optionally leave it running). |
| POST | `/restore` | Restore a sandbox from an S3 snapshot. |
| POST | `/suspend` | Checkpoint to S3, then free the worker (delete + cleanup). |
| POST | `/reset` | Free the worker **without** checkpointing (discard state). |
| GET | `/status?sandboxId=` | Runtime status of one sandbox. |
| GET | `/capacity` | Is this worker busy? (one sandbox per worker.) |
| GET | `/sandboxes` | List all tracked sandboxes on this worker. |
| DELETE | `/sandbox?sandboxId=` | Delete a sandbox (best‑effort teardown + cleanup). |
| GET | `/logs?sandboxId=` | Nested gVisor + launch logs (text). |
| GET | `/metrics` | Counters. |
| GET | `/healthz` | Liveness (`ok`). |
| GET | `/version` | Pinned `runsc` version + configured bucket. |

## Shared body types

**PortMap** — maps a worker pod port to the sandbox's interior container port
(nftables DNAT `podIP:host → 169.254.17.2:container`):

```json
{ "container": 8080, "host": 8080 }   // host omitted or 0 defaults to container
```

**health** — supervisor probe / restart / idle config:

```json
{
  "restartPolicy": "none|cold|restore",
  "probe": "none|tcp|http",
  "probePort": 8080,
  "probePath": "/v1/health",
  "idleTimeoutSec": 600
}
```

---

## POST /run

Pull an image (via the node's containerd cache) and start it as a detached nested
gVisor sandbox.

**Request**

```json
{
  "image": "ghcr.io/agent-infra/sandbox:latest",   // required
  "cmd": ["..."],                                    // optional; overrides entrypoint+cmd
  "env": ["KEY=VALUE"],                              // optional; appended to image env
  "sandboxId": "sess-…",                             // optional; generated if empty
  "ports": [{ "container": 8080, "host": 8080 }],    // optional
  "health": { "probe": "http", "probePort": 8080, "probePath": "/v1/health" },
  "iamRoleArn": "arn:aws:iam::…:role/…"              // optional; vend per-session AWS creds for this role
}
```

`iamRoleArn` (optional): when set and the credential vendor is enabled
(`SANDBOXD_CRED_TOKEN_KEY`), the worker registers the role and injects
`AWS_CONTAINER_CREDENTIALS_FULL_URI` + `AWS_CONTAINER_AUTHORIZATION_TOKEN` into the
sandbox so its AWS SDK gets per-session temporary credentials for the role.

**Response `200`**

```json
{ "sandboxId": "sess-…", "status": "running", "image": "…", "ports": [ … ] }
```

**Errors:** `400` image required / bad ports; `409` sandbox already exists;
`502` image pull failed; `500` spec/runsc/network failure.

---

## POST /checkpoint

Checkpoint a sandbox to S3 as a single atomic RAM+FS image. The exact
`config.json` is stored alongside so restore enforces a spec match.

**Request**

```json
{
  "sandboxId": "sess-…",     // required
  "leaveRunning": false,      // keep the sandbox running after checkpoint
  "compress": true            // optional; null ⇒ server default (SANDBOXD_COMPRESS)
}
```

**Response `200`**

```json
{
  "sandboxId": "sess-…",
  "snapshot": "sandboxes/sess-…/snap-1700000000000000000",
  "sizeBytes": 123456789,
  "image": "…",
  "digest": "sha256:…",
  "runscVersion": "release-20260622.0"
}
```

**Errors:** `400` bad id; `404` unknown sandbox; `503` S3 not configured;
`500` checkpoint failed; `502` S3 upload failed.

> `compress` (flate) makes the image ~6.5× smaller on S3 but forces eager
> page‑load on restore (compressed images can't use background restore).

---

## POST /restore

Establish a sandbox from an S3 snapshot (teleport). `runsc restore` establishes
and restores in one step — there is no separate `create`.

**Request**

```json
{
  "sandboxId": "sess-…",                 // optional; generated if empty
  "image": "ghcr.io/agent-infra/sandbox:latest",  // required (base rootfs)
  "snapshot": "sandboxes/sess-…/snap-…", // required (S3 prefix)
  "runscVersion": "release-20260622.0",  // optional; 409 on mismatch
  "ports": [{ "container": 8080, "host": 8080 }],
  "health": { "probe": "http", "probePort": 8080, "probePath": "/v1/health" },
  "iamRoleArn": "arn:aws:iam::…:role/…"  // optional; re-establish per-session AWS creds after teleport
}
```

`iamRoleArn` re-registers the session's role with the new worker's credential
vendor on teleport (the AWS env already travels baked into the checkpoint; the
per-session token is deterministic so it keeps matching).

**Response `200`**

```json
{ "sandboxId": "sess-…", "status": "running", "restoredFrom": "sandboxes/…", "ports": [ … ] }
```

**Errors:** `400` image/snapshot required; `409` sandbox exists **or** runsc
version mismatch (`{"error":"runsc version mismatch","want":…,"have":…}`);
`503` S3 not configured; `502` pull/download failed; `500` network/restore failure.

> Restore rebuilds the veth/netns with the **same interior IP**, so the session is
> reachable at the same `podIP:hostPort` after teleport. Sockets do not survive —
> clients reconnect (fresh MCP `initialize`).

---

## POST /suspend

Checkpoint to S3, then free the worker (delete the sandbox + tear down networking
+ cleanup + forget). This is the idle‑suspend primitive: state persists only in
S3 and the worker becomes reusable.

**Request**

```json
{ "sandboxId": "sess-…" }
```

**Response `200`**

```json
{ "sandboxId": "sess-…", "snapshot": "sandboxes/sess-…/snap-…", "image": "…", "suspended": true }
```

**Errors:** `400` bad id; `404` unknown sandbox; `503` S3 not configured;
`500` checkpoint failed; `502` upload failed.

---

## POST /reset

Free the worker **without** checkpointing — discards all sandbox state. Used to
reclaim a worker when the session's state should be thrown away.

**Request**

```json
{ "sandboxId": "sess-…" }
```

**Response `200`**

```json
{ "sandboxId": "sess-…", "reset": true }
```

---

## GET /status?sandboxId=

Runtime status of one sandbox (query parameter, not a body).

**Response `200`**

```json
{ "sandboxId": "sess-…", "status": "running", "ready": true, "idle": false, "restarts": 0 }
```

**Errors:** `400` bad id; `404` unknown to runsc.

---

## GET /capacity

Whether this worker is busy (one sandbox per worker). Used by the operator's
idle‑worker selection.

**Response `200`**

```json
{ "busy": true, "count": 1, "sandboxId": "sess-…", "idle": false }
```

(`sandboxId`/`idle` present only when busy.)

---

## GET /sandboxes

List all sandboxes this worker tracks.

**Response `200`**

```json
{
  "sandboxes": [
    {
      "id": "sess-…", "image": "…", "digest": "sha256:…", "bundle": "/work/bundles/…",
      "snapshot": "sandboxes/…", "ports": [ … ], "health": { … },
      "runscVersion": "release-20260622.0", "createdAt": "2026-07-07T…Z"
    }
  ]
}
```

---

## DELETE /sandbox?sandboxId=

Best‑effort teardown of a sandbox: delete runsc state, tear down networking,
remove artifacts and metadata. (Query parameter.)

**Response `200`**

```json
{ "sandboxId": "sess-…", "deleted": "true" }
```

---

## GET /logs?sandboxId=

Returns `text/plain`: the sandbox's launch logs (`<id>.run.log`,
`<id>.restore.log`) plus a tail of the nested gVisor sentry/gofer debug logs. This
is the **session‑scoped** log path — the production way to read a sandbox's logs
(as opposed to the opt‑in console streaming that mixes into the worker's
`kubectl logs`).

---

## GET /metrics

Counters plus tracked‑sandbox count.

**Response `200`** (shape; keys are counters)

```json
{
  "checkpoints": 3, "restores": 2, "suspends": 1, "resets": 0,
  "restarts": 0, "restart_failures": 0, "requests": 42, "panics": 0,
  "tracked_sandboxes": 1
}
```

---

## GET /healthz

Liveness probe. Returns the literal text `ok` with `200`.

---

## GET /version

**Response `200`**

```json
{ "runsc": "runsc version release-20260622.0\n…", "bucket": "aio-checkpoint-spike-820537372947-us-west-2" }
```

---

## Worker configuration (environment variables)

The worker is configured entirely by environment variables (set on the worker pod,
usually by the operator from the `SandboxTemplate`/global flags):

| Env var | Default | Meaning |
|---------|---------|---------|
| `SANDBOXD_WORK` | `/work` | Base workdir (bundles, runsc `-root`, image staging, snapshots). |
| `SANDBOXD_RUNSC` | `/usr/local/bin/runsc` | Path to the pinned `runsc` binary. |
| `SANDBOXD_ADDR` | `:8090` | HTTP listen address. |
| `SANDBOXD_BUCKET` | `""` | S3 bucket for checkpoints. Empty ⇒ checkpoint/restore/suspend return `503`. |
| `SANDBOXD_POD_IP` | `""` | Worker pod IP (via downward API `status.podIP`); required for the sandbox network data path. |
| `SANDBOXD_NETWORK` | `sandbox` | runsc network mode: `sandbox` (checkpointable netstack), `host`, or `none`. Only `sandbox` supports port exposure/teleport. |
| `SANDBOXD_COMPRESS` | on | Compress checkpoint pages by default; set `=0` to disable. |
| `SANDBOXD_DEBUG` | `0` | `=1` enables verbose runsc `--debug` + streams `[gvisor …]` logs to stdout. |
| `SANDBOXD_STREAM_CONSOLE` | off | `=1` streams the nested workload's console to the worker's stdout (`[sandbox <id>] …`), sanitized + capped. |
| `SANDBOXD_STREAM_CONSOLE_MAX_BYTES` | `8388608` (8 MiB) | Cap on relayed console bytes per sandbox. |
| `SANDBOXD_SUPERVISE_INTERVAL` | `10s` | Supervisor loop period (readiness/restart/idle). |
| `SANDBOXD_GC_INTERVAL` | `5m` | Background on‑disk artifact GC period. |
| `SANDBOXD_DRAIN_DEADLINE` | `100s` | On SIGTERM, how long the worker keeps serving (drain‑waits) so the operator can checkpoint‑on‑terminate before exit. Keep it under the pod's `terminationGracePeriodSeconds` (120s). |
| `SANDBOXD_CRED_TOKEN_KEY` | `""` | Fleet‑wide HMAC key for the per‑session AWS credential vendor. Set (non‑empty) enables the vendor; the sandbox's `AWS_CONTAINER_AUTHORIZATION_TOKEN` = `HMAC(this, sid)`. Injected by the operator from the `--cred-token-secret` Secret. Empty = vendor disabled. |
| `SANDBOXD_CRED_PORT` | `8091` | Port the credential vendor listens on (bound to `169.254.170.2`, reachable only from the worker's sandbox netns). |
| `AWS_REGION` | (SDK default) | Region for S3 (via the AWS default credential/config chain → EKS Pod Identity). |

S3 credentials come from the AWS default chain (EKS Pod Identity via the worker's
ServiceAccount) — there are no static keys.

---

## How the operator uses this API

For context, the resume/suspend workflows map onto these endpoints:

- **Cold start** (new session): operator claims an idle worker → `POST /run` with
  the template's image/ports/health → waits for `/status` ready → marks the session
  Running.
- **Teleport / resume from snapshot:** operator claims an idle worker → `POST
  /restore` with the snapshot URI → waits ready → marks Running.
- **Idle suspend:** operator → `POST /suspend` → session Suspended, worker freed.
- **Reclaim without saving:** `POST /reset`.
- **Router** proxies user traffic to the sandbox's **workload port** (e.g. `:8080`
  via the DNAT), *not* to `:8090` — the control API and the data path are separate.

See [architecture-sandboxd.md](architecture-sandboxd.md) for the full lifecycle.
