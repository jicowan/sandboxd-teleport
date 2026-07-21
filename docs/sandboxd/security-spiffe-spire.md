# Securing the control plane with SPIFFE/SPIRE (mTLS)

How to install and configure **SPIRE** so the sandboxd **control‑plane hops are
mutually authenticated with SPIFFE mTLS** (P1.5). This closes the trust gap where any
workload that could reach a control port could drive it — after this, only the
workload holding the expected **SPIFFE ID** can.

**Scope of this pass — the CONTROL hops:**

| Hop | Client → Server | Server port | Authorized peer |
|-----|-----------------|-------------|-----------------|
| Resume | **router → operator** `/resume` | operator `:8082` | server authorizes caller `spiffe://sandboxd/router` |
| Worker control | **operator → worker** `/run`,`/restore`,`/checkpoint`,`/suspend`,`/reset` | worker `:8090` | worker authorizes caller `spiffe://sandboxd/operator` |

The **data‑plane** hops (broker → router, router → worker) are **not** in this pass
(deferred). The worker credential vendor (`:8091`, netns‑internal, HMAC‑authed) is
unaffected.

mTLS is **off by default** in code (`--mtls` / `SANDBOXD_MTLS=1`); the steps below opt
in. When off, everything runs plain HTTP as before (rollout fallback).

---

## 1. Prerequisites

- A cluster with a default `StorageClass` (SPIRE server keeps a small datastore on a
  PVC). The reference cluster uses `ebs-sc`.
- `helm` 3.x.
- The identities are keyed on Kubernetes ServiceAccount + namespace, so the
  operator/router/worker must run under stable SAs (they do:
  `sandboxd-operator`, `sandboxd-router`'s pods, `sandboxd-worker`).

---

## 2. Install SPIRE (Helm)

The reference values live at `controlplane/deploy/spire/values.yaml`. Install the CRDs
release first, then the chart:

```sh
helm repo add spiffe https://spiffe.github.io/helm-charts-hardened/
helm repo update spiffe

helm install spire-crds spiffe/spire-crds --version 0.5.0 \
  -n spire-system --create-namespace

helm install spire spiffe/spire --version 0.29.0 \
  -n spire-system -f controlplane/deploy/spire/values.yaml
```

Key points in `values.yaml`:

```yaml
global:
  spire:
    trustDomain: sandboxd            # SPIFFE IDs are spiffe://sandboxd/<name>
    clusterName: EKSClusterStack-cluster
spire-server:
  persistence: { enabled: true, storageClass: ebs-sc, size: 1Gi }
  controllerManager: { enabled: true }   # reconciles ClusterSPIFFEID CRs -> entries
spiffe-csi-driver:
  enabled: true                          # mounts the Workload API socket into pods
  tolerations:                           # REQUIRED: run on the gVisor worker nodes
    - { key: sandbox, operator: Equal, value: gvisor, effect: NoSchedule }
spire-agent:
  tolerations:                           # REQUIRED: attest pods on gVisor nodes
    - { key: sandbox, operator: Equal, value: gvisor, effect: NoSchedule }
spire-spiffe-oidc-discovery-provider:
  enabled: false                         # not needed for internal mTLS
```

> **Tolerations are not optional.** sandboxd worker pods run on nodes tainted
> `sandbox=gvisor:NoSchedule`. The SPIRE **agent** and **spiffe‑csi‑driver**
> DaemonSets must tolerate that taint, or worker pods on those nodes can't get an
> SVID and fail to mount the Workload API socket
> (`driver csi.spiffe.io not found in the list of registered CSI drivers`).

> **`helm --wait` may report `failed`** even on a healthy install (it waits on a
> resource that never becomes Ready in a minimal config). Verify pods directly
> instead of trusting the release status; install **without** `--wait`.

Verify:

```sh
kubectl get pods -n spire-system          # server 2/2 (server + controller-manager),
                                          # agent + csi-driver on EVERY node incl. gVisor
```

---

## 3. Register the workload identities

The operator, router, and worker each need a `ClusterSPIFFEID` (reconciled into a
SPIRE registration entry by the controller‑manager). They are at
`controlplane/deploy/spire/clusterspiffeids.yaml`:

```sh
kubectl apply -f controlplane/deploy/spire/clusterspiffeids.yaml
```

| SPIFFE ID | Selector |
|-----------|----------|
| `spiffe://sandboxd/operator` | pod label `app.kubernetes.io/name=sandboxd-operator`, ns `sandboxd-controlplane-system` |
| `spiffe://sandboxd/router`   | pod label `app.kubernetes.io/name=sandboxd-router`, ns `sandboxd-controlplane-system` |
| `spiffe://sandboxd/worker`   | `k8s:sa:sandboxd-worker` + `k8s:ns:default` |

> `className` **must** be `spire-system-spire` (the chart's controller class) or the
> entries are never created.

Confirm entries exist:

```sh
kubectl exec -n spire-system spire-server-0 -c spire-server -- \
  /opt/spire/bin/spire-server entry show | grep spiffe://sandboxd
```

All three (`operator`, `router`, `worker`) must appear. A missing entry makes that
component crash‑loop with `X509Source: context deadline exceeded` (no SVID).

---

## 4. Turn on mTLS in the components

Each component gets the Workload API socket (via the SPIFFE CSI volume) and `--mtls`.

### Operator + router (static Deployments)

mTLS is an **opt-in overlay** on the base control-plane deploy (which stays plain).
Apply the two strategic-merge patches in `controlplane/deploy/spire/` — they add the
SPIRE CSI socket + `--mtls` (and, for the router, flip `--resume-url` to `https://`):

```sh
kubectl patch deploy sandboxd-operator -n sandboxd-controlplane-system \
  --patch-file controlplane/deploy/spire/controlplane-mtls-patch.yaml
kubectl patch deploy sandboxd-router   -n sandboxd-controlplane-system \
  --patch-file controlplane/deploy/spire/router-mtls-patch.yaml
```

Each patch mounts:

```yaml
volumes:
  - name: spiffe-workload-api
    csi: { driver: csi.spiffe.io, readOnly: true }
# container: + --mtls arg + volumeMount /spiffe-workload-api (readOnly)
# router container also: --resume-url=https://sandboxd-operator:8082/resume
```

### Worker (provisioned by the operator — automatic)

Do **not** edit worker pods by hand. When the operator runs with `--mtls`, the
`WarmPool` reconciler automatically renders worker pods with the SPIFFE CSI volume,
`SANDBOXD_MTLS=1`, and a readiness probe on the **plain** health port `:8092` (see
§5). Just set `--mtls` on the operator and roll the pool (bump the template or
annotate the pool) to pick up an mTLS‑capable worker image.

### Flags / env (all components)

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `--mtls` | `SANDBOXD_MTLS=1` | off | Enable SPIFFE mTLS on the control hops. |
| `--spiffe-socket` | `SPIFFE_ENDPOINT_SOCKET` | `unix:///spiffe-workload-api/spire-agent.sock` | Workload API socket. |
| `--spiffe-router-id` (operator) | `SANDBOXD_SPIFFE_ROUTER_ID` | `spiffe://sandboxd/router` | ID the operator authorizes as the `/resume` caller. |
| `--spiffe-worker-id` (operator) | `SANDBOXD_SPIFFE_WORKER_ID` | `spiffe://sandboxd/worker` | ID the operator authorizes when calling workers. |
| `--spiffe-operator-id` (router) | `SANDBOXD_SPIFFE_OPERATOR_ID` | `spiffe://sandboxd/operator` | ID the router authorizes when calling `/resume`. |
| `SANDBOXD_SPIFFE_OPERATOR_ID` (worker) | — | `spiffe://sandboxd/operator` | ID the worker authorizes as the control‑API caller. |

---

## 5. Health probes stay OFF the mTLS ports

kubelet has no SVID, so liveness/readiness probes must not hit an mTLS port:

- **Worker:** a dedicated **plain‑HTTP** health listener on `:8092`
  (`SANDBOXD_HEALTH_ADDR`) serves only `/healthz`; it is always plain regardless of
  `SANDBOXD_MTLS`. The worker Deployment's readiness probe targets `:8092`, never the
  mTLS control API `:8090`. (Both are wired automatically by the reconciler.)
- **Operator:** its `/healthz` + `/readyz` are already on a separate port
  (`--health-probe-bind-address`, `:8081`), independent of the mTLS `/resume` on
  `:8082`. Untouched, stays plain.

---

## 6. Verify it works (and that it enforces)

**Positive — a session flows through both mTLS hops:**

```sh
# from an in-cluster pod, drive a cold start through the router:
curl -sX POST -H 'X-Session-ID: sess-x' -H 'X-Session-Pool: aio-pool' \
  -H 'Content-Type: application/json' -d '{"command":"echo MTLS_OK"}' \
  http://sandboxd-router.sandboxd-controlplane-system.svc.cluster.local:8080/v1/shell/exec
# -> {"...","output":"MTLS_OK",...}  (router->operator /resume + operator->worker /run both mTLS)
```

**Negative — a caller without an SVID is rejected at the TLS layer:**

```sh
# no client cert -> the server demands one:
curl -sk https://sandboxd-operator.sandboxd-controlplane-system:8082/resume
# -> tlsv13 alert certificate required
curl -sk https://<worker-pod-ip>:8090/status
# -> tlsv13 alert certificate required
# plain HTTP to a TLS port:
curl -s http://<worker-pod-ip>:8090/status        # -> 400 "HTTP request to HTTPS server"
# the plain health port is open (kubelet path):
curl -s http://<worker-pod-ip>:8092/healthz       # -> ok
```

---

## 7. Troubleshooting

| Symptom | Cause / fix |
|---------|-------------|
| Worker pod `ContainerCreating`, event `driver csi.spiffe.io not found` | SPIFFE CSI driver isn't on that node — add the node's taint tolerations to `spiffe-csi-driver` **and** `spire-agent` (§2). |
| Component crash‑loops `X509Source: context deadline exceeded` | No SVID — the `ClusterSPIFFEID` for that component is missing or its selector doesn't match the pod labels/SA/ns (§3). |
| `resume failed: ... HTTP request to an HTTPS server` | A scheme mismatch: the caller used `http://` against an mTLS server. Router `--resume-url` must be `https://`; the operator→worker client uses `https` automatically when `--mtls` is set. |
| `resume failed: ... context deadline exceeded` on a long‑running pod | Stale trust bundle after a SPIRE (re)install. **Roll‑restart the component** so it fetches the current bundle. |
| `helm status` shows `failed` but pods are healthy | `--wait` timed out on a non‑critical resource; verify pods directly. Install without `--wait`. |

---

## 8. Rollback

mTLS is opt‑in, so rollback is: remove `--mtls` (and the CSI volume) from operator +
router, revert the router `--resume-url` to `http://`, and roll the worker pool
(operator without `--mtls` renders plain workers). SPIRE itself can stay installed;
it's inert to components that don't request an SVID.

---

## 9. Notes / future

- **Identity source is swappable.** The mTLS wiring is behind a small helper
  (`internal/spiffemtls`, and `sandboxd/mtls.go` for the worker). A future move to
  Kubernetes **pod certificates** (ClusterTrustBundle + kubelet‑issued certs) would
  replace only how the `tls.Config` is populated, not the server/client wiring or the
  peer‑ID authorization.
- **Data‑plane pass (deferred).** Securing broker → router and router → worker is a
  second pass. When the router's inbound `:8080` gets mTLS, it will need a dedicated
  plain health port too (as the worker's `:8092` does now).
- **SVID rotation** is automatic (the agent rotates X509‑SVIDs well before expiry; the
  go‑spiffe source hot‑reloads them). No restarts needed in steady state.
