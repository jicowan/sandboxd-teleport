# Installation guide — sandboxd control plane

How to deploy the sandboxd control plane onto an EKS cluster: the CRDs, RBAC,
Valkey, the operator, the router, worker Pod Identity for S3, and a first pool of
gVisor workers. When you finish you'll have a working control plane the broker can
front (see [admin-guide-broker.md](admin-guide-broker.md)).

Read [architecture-sandboxd.md](architecture-sandboxd.md) for what these
components do. All commands use the reference environment
(`EKSClusterStack-cluster`, `us-west-2`, account `820537372947`) — substitute your
own cluster, registry, bucket, and IAM ARNs.

Paths are relative to `checkpoint-restore/controlplane/` unless noted.

## Prerequisites

### Cluster & nodes

- An EKS cluster with **EKS Pod Identity** enabled (the `eks-pod-identity-agent`
  add‑on).
- A **gVisor node group** with `runsc` installed on‑node at `/usr/local/bin/runsc`,
  version **`release-20260622.0`** (must match the version baked into the worker
  image — gVisor hard‑errors on a restore version mismatch). Reference nodes are
  `c7a` (AMD) — a single instance family so CPU features match across nodes
  (gVisor requires the restore host to have all CPU features of the checkpoint
  host).
- Those nodes **labeled and tainted**:

  ```sh
  kubectl label node <node> sandbox=gvisor
  kubectl taint node <node> sandbox=gvisor:NoSchedule
  ```

- ~100Gi root disk per gVisor node (a browser‑class rootfs is ~9GB + checkpoints).

### Tooling

- `kubectl` pointed at the target cluster. **Always confirm context first:**

  ```sh
  kubectl config current-context
  # arn:aws:eks:us-west-2:820537372947:cluster/EKSClusterStack-cluster
  ```

- Go 1.26+, Docker (buildx, linux/amd64), `aws` CLI, and — for the operator/router
  images — [`ko`](https://ko.build). `kustomize` (or `make`, which vendors it).

### Container registry

An ECR (or other) registry for three images: `sandboxd-operator`,
`sandboxd-router` (both pure‑Go, built with `ko`), and `sandboxd` (the worker,
built with Docker because it ships the non‑Go `runsc` binary).

## Step 1 — S3 bucket + worker Pod Identity

Checkpoints live in S3; the worker reads/writes them via EKS Pod Identity.

1. **Create the bucket** (private):

   ```sh
   aws s3 mb s3://aio-checkpoint-spike-820537372947-us-west-2 --region us-west-2
   ```

2. **Create the worker IAM role** `aio-checkpoint-spike-role`:
   - Trust policy: principal `pods.eks.amazonaws.com`, actions `sts:AssumeRole` +
     `sts:TagSession`.
   - Inline policy `s3-checkpoints` (least privilege, scoped to the one bucket):
     - `s3:ListBucket` on `arn:aws:s3:::aio-checkpoint-spike-820537372947-us-west-2`
     - `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` on `…/*`

   > The worker keeps read/write; it does **not** need delete for normal operation
   > (delete is only used by the operator GC — see Step 6). You may drop
   > `s3:DeleteObject` from the worker role if you never call `/reset`‑style cleanup
   > that removes S3 objects.

3. **Create the ServiceAccount** the workers run as and **associate** the role.
   The reference uses SA `ckpt-spike` in namespace `default`:

   ```sh
   kubectl create serviceaccount ckpt-spike -n default

   aws eks create-pod-identity-association \
     --cluster-name EKSClusterStack-cluster \
     --namespace default --service-account ckpt-spike \
     --role-arn arn:aws:iam::820537372947:role/aio-checkpoint-spike-role \
     --region us-west-2
   ```

> **Why namespace `default`?** Pools/sessions/workers run in `default` in the
> reference deployment, so the operator is started with `--resume-namespace=default`
> and the worker SA + Pod Identity live there. If you use a different namespace,
> keep the SA, the pool objects, and `--resume-namespace` consistent.

## Step 2 — Build and push images

### Operator and router (`ko`)

```sh
# Operator
KO_DOCKER_REPO=<registry>/sandboxd-operator \
  ko build --bare --platform=linux/amd64 -t v1 ./cmd

# Router
KO_DOCKER_REPO=<registry>/sandboxd-router \
  ko build --bare --platform=linux/amd64 -t v1 ./cmd/router
```

(`ko` needs to be logged in to your registry; for ECR run `aws ecr
get-login-password | docker login …` first.)

### Worker (`sandboxd`, Docker)

The worker image bundles the pinned `runsc`. From `checkpoint-restore/sandboxd/`:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o sandboxd ./
curl -fsSL -o runsc \
  https://storage.googleapis.com/gvisor/releases/release/20260622.0/x86_64/runsc
chmod +x runsc
docker build --platform=linux/amd64 -t <registry>/sandboxd:v1 .
docker push <registry>/sandboxd:v1
```

The `runsc` version here **must** equal the version on your gVisor nodes.

## Step 3 — Install CRDs and the operator ClusterRole

The CRDs and the `manager-role` ClusterRole are applied separately from the smoke
deploy (which references but doesn't contain them).

```sh
# CRDs (SandboxTemplate, WarmPool, Session)
make install                       # = kustomize build config/crd | kubectl apply -f -
# or explicitly:
kubectl apply -f config/crd/bases/

# Operator ClusterRole (manager-role): pods get/list/watch/patch (patch = set
# pod-deletion-cost for graceful scale-in); deployments r/w; core.sandboxd.io
# templates r/o, sessions+warmpools r/w (+status/finalizers)
kubectl apply -f config/rbac/role.yaml
```

Verify:

```sh
kubectl get crd | grep core.sandboxd.io
# sandboxtemplates.core.sandboxd.io
# sessions.core.sandboxd.io
# warmpools.core.sandboxd.io
```

## Step 4 — Deploy Valkey, operator, and router

The self‑contained smoke manifest creates the namespace, operator SA, RBAC
bindings, Valkey, the operator, and the router. **Edit the image tags** in it to
match what you pushed in Step 2 (it ships `:v1` placeholders), then apply:

```sh
kubectl apply -f deploy/smoke/controlplane.yaml
```

What it creates in namespace `sandboxd-controlplane-system`:

| Object | Notes |
|--------|-------|
| Namespace `sandboxd-controlplane-system` | control‑plane namespace |
| ServiceAccount `sandboxd-operator` | operator identity |
| ClusterRoleBinding `sandboxd-operator` | binds `manager-role` (Step 3) to the SA |
| Role/RoleBinding `sandboxd-operator-leaderelection` | leases + events for leader election |
| Deployment/Service `valkey` | `valkey/valkey:8-alpine`, `:6379`, `--save "" --appendonly no --maxmemory-policy noeviction` |
| Deployment/Service `sandboxd-operator` | operator; `/resume` on `:8082`, health `:8081` |
| Deployment/Service `sandboxd-router` | router; `:8080` (`/mcp`, `/_warm`, `/healthz`) |

The operator is started with these args (already set in the manifest — adjust to
your bucket/region/SA/namespace):

```
--leader-elect
--health-probe-bind-address=:8081
--kv-addr=valkey:6379
--resume-addr=:8082
--resume-namespace=default
--worker-sa=ckpt-spike
--worker-bucket=aio-checkpoint-spike-820537372947-us-west-2
--worker-region=us-west-2
```

The router is started with:

```
--listen=:8080
--kv-addr=valkey:6379
--resume-url=http://sandboxd-operator:8082/resume
--worker-port=8090
```

> **Worker image.** The smoke manifest doesn't set `--worker-image`, so the
> operator uses its built‑in default. Set `--worker-image=<registry>/sandboxd:v1`
> (or `SANDBOXD_WORKER_IMAGE`) on the operator Deployment to point pools at the
> image you built, unless a pool overrides it per‑pool via
> `SandboxTemplate.workerImage`.

Verify:

```sh
kubectl get pods -n sandboxd-controlplane-system
# sandboxd-operator-…   1/1 Running
# sandboxd-router-…     1/1 Running
# valkey-…              1/1 Running
```

### Operator flags reference

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `--kv-addr` | `SANDBOXD_KV_ADDR` | `valkey:6379` | Valkey address. |
| `--resume-addr` | `SANDBOXD_RESUME_ADDR` | `:8082` | `/resume` listen addr. |
| `--resume-namespace` | `SANDBOXD_NAMESPACE` | `sandboxd-controlplane-system` | Namespace for templates/pools/sessions **and** worker pods. Set to `default` for the reference layout. |
| `--worker-sa` | `SANDBOXD_WORKER_SA` | `""` | ServiceAccount for worker pods (Pod Identity → S3). |
| `--worker-bucket` | `SANDBOXD_BUCKET` | `""` | S3 checkpoint bucket. |
| `--worker-region` | `AWS_REGION` | `""` | Region for worker S3. |
| `--worker-image` | `SANDBOXD_WORKER_IMAGE` | built‑in | Global worker image (per‑pool override via `SandboxTemplate.workerImage`). |
| `--max-concurrent-resumes` | `SANDBOXD_MAX_CONCURRENT_RESUMES` | `0` (unlimited) | Backpressure semaphore on concurrent resumes. |
| `--resume-deadline-seconds` | `SANDBOXD_RESUME_DEADLINE_SECONDS` | `90` | Resume/warm‑up deadline. Must exceed your image's cold start. |
| `--enable-checkpoint-gc` | `SANDBOXD_ENABLE_GC` (`=1`) | `false` | Enable snapshot GC (requires `--worker-bucket`). |
| `--checkpoint-gc-interval-seconds` | `SANDBOXD_GC_INTERVAL_SECONDS` | `300` | GC period. |
| `--leader-elect` | — | `false` | Leader election (enable for HA). |
| `--metrics-bind-address` | — | `0` (off) | Prometheus metrics addr. |

### Router flags reference

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `--listen` | `ROUTER_LISTEN` | `:8080` | Listen address. |
| `--kv-addr` | `SANDBOXD_KV_ADDR` | `valkey:6379` | Valkey address. |
| `--resume-url` | `SANDBOXD_RESUME_URL` | `http://sandboxd-controlplane-operator:8082/resume` | Operator `/resume` URL. **The smoke manifest overrides this to `http://sandboxd-operator:8082/resume`** to match the operator Service name — set it to match yours. |
| `--worker-port` | — | `8090` | sandboxd worker data port. |
| `--resume-deadline-seconds` | `SANDBOXD_RESUME_DEADLINE_SECONDS` | `90` | Time‑to‑first‑byte / warm‑up bound. |

## Step 5 — Create a pool of gVisor workers

Define a `SandboxTemplate` (what to run) and a `WarmPool` (how many workers). The
reference AIO pool is a ready example:

```sh
kubectl apply -f deploy/aio/aio-pool.yaml
```

It creates `SandboxTemplate/aio` (image `ghcr.io/agent-infra/sandbox:latest`, port
8080, health `/v1/health`, idle 600s, `streamConsole: true`, a per‑pool
`workerImage`) and `WarmPool/aio-pool` (`replicas: 4, minIdle: 2`), pinned to
gVisor nodes with node spread (`minDomains: 2`).

> The operator creates a `Deployment` of worker pods for the pool. Scheduling is
> **pass‑through** — the template must set `scheduling.nodeSelector: {sandbox:
> gvisor}` + the matching toleration, or workers won't land on gVisor nodes.

Verify workers come up and register as idle:

```sh
kubectl get pods -n default -l sandboxd.io/pool=aio-pool
kubectl get wp -n default
# NAME       REPLICAS   IDLE   BUSY
# aio-pool   4          2      0     (idle == replicas until sessions arrive)
```

See [admin-guide-crds.md](admin-guide-crds.md) for every template/pool field.

## Step 6 — (Optional) Snapshot GC

GC deletes expired/orphaned snapshots under a **separate, least‑privilege** S3
identity — list + delete on `sandboxes/*` only, so the "privileged worker + delete"
combination never coexists.

1. Create an operator GC IAM role granting only `s3:ListBucket` +
   `s3:DeleteObject` on `sandboxes/*` of the bucket, trust `pods.eks.amazonaws.com`.
   (No manifest ships for this — create it out‑of‑band.)
2. Associate it to the **operator's** ServiceAccount (`sandboxd-operator`) via a
   Pod Identity association.
3. Enable GC on the operator: add `--enable-checkpoint-gc` (needs
   `--worker-bucket`), optionally tune `--checkpoint-gc-interval-seconds`.

GC is **off by default**.

## Step 7 — Smoke test

Exercise the control plane end‑to‑end without the broker, using the `/_warm`
primitive and an in‑cluster client. From a debug pod in `default`:

```sh
# Warm (resume/cold-start) a session onto the pool:
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  -H 'X-Session-ID: sess-smoketest' \
  -H 'X-Session-Pool: aio-pool' \
  http://sandboxd-router.sandboxd-controlplane-system:8080/_warm
# expect 204 (or 503 Retry-After if the pool has no idle worker)

# Then confirm the session is Running on a worker:
kubectl get sess -n default sess-smoketest
```

Then send a real MCP call to `…:8080/mcp` with the same headers (see the runbook
for a full walkthrough). For the full user‑facing path, deploy the broker,
agentgateway, and Keycloak — [admin-guide-broker.md](admin-guide-broker.md).

## Upgrades

- **Operator / router:** build a new tag with `ko`, then
  `kubectl set image deploy/sandboxd-operator -n sandboxd-controlplane-system
  operator=<registry>/sandboxd-operator:<tag>` (container name is `operator`;
  router's is `router`).
- **Worker image:** bump the global `--worker-image` (rolls all pools) or a pool's
  `SandboxTemplate.workerImage` (rolls one pool). After a template change, nudge the
  pool: `kubectl annotate warmpool <pool> -n default sandboxd.io/nudge="$(date +%s)"
  --overwrite`.
- **CRD schema changes:** re‑apply `config/crd/bases/` (or `make install`) **and**
  `config/rbac/role.yaml` if RBAC changed — forgetting the RBAC re‑apply is a
  known footgun (Session creation gets `forbidden`).
- Always **reuse a fresh image tag** — reusing a tag with `imagePullPolicy:
  IfNotPresent` serves a stale cached image on the node.

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| Workers stay `Pending` | Template scheduling doesn't tolerate/select gVisor nodes. | Set `scheduling.nodeSelector {sandbox: gvisor}` + toleration on the template. |
| `WarmPool` shows fewer idle than expected | Stale KV worker entries after restarts, or capacity in use. | The 30s prune loop reconciles; check `kubectl get pods -l sandboxd.io/pool=…`. |
| Resume `503 Retry-After` | No idle worker (pool saturated). | Raise `replicas`/`minIdle`. |
| Resume times out | `--resume-deadline-seconds` shorter than the image cold start. | Raise it above your cold‑start time. |
| Session create `forbidden` | `manager-role` ClusterRole not (re)applied after a CRD/RBAC change. | `kubectl apply -f config/rbac/role.yaml`. |
| Checkpoint/restore returns `503 s3 not configured` | Worker has no `SANDBOXD_BUCKET`, or Pod Identity not associated. | Set `--worker-bucket`; verify the SA↔role association. |
| Restore fails with runsc version mismatch | Worker image `runsc` ≠ node `runsc`. | Rebuild the worker image against the node's pinned version. |
