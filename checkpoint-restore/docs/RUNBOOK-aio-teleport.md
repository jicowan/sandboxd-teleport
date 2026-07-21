# Runbook — Teleport a full AIO sandbox between workers via sandboxd

Reproduces what we proved 2026-07-02: run the **AIO all-in-one image** (Chrome +
MCP hub, ~3.4GB) as a **nested gVisor sandbox** inside a worker pod, reachable
over the network, then **checkpoint → S3 → restore onto a different worker** with
its memory + filesystem state intact and the MCP server functional.

This is driven entirely through the **sandboxd** HTTP API. sandboxd is the
minimal worker agent (distroless + one Go binary + pinned runsc) built in
`../sandboxd/`. See [`TELEPORT-SOLUTION.md`](./TELEPORT-SOLUTION.md) and
[`NETWORKING-LIFECYCLE.md`](./NETWORKING-LIFECYCLE.md) for the design.

---

## Prerequisites (already provisioned; listed so it's reproducible)

- **EKS cluster** with a gVisor node group: nodes labeled `sandbox=gvisor` + taint
  `sandbox=gvisor:NoSchedule`, `runsc` (release-20260622.0) on-node, 100Gi root
  disk (AIO's flattened rootfs is ~8.9GB + a 790MB checkpoint).
- **S3 + Pod Identity**: bucket `aio-checkpoint-spike-820537372947-us-west-2`;
  IAM role `aio-checkpoint-spike-role` (trust `pods.eks.amazonaws.com`, scoped to
  the bucket); ServiceAccount `default/ckpt-spike` associated to that role.
- **ECR repo** `820537372947.dkr.ecr.us-west-2.amazonaws.com/sandboxd`.
- Tools locally: `go` (1.25), `docker` (buildx, linux/amd64), `kubectl`, `aws`.
- `runsc` binary pinned in the image MUST match the node's runsc version
  (gVisor hard-errors on restore mismatch).

## 0. Build & push sandboxd (skip if `sandboxd:v24`+ already in ECR)

```sh
cd checkpoint-restore/sandboxd
# static linux binary
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o sandboxd .
# pinned runsc, version-matched to the node
curl -sSL -o runsc \
  https://storage.googleapis.com/gvisor/releases/release/20260622.0/x86_64/runsc
chmod +x sandboxd runsc
aws ecr get-login-password --region us-west-2 | \
  docker login --username AWS --password-stdin 820537372947.dkr.ecr.us-west-2.amazonaws.com
docker build --platform linux/amd64 \
  -t 820537372947.dkr.ecr.us-west-2.amazonaws.com/sandboxd:v24 .
docker push 820537372947.dkr.ecr.us-west-2.amazonaws.com/sandboxd:v24
rm -f sandboxd runsc   # don't commit the 130MB binaries
```

The image is **distroless + `sandboxd` + pinned `runsc`** (Dockerfile in
`../sandboxd/`). Key env (set in `worker-deploy.yaml`):
`SANDBOXD_BUCKET`, `AWS_REGION`, `SANDBOXD_POD_IP` (downward API — required for
the veth/nftables data path), optional `SANDBOXD_NETWORK` (default `sandbox`),
`SANDBOXD_DEBUG` (verbose gVisor logs; leave OFF normally).

## 1. Deploy the worker pool (2 replicas: A and B)

```sh
kubectl apply -f checkpoint-restore/sandboxd/worker-deploy.yaml
kubectl rollout status deploy/sandboxd-worker -n default
kubectl get pods -n default -l app=sandboxd-worker -o wide
```
Workers are privileged (nested gVisor = DinD), on the gvisor node, SA `ckpt-spike`
(S3 via Pod Identity). Each logs `networking: interior netns ready (podIP=…,
interiorIP=169.254.17.2)` on startup — confirms the data path is armed.

> Helper for all API calls below: port-forward to a worker and curl. Or curl a
> worker's pod IP directly from another pod. Set:
> ```sh
> WA=$(kubectl get pods -n default -l app=sandboxd-worker -o jsonpath='{.items[0].metadata.name}')
> WB=$(kubectl get pods -n default -l app=sandboxd-worker -o jsonpath='{.items[1].metadata.name}')
> WAIP=$(kubectl get pod $WA -n default -o jsonpath='{.status.podIP}')
> WBIP=$(kubectl get pod $WB -n default -o jsonpath='{.status.podIP}')
> kubectl port-forward -n default pod/$WA 18090:8090 &   # A control API on :18090
> kubectl port-forward -n default pod/$WB 18092:8090 &   # B control API on :18092
> ```

## 2. Run AIO on worker A (nested gVisor, networked)

```sh
curl -s -X POST http://localhost:18090/run -H 'Content-Type: application/json' -d '{
  "sandboxId":"aio",
  "image":"ghcr.io/agent-infra/sandbox:latest",
  "ports":[{"container":8080,"host":8080}],
  "health":{"probe":"http","probePort":8080,"probePath":"/v1/sandbox"}
}'
```
- **First run per worker is SLOW (~3 min):** sandboxd pulls + flattens the 3.4GB
  image into its per-digest rootfs cache (`<work>/imgcache/<digest>`). Subsequent
  runs of the same image are **~17s** (hardlink copy from cache).
- The HTTP call blocks until the sandbox is created; the pull happens inline. (A
  known follow-up is async `/run`.)

Wait for readiness, then verify AIO is fully up:
```sh
curl -s "http://localhost:18090/status?sandboxId=aio"        # -> "ready":true
# REST:
curl -s http://$WAIP:8080/v1/sandbox                          # -> 200, env payload
# MCP protocol (initialize):
curl -s -X POST http://$WAIP:8080/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}'
# -> serverInfo {"name":"Sandbox MCP Tools","version":"2.14.7"}
```

## 3. Write state (to prove FS teleports)

Use AIO's own shell tool to write a marker inside the sandbox:
```sh
curl -s -X POST http://$WAIP:8080/v1/shell/exec -H 'Content-Type: application/json' \
  -d '{"command":"echo TELEPORT-MARKER-42 > /home/gem/marker.txt && cat /home/gem/marker.txt"}'
# -> output: TELEPORT-MARKER-42
```

## 4. Checkpoint on A → S3

```sh
curl -s -X POST http://localhost:18090/checkpoint -H 'Content-Type: application/json' \
  -d '{"sandboxId":"aio"}'
# -> {"snapshot":"sandboxes/aio/snap-…","sizeBytes":~790000000, ...}
```
- Captures the FULL memory image (~783MB: Chrome/Jupyter/MCP hub) + fs, plus the
  exact `config.json`, and uploads to `s3://…/sandboxes/aio/<snap>/`.
- With `leaveRunning:false` (default) the sandbox is stopped and the worker freed.
- Save the returned snapshot path:
  ```sh
  SNAP=sandboxes/aio/snap-…        # from the response
  aws s3 ls s3://aio-checkpoint-spike-820537372947-us-west-2/$SNAP/
  # checkpoint.img  config.json  pages.img  pages_meta.img
  ```

## 5. Restore on worker B (the teleport)

```sh
curl -s -X POST http://localhost:18092/restore -H 'Content-Type: application/json' -d "{
  \"sandboxId\":\"aio-b\",
  \"image\":\"ghcr.io/agent-infra/sandbox:latest\",
  \"snapshot\":\"$SNAP\",
  \"ports\":[{\"container\":8080,\"host\":8080}]
}"
# -> {"sandboxId":"aio-b","status":"running","restoredFrom":"…"}
```
- B re-materializes the **same base rootfs from its cache** (cold cache = ~3 min
  pull the first time; warm = fast), **downloads the ~790MB checkpoint from S3**,
  reuses the exact `config.json`, rebuilds the veth/nftables with the same
  interior IP, and `runsc restore`s.
- S3 up/download run on a background context, so a client-side curl timeout does
  NOT cancel them (the op finishes server-side; poll `/status` or `/metrics`).

## 6. Verify the teleport on B

```sh
# memory state resumed — MCP hub answers on B's pod IP:
curl -s -X POST http://$WBIP:8080/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"post","version":"0"}}}'
# -> serverInfo {"name":"Sandbox MCP Tools","version":"2.14.7"}

# filesystem state survived — the marker written on A is present on B:
curl -s -X POST http://$WBIP:8080/v1/shell/exec -H 'Content-Type: application/json' \
  -d '{"command":"cat /home/gem/marker.txt"}'
# -> output: TELEPORT-MARKER-42
```

**Success = both:** MCP `serverInfo` returns on B (memory teleported) AND
`marker.txt` = `TELEPORT-MARKER-42` on B (filesystem teleported).

## Cheat sheet

```
[A] POST /run   {image:aio, ports:[8080]}          # nested gVisor, ~3min cold / ~17s warm
[A] POST /v1/shell/exec {echo MARKER > /home/gem/marker.txt}   # write fs state
[A] POST /checkpoint {sandboxId:aio}               # -> S3 snapshot (~790MB), worker freed
[B] POST /restore {image:aio, snapshot:$SNAP, ports:[8080]}    # teleport onto B
[B] curl :8080/mcp initialize  ->  serverInfo v2.14.7          # memory survived
[B] curl :8080 shell cat marker.txt -> MARKER                  # fs survived
```

## sandboxd API reference (used above)

| Verb | Purpose |
|---|---|
| `POST /run {image,ports,cmd?,env?,health?,sandboxId?}` | pull(+cache)→bundle→nested gVisor |
| `POST /checkpoint {sandboxId,leaveRunning?}` | `runsc checkpoint` → upload to S3 |
| `POST /restore {image,snapshot,ports?,sandboxId?}` | download → `runsc restore` |
| `GET /status?sandboxId=` | status + ready/idle/restarts |
| `POST /suspend {sandboxId}` | checkpoint→S3→free worker (reuse) |
| `POST /reset {sandboxId}` | free worker without checkpoint |
| `GET /capacity` | busy/idle (for a scheduler) |
| `GET /sandboxes` / `GET /metrics` | list / counters |
| `GET /logs?sandboxId=` | nested gVisor (sentry/gofer) logs |
| `DELETE /sandbox?sandboxId=` | delete |

## Gotchas (each cost real debugging)

- **Writable `/tmp`**: AIO's python-server (runs as user `gem`) does
  `os.makedirs('/tmp/aio-sandbox')`; without a `/tmp` tmpfs (mode 1777) it
  crash-loops with `PermissionError` and `:8080` never comes up. sandboxd's OCI
  spec provides it.
- **DNS**: without `/etc/resolv.conf` in the sandbox, AIO services fail hostname
  lookups (`EAI_AGAIN`, nginx stalls). sandboxd writes one (copied from the pod's
  resolver) on networked runs. It breaks the cache hardlink first (remove +
  create) so it doesn't corrupt the shared image cache.
- **`--network=sandbox`, NOT `--network=host`**: host-net is reachable but
  `checkpoint not supported when using hostinet`. Only netstack (`sandbox`) is
  checkpointable; reachability comes from the veth + nftables DNAT.
- **S3 ops must use a background context**, not the HTTP request context — a
  client timeout was canceling a 790MB download mid-flight.
- **runsc version must match** across A and B (pinned in the image). Also CPU
  features must be compatible across nodes (keep one instance family, or set
  `dev.gvisor.internal.cpufeatures`).
- **Cache is per-worker-pod ephemeral fs today** — a worker rollout loses it and
  re-pulls 3.4GB. TODO: hostPath cache (see NETWORKING-LIFECYCLE.md).
- **Restart caveat**: kubelet does NOT manage the nested sandbox; sandboxd's
  supervisor does (liveness/readiness/restart). The worker pod itself is the
  kubelet-managed unit.
