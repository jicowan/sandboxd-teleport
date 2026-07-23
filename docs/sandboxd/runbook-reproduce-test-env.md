# Runbook — reproduce the test environment and run a sample container

An end‑to‑end walkthrough that stands up the sandboxd test environment from
scratch and runs a sample container as a nested gVisor sandbox — proving both the
**direct worker path** (sandboxd HTTP API) and the **full control‑plane path**
(router → operator → worker), including a checkpoint→S3→restore teleport.

This is the "does it all actually work" runbook. For conceptual background see
[architecture-sandboxd.md](architecture-sandboxd.md); for install detail see
[install-guide-sandboxd.md](install-guide-sandboxd.md); for the worker API see
[api-reference-sandboxd-worker.md](api-reference-sandboxd-worker.md).

Reference environment: cluster `EKSClusterStack-cluster`, `us-west-2`, account
`111122223333`, bucket `aio-checkpoint-spike-111122223333-us-west-2`. Substitute
your own.

---

## 0. Confirm your context first

The single most important habit — everything below targets a specific cluster:

```sh
kubectl config current-context
# arn:aws:eks:us-west-2:111122223333:cluster/EKSClusterStack-cluster
```

If it's not the sandboxd cluster, switch before doing anything:

```sh
kubectl config use-context arn:aws:eks:us-west-2:111122223333:cluster/EKSClusterStack-cluster
```

## 1. Prerequisites checklist

- [ ] gVisor node group: nodes labeled `sandbox=gvisor` + taint
      `sandbox=gvisor:NoSchedule`, `runsc release-20260622.0` on‑node, ~100Gi root
      disk, single instance family (e.g. `c7a`) so CPU features match.
- [ ] S3 bucket + worker Pod Identity (SA `default/sandboxd-worker` → role
      `sandboxd-worker-checkpoint`, scoped to the bucket). See install guide Step 1.
- [ ] Registry access for `sandboxd`, `sandboxd-operator`, `sandboxd-router`.
- [ ] Local tools: `go` 1.26+, `docker` (buildx linux/amd64), `ko`, `kubectl`,
      `aws`.

Quick node check:

```sh
kubectl get nodes -l sandbox=gvisor
```

---

## Part A — Direct worker path (no control plane)

Proves the core primitive: run a container as a nested gVisor sandbox on a worker,
checkpoint it to S3, and restore it onto a **second** worker with state intact.
This is the fastest way to validate a cluster before layering the control plane on.

### A1. Build & push the worker image

From `checkpoint-restore/sandboxd/`:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o sandboxd ./
curl -fsSL -o runsc \
  https://storage.googleapis.com/gvisor/releases/release/20260622.0/x86_64/runsc
chmod +x runsc
aws ecr get-login-password --region us-west-2 | \
  docker login --username AWS --password-stdin 111122223333.dkr.ecr.us-west-2.amazonaws.com
docker build --platform=linux/amd64 -t 111122223333.dkr.ecr.us-west-2.amazonaws.com/sandboxd:test .
docker push 111122223333.dkr.ecr.us-west-2.amazonaws.com/sandboxd:test
rm -f sandboxd runsc   # don't commit the ~130MB binaries
```

`runsc` in the image **must** match the node version, or restore fails.

### A2. Deploy two workers (A and B)

`checkpoint-restore/sandboxd/worker-deploy.yaml` is a 2‑replica Deployment
(privileged, SA `sandboxd-worker`, gVisor node selector/toleration, port `8090`, S3 env,
`SANDBOXD_POD_IP` from the downward API). Point it at your image tag and apply:

```sh
kubectl apply -f checkpoint-restore/sandboxd/worker-deploy.yaml
kubectl rollout status deploy/sandboxd-worker -n default
kubectl get pods -n default -l app=sandboxd-worker -o wide
```

Each worker logs `networking: interior netns ready (podIP=…, interiorIP=169.254.17.2)`.
Capture the two pod IPs (call them `WAIP`, `WBIP`), and either port‑forward or use
an in‑cluster debug pod to reach `:8090`. Examples below use a debug pod:

```sh
kubectl run toolbox --image=curlimages/curl -n default --command -- sleep infinity
```

### A3. Run a sample container on worker A

Use a small, obviously‑stateful sample (busybox writing a counter). Send `/run`
to worker A:

```sh
kubectl exec toolbox -n default -- sh -c '
curl -s -X POST http://'"$WAIP"':8090/run -H "Content-Type: application/json" -d "{
  \"sandboxId\": \"demo\",
  \"image\": \"docker.io/library/busybox:latest\",
  \"cmd\": [\"sh\",\"-c\",\"i=0; while true; do echo tick_$i; echo $i > /state; i=$((i+1)); sleep 2; done\"]
}"'
# → {"sandboxId":"demo","status":"running","image":"…","ports":null}
```

Confirm it's running and let it accumulate state:

```sh
kubectl exec toolbox -n default -- curl -s "http://$WAIP:8090/status?sandboxId=demo"
# → {"sandboxId":"demo","status":"running","ready":…,"idle":…,"restarts":0}
```

> For a network‑exposed workload (like the AIO image), add
> `"ports":[{"container":8080,"host":8080}]` and a `health` block; the worker sets
> up the veth + nftables DNAT so `WAIP:8080` reaches the sandbox. AIO cold start is
> ~40–45s (image pull + boot).

### A4. Checkpoint worker A → S3

```sh
kubectl exec toolbox -n default -- sh -c '
curl -s -X POST http://'"$WAIP"':8090/checkpoint -H "Content-Type: application/json" \
  -d "{\"sandboxId\":\"demo\"}"'
# → {"sandboxId":"demo","snapshot":"sandboxes/demo/snap-…","sizeBytes":…,"image":"…",…}
```

Note the `snapshot` prefix (call it `SNAP`). The S3 objects are `checkpoint.img`,
`pages.img`, `pages_meta.img`, `config.json` under
`s3://<bucket>/sandboxes/demo/snap-…/`. (`/checkpoint` leaves the sandbox in place;
`/suspend` would also free worker A.)

### A5. Restore onto worker B (teleport)

```sh
kubectl exec toolbox -n default -- sh -c '
curl -s -X POST http://'"$WBIP"':8090/restore -H "Content-Type: application/json" -d "{
  \"sandboxId\": \"demo-b\",
  \"image\": \"docker.io/library/busybox:latest\",
  \"snapshot\": \"'"$SNAP"'\"
}"'
# → {"sandboxId":"demo-b","status":"running","restoredFrom":"sandboxes/demo/snap-…",…}
```

State survived: the restored process continues its counter from where A left off
(RAM), and `/state` holds the last value it wrote (filesystem) — on a **different
worker**, proving teleport. For a workload with a port, hit `WBIP:<hostPort>` and
confirm it responds.

### A6. Clean up Part A

```sh
kubectl exec toolbox -n default -- curl -s -X POST http://$WAIP:8090/reset -d '{"sandboxId":"demo"}'
kubectl exec toolbox -n default -- curl -s -X POST http://$WBIP:8090/reset -d '{"sandboxId":"demo-b"}'
kubectl delete -f checkpoint-restore/sandboxd/worker-deploy.yaml
```

---

## Part B — Full control‑plane path

Now stand up the operator, router, Valkey, and a pool, and drive a session through
the router exactly as the broker would.

> Every command here runs from the in‑cluster debug pod
> (`kubectl exec toolbox`) against ClusterIP Service DNS (the router, Valkey) and
> worker pod IPs — all cluster‑internal.

### B1. Install the control plane

Follow [install-guide-sandboxd.md](install-guide-sandboxd.md) Steps 2–5:

```sh
# CRDs + operator ClusterRole
make -C checkpoint-restore/controlplane install
kubectl apply -f checkpoint-restore/controlplane/config/rbac/role.yaml

# Valkey + operator + router (edit image tags first)
kubectl apply -f checkpoint-restore/controlplane/deploy/smoke/controlplane.yaml
kubectl -n sandboxd-controlplane-system rollout status deploy/sandboxd-operator
kubectl -n sandboxd-controlplane-system rollout status deploy/sandboxd-router

# A generic pool of gVisor workers + the AppTemplates it runs
kubectl apply -f checkpoint-restore/controlplane/deploy/aio/generic-pool.yaml
```

This is a **generic pool** (`aio-generic-pool`): its SandboxTemplate carries only
worker‑shape, so a session brings its own workload via an AppTemplate
(`X-Session-App`). That's the model the rest of these docs lead with. (Prefer a
dedicated, single‑image pool? `deploy/aio/aio-pool.yaml` is a ready example — apply
it instead, then drop `X-Session-App` and use `X-Session-Pool: aio-pool` below.)

Confirm the control plane and pool are healthy:

```sh
kubectl get pods -n sandboxd-controlplane-system
kubectl get wp -n default
# NAME               REPLICAS   IDLE   BUSY
# aio-generic-pool   2          1      0
kubectl get pods -n default -l sandboxd.io/pool=aio-generic-pool
```

### B1.5. (Optional) Secure the control plane with SPIFFE/SPIRE mTLS

By default the control-plane hops are plain HTTP (in-cluster reachability only). To
mutually authenticate the **router→operator** and **operator→worker** hops with SPIFFE
mTLS — so only the workload with the expected SPIFFE ID can call each — install SPIRE
and enable mTLS **before** running sessions. This is opt-in; skip it to run plain.

```sh
helm repo add spiffe https://spiffe.github.io/helm-charts-hardened/ && helm repo update spiffe
helm install spire-crds spiffe/spire-crds --version 0.5.0 -n spire-system --create-namespace
helm install spire      spiffe/spire      --version 0.29.0 -n spire-system \
  -f checkpoint-restore/controlplane/deploy/spire/values.yaml
kubectl apply -f checkpoint-restore/controlplane/deploy/spire/clusterspiffeids.yaml

# enable mTLS on the operator + router, then roll the pool to get mTLS workers
kubectl patch deploy sandboxd-operator -n sandboxd-controlplane-system \
  --patch-file checkpoint-restore/controlplane/deploy/spire/controlplane-mtls-patch.yaml
kubectl patch deploy sandboxd-router -n sandboxd-controlplane-system \
  --patch-file checkpoint-restore/controlplane/deploy/spire/router-mtls-patch.yaml
kubectl annotate warmpool aio-generic-pool -n default sandboxd.io/nudge="$(date +%s)" --overwrite
```

Full guide (registration, verification, troubleshooting, rollback):
[security-spiffe-spire.md](security-spiffe-spire.md). The rest of Part B works
identically with mTLS on.

### B2. Warm a session through the router

`/_warm` is a protocol‑agnostic router primitive: it resumes/cold‑starts the
session onto the pool and returns `204` — no payload. (The broker doesn't use this
path; it warms by transparently forwarding the MCP `initialize`, as in B3. `/_warm`
is handy here to warm the session in isolation before making a real MCP call.)

On a generic pool, `X-Session-App` names the AppTemplate (workload) the session
should run; `X-Session-Pool` names the pool (capacity). The operator lazily creates
the `Session` (poolRef + appRef) from these hints on first contact:

```sh
kubectl exec toolbox -n default -- sh -c '
curl -s -o /dev/null -w "%{http_code}\n" -X POST \
  -H "X-Session-ID: sess-demo" \
  -H "X-Session-Pool: aio-generic-pool" \
  -H "X-Session-App: aio-app" \
  http://sandboxd-router.sandboxd-controlplane-system:8080/_warm'
# → 204  (or 503 if the pool has no idle worker → raise replicas/minIdle)
```

Confirm the session went Running and got a worker (the operator lazily created the
`Session` object from the pool + app hints):

```sh
kubectl get sess -n default sess-demo
# NAME        PHASE     WORKER
# sess-demo   Running   10.0.x.y
kubectl get wp -n default          # busy should be ≥ 1
```

### B3. Make a real MCP call through the router (fast path)

Now the session is Running on a live worker, so this exercises the router's
**fast path** (with worker‑liveness fencing) and proxies to the sandbox's workload
port:

```sh
kubectl exec toolbox -n default -- sh -c '
curl -s -X POST http://sandboxd-router.sandboxd-controlplane-system:8080/mcp \
  -H "X-Session-ID: sess-demo" \
  -H "X-Session-Pool: aio-generic-pool" \
  -H "X-Session-App: aio-app" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{},\"clientInfo\":{\"name\":\"runbook\",\"version\":\"1\"}}}"'
# → 200 with a real MCP initialize result (serverInfo: Sandbox MCP Tools …)
```

### B4. Prove teleport through the control plane

Write a marker inside the sandbox (via an MCP tool call), then force a
suspend→resume and confirm the marker survives on a possibly‑different worker.

1. Write a marker (power‑tier tool; adjust to your authorization):

   ```sh
   kubectl exec toolbox -n default -- sh -c '
   curl -s -X POST http://sandboxd-router.sandboxd-controlplane-system:8080/mcp \
     -H "X-Session-ID: sess-demo" -H "X-Session-Pool: aio-generic-pool" -H "X-Session-App: aio-app" \
     -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" \
     -d "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"sandbox_execute_bash\",\"arguments\":{\"cmd\":\"echo teleport-proof-$(date +%s) | tee /tmp/marker.txt\"}}}"'
   ```

2. Suspend the session (checkpoint→S3, free the worker). Either wait out the
   template idle timeout, or suspend the worker directly (find the worker holding
   the session, then `POST /suspend`):

   ```sh
   # Which worker holds sess-demo:
   kubectl exec toolbox -n default -- redis-cli -h valkey.sandboxd-controlplane-system \
     GET "session:sess-demo"     # note workerPod / workerPodIP
   # Suspend it via that worker's API (WIP = its podIP):
   kubectl exec toolbox -n default -- curl -s -X POST http://$WIP:8090/suspend -d '{"sandboxId":"sess-demo"}'
   ```

   The session goes `Suspended`; state lives only in S3.

3. Resume by simply calling `/mcp` again — the router teleports the session onto
   any idle worker by restoring the snapshot:

   ```sh
   kubectl exec toolbox -n default -- sh -c '
   curl -s -X POST http://sandboxd-router.sandboxd-controlplane-system:8080/mcp \
     -H "X-Session-ID: sess-demo" -H "X-Session-Pool: aio-generic-pool" -H "X-Session-App: aio-app" \
     -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" \
     -d "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"sandbox_execute_bash\",\"arguments\":{\"cmd\":\"cat /tmp/marker.txt\"}}}"'
   # → the SAME teleport-proof-… marker, on a possibly different worker
   ```

   Check `kubectl get sess -n default sess-demo` — `WORKER` may now be a different
   pod IP, and the marker survived: **teleport proven through the full stack.**

### B5. (Optional) Observe the sandbox console

If the pool's template has `streamConsole: true`, the nested workload's own
stdout/stderr appears in the worker's logs:

```sh
kubectl logs -n default <worker-pod-holding-the-session> | grep '\[sandbox'
```

### B6. Clean up Part B

```sh
kubectl exec toolbox -n default -- redis-cli -h valkey.sandboxd-controlplane-system DEL "session:sess-demo"
kubectl delete -f checkpoint-restore/controlplane/deploy/aio/generic-pool.yaml
# (leave the control plane running, or delete deploy/smoke/controlplane.yaml too)
kubectl delete pod toolbox -n default
```

---

## Part C — The full user‑facing path (optional)

To reproduce what an end user sees, add the front door and connect a real client:

1. Deploy Keycloak realm, agentgateway (multi‑app routes), the sandboxd broker, and
   the generic pool + AppTemplates — [admin-guide-broker.md](admin-guide-broker.md).
2. Ensure the `aio-sandbox-broker-svc` Service selects the sandboxd broker
   (`app: aio-sandbox-broker-sandboxd`).
3. Connect Claude Code to a per‑app endpoint, e.g.
   `https://<your-gateway-host>/aio/mcp` (or `/everything/mcp`), and authenticate —
   [end-user-guide-broker.md](end-user-guide-broker.md).
4. Ask Claude to run a tool; confirm it executes in your sandbox and that state
   persists across a reconnect.

---

## Gotchas learned (save yourself the debugging)

- **Wrong kube context** is the #1 time‑waster — verify it before every session.
- **Reused image tags** with `imagePullPolicy: IfNotPresent` serve a stale cached
  image on the node. Always bump the tag.
- **runsc version mismatch** (image vs. node) → restore hard‑fails. Keep them equal.
- **CPU‑feature match** — restore host must have all CPU features of the checkpoint
  host. Pin one instance family.
- **`--network=sandbox`, not host** — host networking isn't checkpointable; the
  worker uses the veth/netns netstack with a stable interior IP `169.254.17.2`.
- **Sockets don't survive restore** — every resume is a fresh MCP session
  (reconnect). The address is stable; the connection is not.
- **`minIdle` sizing** — a saturated pool makes the first user eat a cold start and
  can `503`, which poisons an MCP client's tool list. Keep `minIdle` ≥ expected
  concurrent new sessions.
- **Editing a template doesn't roll the pool** — nudge it:
  `kubectl annotate warmpool <pool> -n default sandboxd.io/nudge="$(date +%s)" --overwrite`.
- **RBAC re‑apply** — after CRD/RBAC changes, re‑apply `config/rbac/role.yaml` or
  Session creation gets `forbidden`.
- **TTY banners pollute output** — prefer a persistent debug pod (`kubectl exec`)
  over `kubectl run -it` for scripted curl/redis‑cli.
