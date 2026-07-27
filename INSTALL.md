# Installing sandboxd

Get sandboxd running on an **existing** EKS cluster in four steps. See
[`docs/sandboxd/install-guide-sandboxd.md`](docs/sandboxd/install-guide-sandboxd.md)
for the full reference.

## Prerequisites (bring your own)

sandboxd does **not** create the cluster or nodes. You provide:

1. **An EKS cluster** with the **EKS Pod Identity agent** add-on enabled.
2. **A gVisor node group** — nodes labeled `sandbox=gvisor`, tainted
   `sandbox=gvisor:NoSchedule`, with `runsc release-20260622.0` installed on-node
   (must match the worker image's runsc), ~100Gi root disk, a single instance
   family (CPU-feature match for checkpoint/restore). For a walkthrough of running
   gVisor on EKS with Karpenter, see
   [Running gVisor on EKS with Karpenter](https://medium.com/@jicowan/running-gvisor-on-eks-with-karpenter-39e8d914e1c3).
3. **Tools:** `terraform`, `helm`, `kubectl`, `aws`, `docker`, Go 1.26 (only if
   building images locally).
4. **(mTLS, default on)** SPIRE installed with the sandboxd ClusterSPIFFEIDs — see
   [`docs/sandboxd/security-spiffe-spire.md`](docs/sandboxd/security-spiffe-spire.md).
   To skip: install with `--set mtls.enabled=false`.

## 1. Provision AWS infra (S3 + IAM + Pod Identity)

```sh
make infra CLUSTER_NAME=<your-eks-cluster> AWS_REGION=us-west-2
```

Creates the checkpoint bucket, the worker + operator IAM roles (least privilege),
and the Pod Identity associations. Prints a `helm_hint` with the values to use next.
Tear down with `make infra-destroy CLUSTER_NAME=<...>` (does **not** touch the cluster).

## 2. Build + push the component images

```sh
make images AWS_ACCOUNT=<acct> AWS_REGION=us-west-2
```

Builds the operator, router, worker (sandboxd + pinned runsc), and the broker
(Python, from source) and pushes them to ECR tagged with the git SHA and `:latest`.
To package the Go binaries from a published GitHub release instead of building them
locally: `make images BINSRC=release RELEASE=vX` (the broker always builds from source).

## 3. Install the control plane (Helm)

```sh
helm upgrade --install sandboxd charts/sandboxd \
  --namespace sandboxd-controlplane-system --create-namespace --wait \
  --set image.registry=<acct>.dkr.ecr.us-west-2.amazonaws.com \
  --set image.tag=<tag> \
  --set aws.region=us-west-2 \
  --set aws.bucket=<bucket-from-terraform>
```

Installs the CRDs, RBAC, Valkey, operator, and router. The pods start in dependency
order (valkey → operator → router, via initContainers), and `--wait` blocks until
Ready. mTLS is on by default (needs SPIRE); add `--set mtls.enabled=false` for a
plain-HTTP install. See `charts/sandboxd/values.yaml` for all options.

## 4. Create a pool (or use the starter)

Workers are managed by the operator via `WarmPool` + `SandboxTemplate` CRs — not by
Helm. Enable the built-in starter pool for an immediately drivable sandbox:

```sh
helm upgrade sandboxd charts/sandboxd -n sandboxd-controlplane-system --reuse-values \
  --set starterPool.enabled=true --set runtimeClass.enabled=true
```

Or author your own pool — see
[`checkpoint-restore/controlplane/deploy/aio/`](checkpoint-restore/controlplane/deploy/aio/)
and [`docs/sandboxd/admin-guide-crds.md`](docs/sandboxd/admin-guide-crds.md).

## 5. (Optional) Install the MCP front door

The front door — broker + agentgateway + optional Keycloak realm — terminates
user identity at the edge and routes per-app MCP traffic to the control plane.
It's a separate chart (`charts/sandboxd-frontdoor`) so you can adopt it
independently or bring your own gateway.

```sh
helm upgrade --install sandboxd-frontdoor charts/sandboxd-frontdoor \
  --namespace default --wait \
  --set image.registry=<acct>.dkr.ecr.us-west-2.amazonaws.com \
  --set oidc.issuer=https://<keycloak-host>/realms/sandbox \
  --set publicHost=agentgateway.example.com
```

Install this **after** the control plane — the broker's initContainer waits for
the router Service, and agentgateway's waits for the broker. A single `oidc.*`
block feeds the broker, agentgateway, and (if `keycloak.deploy=true`) the realm
import so issuer/audience/group can't drift. Expose it publicly with
`--set agentgateway.ingress.enabled=true --set agentgateway.ingress.certificateArn=<acm-arn>`
(needs the AWS Load Balancer Controller). Keycloak is BYO by default; the chart
does not install the Keycloak server. See `charts/sandboxd-frontdoor/values.yaml`.

## Releases (CI)

Pushing to `main` builds the binaries and pushes `:latest` + `:<sha>` images to ECR.
Pushing a `v*` tag additionally publishes a GitHub Release with the binaries + runsc
attached and tags the images `:<version>`. The image-push job assumes an AWS role via
OIDC — set the `AWS_ECR_PUSH_ROLE_ARN` repo secret. See
[`.github/workflows/release.yml`](.github/workflows/release.yml).

## Layout

| Path | What |
|------|------|
| `Makefile` | Top-level: `make infra` / `images` / `install` / `help` |
| `terraform/` | S3 bucket + IAM roles + Pod Identity (BYO cluster) |
| `build/docker/` | Distroless Dockerfiles (copy prebuilt binaries) |
| `charts/sandboxd/` | Helm chart (CRDs, RBAC, operator, router, valkey) |
| `charts/sandboxd-frontdoor/` | Helm chart (broker, agentgateway, optional Keycloak realm) |
| `.github/workflows/release.yml` | Build binaries + images (incl. broker) on merge/tag |
