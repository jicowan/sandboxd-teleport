# Installation guide — sandboxd control plane

How to deploy the sandboxd control plane onto an EKS cluster: the CRDs, RBAC,
Valkey, the operator, the router, worker Pod Identity for S3, and a first pool of
gVisor workers. When you finish you'll have a working control plane the broker can
front (see [admin-guide-broker.md](admin-guide-broker.md)).

Read [architecture-sandboxd.md](architecture-sandboxd.md) for what these
components do. All commands use the reference environment
(`EKSClusterStack-cluster`, `us-west-2`, account `111122223333`) — substitute your
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
  # arn:aws:eks:us-west-2:111122223333:cluster/EKSClusterStack-cluster
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
   aws s3 mb s3://aio-checkpoint-spike-111122223333-us-west-2 --region us-west-2
   ```

2. **Create the worker IAM role** `sandboxd-worker-checkpoint`:
   - Trust policy: principal `pods.eks.amazonaws.com`, actions `sts:AssumeRole` +
     `sts:TagSession`.
   - Inline policy `s3-checkpoints` (least privilege, scoped to the one bucket):
     - `s3:ListBucket` on `arn:aws:s3:::aio-checkpoint-spike-111122223333-us-west-2`
     - `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` on `…/*`

   > The worker keeps read/write; it does **not** need delete for normal operation
   > (delete is only used by the operator GC — see Step 6). You may drop
   > `s3:DeleteObject` from the worker role if you never call `/reset`‑style cleanup
   > that removes S3 objects.

   **Private‑registry (ECR) image pulls.** The worker pulls the *workload* image
   (the sandbox's OCI image) directly via the node containerd API, authenticating
   with the worker's Pod Identity. Public images (ghcr/Docker Hub) and images the
   kubelet already cached pull anonymously, but a **private ECR** workload image
   needs pull permission on this role. If any `SandboxTemplate.image` or
   `AppTemplate.image` you run is a private ECR repo, add an inline policy
   `ecr-pull`:

   - `ecr:GetAuthorizationToken` on `*` (AWS requires `*` for this action)
   - `ecr:BatchGetImage`, `ecr:GetDownloadUrlForLayer`,
     `ecr:BatchCheckLayerAvailability` on
     `arn:aws:ecr:us-west-2:111122223333:repository/*` (or scope to specific repos)

   > The worker's containerd pull uses an ECR authorizer only for `*.dkr.ecr.*`
   > image refs (a token fetched via this role); non‑ECR hosts stay anonymous, so
   > public images are unaffected. Without this policy, a private‑ECR workload image
   > fails at `/run` with a containerd `401 Unauthorized` on the manifest HEAD. 
   > Authenticating to other private registries, such as Arifactory or Harbor, is not
   > currently supported (feat). 

3. **Create the ServiceAccount** the workers run as and **associate** the role.
   The reference environment uses SA `sandboxd-worker` in namespace `default`:

   ```sh
   kubectl create serviceaccount sandboxd-worker -n default

   aws eks create-pod-identity-association \
     --cluster-name EKSClusterStack-cluster \
     --namespace default --service-account sandboxd-worker \
     --role-arn arn:aws:iam::111122223333:role/sandboxd-worker-checkpoint \
     --region us-west-2
   ```

> **Why namespace `default`?** Pools/sessions/workers run in `default` in the
> reference environment, so the operator is started with `--resume-namespace=default`
> and the worker SA + Pod Identity live there. If you elect to use a different namespace,
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

In this step you register sandboxd's **CustomResourceDefinitions** (the API types the
operator reconciles — `SandboxTemplate`, `AppTemplate`, `WarmPool`, `Session`,
`ForkSet`, `BaseSnapshot`) and the operator's **`manager-role` ClusterRole** (the
permission *definition* — get/list/watch/patch pods, read/write Deployments and the
`core.sandboxd.io` resources). Both are **cluster‑scoped**: the CRDs extend the
Kubernetes API for the whole cluster, and a ClusterRole is not namespaced — so they
are installed once, up front, independent of any namespace or release.

> **Role here, binding in Step 4.** This step creates only the ClusterRole (the set of
> permissions). The operator's **ServiceAccount** and the **ClusterRoleBinding** that
> grants it that role — plus a namespaced leader‑election `Role`/`RoleBinding` — are
> created by the smoke deploy in Step 4 (they're namespace/deployment‑specific). So a
> ClusterRole exists after this step, but nothing is *bound* to it yet; the operator
> gets its permissions when Step 4 creates the SA + binding.

This is a **separate step** because the CRDs and ClusterRole are **not** part of the
smoke deploy (below) — they're cluster‑scoped, generated artifacts
(`config/crd/bases/`, `config/rbac/role.yaml`) applied on their own, and the operator
can't start without them (it reconciles those CRDs, and Step 4's binding points at
this ClusterRole).

```sh
# CRDs: SandboxTemplate, AppTemplate, WarmPool, Session, ForkSet, BaseSnapshot
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
# apptemplates.core.sandboxd.io
# basesnapshots.core.sandboxd.io
# forksets.core.sandboxd.io
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
--worker-sa=sandboxd-worker
--worker-bucket=aio-checkpoint-spike-111122223333-us-west-2
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
| `--cred-token-secret` | `SANDBOXD_CRED_TOKEN_SECRET` | `""` | Secret name (key `token`) with the fleet HMAC key for per‑session IAM credentials; empty disables the vendor. See Step 6b. |
| `--max-concurrent-resumes` | `SANDBOXD_MAX_CONCURRENT_RESUMES` | `0` (unlimited) | Backpressure semaphore on concurrent resumes. |
| `--resume-deadline-seconds` | `SANDBOXD_RESUME_DEADLINE_SECONDS` | `90` | Resume/warm‑up deadline. Must exceed your image's cold start. |
| `--sweep-interval-seconds` | `SANDBOXD_SWEEP_INTERVAL_SECONDS` | `30` | Idle‑suspend / periodic‑checkpoint sweep period. Sweeps are O(due) (indexed), so this is a granularity knob, not a scan‑cost one; the checkpoint sweep runs a half‑interval offset to avoid lockstep. |
| `--enable-checkpoint-gc` | `SANDBOXD_ENABLE_GC` (`=1`) | `false` | Enable session GC — TTL + abandoned + orphan S3/CR reaping (requires `--worker-bucket`). |
| `--checkpoint-gc-interval-seconds` | `SANDBOXD_GC_INTERVAL_SECONDS` | `300` | GC sweep period. |
| `--gc-dry-run` | `SANDBOXD_GC_DRY_RUN` (`=1`) | `true` | GC classifies + records candidates (logs + `sandboxd_gc_candidates` / `sandboxd_gc_reaped_total`) but **mutates nothing**. Set to `0` to arm actual reaping. |
| `--default-ttl-after-suspend-seconds` | `SANDBOXD_DEFAULT_TTL_AFTER_SUSPEND_SECONDS` | `0` | Default retention for a Suspended session's checkpoint when the `Session` sets no `ttlAfterSuspendSeconds`. `0` = keep forever. |
| `--abandoned-grace-seconds` | `SANDBOXD_ABANDONED_GRACE_SECONDS` | `3600` | How long a non‑Suspended session (or orphan CR) must look dead before GC reaps it. `0` disables the abandoned + orphan‑CR passes. |
| `--leader-elect` | — | `false` | Leader election (enable for HA). |
| `--metrics-bind-address` | — | `0` (off) | Prometheus metrics addr. Enable to export `sandboxd_*` metrics (resumes, suspends, pool workers, sweep duration/due, GC candidates/reaped). |
| `--mtls` | `SANDBOXD_MTLS` (`=1`) | `false` | Enable SPIFFE mTLS on the control hops (router→operator `/resume`, operator→worker). Requires SPIRE + the Workload API socket. Also injects mTLS into provisioned worker pods. See [security-spiffe-spire.md](security-spiffe-spire.md). |
| `--spiffe-socket` | `SPIFFE_ENDPOINT_SOCKET` | CSI default | SPIRE Workload API socket. |
| `--spiffe-router-id` | `SANDBOXD_SPIFFE_ROUTER_ID` | `spiffe://sandboxd/router` | SPIFFE ID authorized as the `/resume` caller. |
| `--spiffe-worker-id` | `SANDBOXD_SPIFFE_WORKER_ID` | `spiffe://sandboxd/worker` | SPIFFE ID authorized when calling workers. |

### Router flags reference

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `--listen` | `ROUTER_LISTEN` | `:8080` | Listen address. |
| `--kv-addr` | `SANDBOXD_KV_ADDR` | `valkey:6379` | Valkey address. |
| `--resume-url` | `SANDBOXD_RESUME_URL` | `http://sandboxd-controlplane-operator:8082/resume` | Operator `/resume` URL. **The smoke manifest overrides this to `http://sandboxd-operator:8082/resume`** to match the operator Service name — set it to match yours. **With `--mtls`, use `https://`.** |
| `--worker-port` | — | `8090` | sandboxd worker data port. |
| `--resume-deadline-seconds` | `SANDBOXD_RESUME_DEADLINE_SECONDS` | `90` | Time‑to‑first‑byte / warm‑up bound. |
| `--mtls` | `SANDBOXD_MTLS` (`=1`) | `false` | Enable SPIFFE mTLS for the router→operator `/resume` hop. Requires SPIRE + the Workload API socket. |
| `--spiffe-socket` | `SPIFFE_ENDPOINT_SOCKET` | CSI default | SPIRE Workload API socket. |
| `--spiffe-operator-id` | `SANDBOXD_SPIFFE_OPERATOR_ID` | `spiffe://sandboxd/operator` | SPIFFE ID the router authorizes when calling `/resume`. |

## Step 5 — Create a pool of gVisor workers

A pool is a `WarmPool` (how many workers) bound to a `SandboxTemplate` (the pool's
**worker‑shape**: scheduling, resources, `workerImage`). Whether that template pins an
`image` decides the pool's character:

- **Generic pool (recommended)** — template has **no image** (worker‑shape only). It
  runs whatever workload a session brings via `spec.appRef` (an `AppTemplate`), so many
  apps share one pool. This is the model the multi‑app front door uses.
- **Dedicated pool** — template pins one `image`; the pool runs only that image
  (`poolRef`‑only sessions). Use it when one image earns its own warm fleet.

Deploy the reference **generic pool** (it also defines the example `AppTemplate`s):

```sh
kubectl apply -f deploy/aio/generic-pool.yaml
```

It creates:
- `SandboxTemplate/aio-generic` — **no image**; worker‑shape only (`workerImage`,
  `resources`, `scheduling` pinned to gVisor nodes), so it's a generic pool.
- `WarmPool/aio-generic-pool` — `replicas: 2, minIdle: 1`.
- `AppTemplate/aio-app` — the AIO sandbox (`ghcr.io/agent-infra/sandbox:latest`, port
  8080, health `/v1/health`, idle 600s).
- `AppTemplate/everything-app` — a second, distinct MCP server (private‑ECR image;
  needs the worker `ecr-pull` policy from Step 1).

> The operator creates a `Deployment` of worker pods for the pool. Scheduling is
> **pass‑through** — the template must set `scheduling.nodeSelector: {sandbox: gvisor}`
> + the matching toleration, or workers won't land on gVisor nodes. A generic pool's
> template still needs a resolvable `workerImage` (or the operator's global
> `--worker-image`) even though it has no *workload* image.

Verify workers come up and register as idle:

```sh
kubectl get pods -n default -l sandboxd.io/pool=aio-generic-pool
kubectl get wp -n default
# NAME               REPLICAS   IDLE   BUSY
# aio-generic-pool   2          1      0     (idle == replicas until sessions arrive)
```

> **Prefer a dedicated pool instead?** `deploy/aio/aio-pool.yaml` is a ready example:
> `SandboxTemplate/aio` (pins the AIO image) + `WarmPool/aio-pool`. A `poolRef`‑only
> session on it runs the AIO image directly (no `appRef` needed).

To run a workload on a generic pool, a `Session` names both a pool (capacity) and an
app (workload): `spec.poolRef: {name: aio-generic-pool}` + `spec.appRef: {name:
aio-app}`. The front door, i.e. the broker, sets both of these for you; see
[howto-add-an-app.md](howto-add-an-app.md) to add more apps. See
[admin-guide-crds.md](admin-guide-crds.md) for every field.

## Step 6 — (Optional) Session GC

GC reaps a dead session's **whole footprint** — the S3 snapshot, the Valkey
`session:*` entry (+ its due‑index membership), and the `Session` CR — across every
way a session goes dead. Four passes:

- **TTL** — a `Suspended` session whose checkpoint is older than its retention
  (per‑session `ttlAfterSuspendSeconds`, else `--default-ttl-after-suspend-seconds`).
- **Abandoned** — a non‑`Suspended` entry whose bound worker is gone / no longer
  holds it (the same `workerHolds` fence the router uses), idle past
  `--abandoned-grace-seconds`. Catches zombie‑`Running` entries pointing at a dead
  worker.
- **Orphan‑S3** — a `sandboxes/<sid>/` snapshot prefix referenced by no session.
- **Orphan‑CR** — an operator‑owned `Session` CR with a dead phase and no KV entry.

CR deletion is **ownership‑aware**: only CRs the operator lazily created (labeled
`sandboxd.io/created-by=operator`) are deleted; a **user‑declared** `Session` is only
tombstoned to `Absent`, never deleted.

S3 deletes run under a **separate, least‑privilege** S3 identity — list + delete on
`sandboxes/*` only — so the "privileged worker + delete" combination never coexists.

1. Create an operator GC IAM role granting only `s3:ListBucket` +
   `s3:DeleteObject` on `sandboxes/*` of the bucket, trust `pods.eks.amazonaws.com`.
   (No manifest ships for this — create it out‑of‑band.)
2. Associate it to the **operator's** ServiceAccount (`sandboxd-operator`) via a
   Pod Identity association. (CR deletes use the operator's existing RBAC, which
   already grants `delete` on `sessions` — no change needed.)
3. Enable GC on the operator: add `--enable-checkpoint-gc` (needs `--worker-bucket`),
   optionally tune `--checkpoint-gc-interval-seconds`,
   `--default-ttl-after-suspend-seconds`, and `--abandoned-grace-seconds`.

GC is **off by default**, and when enabled it starts in **dry‑run**
(`--gc-dry-run=true`): it logs and counts what it *would* reap (per‑class candidate
counts + the `sandboxd_gc_candidates` gauge) but deletes nothing. **Validate the
classification against your fleet first** — watch the `gc-sweeper` "would reap
session footprint" log line and confirm the per‑class counts match live/dead
sessions — then arm reaping with `SANDBOXD_GC_DRY_RUN=0` (or `--gc-dry-run=false`).

> **Setting a default TTL** (`--default-ttl-after-suspend-seconds`) is what makes the
> TTL pass actually fire — with the default `0` (keep forever), suspended‑session
> snapshots are retained indefinitely. Pick a retention that matches your
> come‑back‑and‑resume promise (e.g. `604800` = 7 days).

## Step 6b — (Optional) Per‑session IAM credentials for sandboxes

Let sandboxes assume an AWS IAM role scoped to their session (standard AWS SDK,
no workload code change), teleport‑safe and never the worker's own identity. Off
unless configured. See [PRD-sandbox-iam-credentials.md](../PRD-sandbox-iam-credentials.md).

> **Why an HMAC key?** The worker runs a tiny credential vendor on the sandbox's
> interior network; the sandbox's AWS SDK fetches its session credentials from it,
> presenting a bearer token. That token is **HMAC(key, sid)** — a keyed hash
> (**HMAC** = Hash‑based Message Authentication Code, here HMAC‑SHA256) of the session
> id under a fleet‑wide secret **key**. It does two jobs:
> - **Authorization** — only something holding the key can produce the right token for
>   a given `sid`, so one sandbox can't guess another session's token and steal its
>   credentials (even co‑tenant on the same worker).
> - **Teleport‑safety** — because the token is a *deterministic* function of
>   `(key, sid)` and **every worker shares the same key**, any worker recomputes the
>   same token. The token is baked into the checkpoint, so after a session restores on
>   a *different* worker it still matches — AWS access resumes with no handoff or
>   re‑issue. (A random token couldn't do this; it would have to be persisted and
>   transferred.) The Secret below holds only this shared key — not the AWS
>   credentials themselves; only the *ability to fetch* them teleports.

1. **Fleet HMAC key Secret** (worker namespace) — the per‑session auth token is
   `HMAC(key, sid)`:

   ```sh
   kubectl create secret generic sandboxd-cred-token -n default \
     --from-literal=token="$(openssl rand -hex 24)"
   ```

2. **Enable the vendor on the operator** — add `--cred-token-secret=sandboxd-cred-token`
   to the operator args (injects `SANDBOXD_CRED_TOKEN_KEY` into workers from the
   Secret). Roll the operator.

3. **Grant the worker's identity `sts:AssumeRole`** on the target session role(s).
   With EKS Pod Identity the worker pod has one identity (the checkpoint role,
   currently `sandboxd-worker-checkpoint`); the vendor's AssumeRole runs as it:

   ```sh
   aws iam put-role-policy --role-name sandboxd-worker-checkpoint \
     --policy-name assume-session-roles --policy-document '{"Version":"2012-10-17",
       "Statement":[{"Effect":"Allow","Action":["sts:AssumeRole","sts:TagSession"],
       "Resource":"arn:aws:iam::<acct>:role/<session-role>"}]}'
   ```

4. **Create the session role** with a trust policy naming the worker role as
   principal (and, recommended, requiring the `sandbox-session` tag):

   ```sh
   aws iam create-role --role-name <session-role> --assume-role-policy-document '{
     "Version":"2012-10-17","Statement":[{"Effect":"Allow",
       "Principal":{"AWS":"arn:aws:iam::<acct>:role/sandboxd-worker-checkpoint"},
       "Action":["sts:AssumeRole","sts:TagSession"]}]}'
   # attach whatever permissions the sandbox workload needs
   ```

5. **Enable it on a pool** — set `iam.roleArn` on the `SandboxTemplate` (or per
   `Session`):

   ```sh
   kubectl patch sandboxtemplate <tmpl> -n default --type=merge \
     -p '{"spec":{"iam":{"roleArn":"arn:aws:iam::<acct>:role/<session-role>"}}}'
   kubectl annotate warmpool <pool> -n default sandboxd.io/nudge="$(date +%s)" --overwrite
   ```

Verify from inside a session (the workload's SDK reports the **session role**, not
the worker/node identity):

```
python3 -c 'import boto3; print(boto3.client("sts").get_caller_identity()["Arn"])'
# arn:aws:sts::<acct>:assumed-role/<session-role>/<sid>
```

> The vendor listens on `169.254.170.2:8091`, reachable only from the worker's
> sandbox netns. Do not use `169.254.170.23` — that's the EKS Pod Identity agent
> address the worker's own SDK needs.

## Step 7 — Smoke test

Exercise the control plane end‑to‑end without the broker, using the `/_warm`
primitive and an in‑cluster client. From a debug pod in `default`:

```sh
# Warm (resume/cold-start) a session onto the generic pool. On a GENERIC pool you must
# also name the app (AppTemplate) — the router passes X-Session-Pool (capacity) +
# X-Session-App (workload) to the operator, which lazily creates the Session:
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  -H 'X-Session-ID: sess-smoketest' \
  -H 'X-Session-Pool: aio-generic-pool' \
  -H 'X-Session-App: aio-app' \
  http://sandboxd-router.sandboxd-controlplane-system:8080/_warm
# expect 204 (or 503 Retry-After if the pool has no idle worker)
# (On a DEDICATED pool, drop X-Session-App and use -H 'X-Session-Pool: aio-pool'.)

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
