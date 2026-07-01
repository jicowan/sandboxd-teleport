# Stage 0 — Findings (NO-GO on EKS as of 2026-07-01)

**Verdict: BLOCKED. The stock Agent Substrate install cannot run on managed
Amazon EKS at any version available today.** Discovered during pre-flight
verification, before creating any AWS resources (no S3 bucket, IAM role, ECR
repo, or cluster changes were made — nothing to tear down).

Substrate upstream pinned SHA: `4aedeab2c6b3efe29cfc2763cc7aee224f675dae`
(cloned to `~/GitHub/Projects/substrate-upstream`, outside this repo).

## The blocker

Substrate's install deploys a **`podcertificate-controller`** (its mTLS
service-identity layer) that the install script *waits on* before deploying the
API server — it blocks until specific ClusterTrustBundles exist. That controller
requires the API server to serve two `certificates.k8s.io` resources:

- `podcertificaterequests`
- `clustertrustbundles`

(RBAC in `manifests/ate-install/pod-certificate-controller.yaml`; signers
`servicedns.podcert.ate.dev/*`, `podidentity.podcert.ate.dev/*`.)

**Neither is served on EKS** — any offered version — so the install stalls
permanently at the trust-bundle wait.

## Why (root cause, not a version/config issue)

These APIs live behind feature gates that default **`false` even at beta** (the
SIG-auth exception to "beta = on by default"):

| Feature | gate | alpha | beta | on-by-default | GA target |
|---|---|---|---|---|---|
| PodCertificateRequest (KEP-4317) | `PodCertificateRequest` | 1.34 | 1.35 | **no** | **1.37** |
| ClusterTrustBundle (KEP-3257) | `ClusterTrustBundle` | 1.27 | 1.33 | **no** | **1.37** |

When the gate is off, the API server doesn't register
`certificates.k8s.io/v1beta1`, so the resources don't exist. **EKS does not let
customers enable arbitrary control-plane feature gates**
(aws/containers-roadmap#512, open since 2019), and EKS release notes do not list
either gate among the non-default betas EKS turns on for 1.33–1.36.

## Empirical proof (live clusters, this account, us-west-2)

`certificates.k8s.io` versions served, per cluster:

| Cluster | K8s version | served |
|---|---|---|
| `EKSClusterStack-cluster` | 1.31 | `v1` only |
| `hybrid` | 1.33 | `v1` only |
| `sa-token-auth` | **1.35** | `v1` only |

The 1.35 cluster is decisive: `PodCertificateRequest` is *already beta* there,
yet no `v1beta1` / `clustertrustbundles` / `podcertificaterequests` appear —
only the long-GA `certificatesigningrequests`. EKS 1.36 shares the same gate
defaults, so it behaves identically.

**"Create a 1.36 cluster" does NOT fix this.** Earliest plausible fix is
Kubernetes **1.37** (gates go on-by-default at GA) *and* EKS shipping+enabling
it — 1.37 is not GA upstream as of 2026-07-01. Not a near-term option.

## Other EKS-adaptation facts learned (still true, for whenever this is revisited)

- **Install is `ko`-from-source** (`ko://` refs; no prebuilt images). Needs Go +
  ko + a registry.
- **S3 snapshots ARE supported** — `cmd/atelet/internal/ategcs/s3.go`
  (aws-sdk-go-v2), selected by env `ATE_STORAGE_BACKEND=s3` (not by URL scheme),
  auth via AWS default chain → **EKS Pod Identity works** (agent confirmed
  running on `EKSClusterStack-cluster`). Set `ATE_STORAGE_BACKEND=s3` +
  `AWS_REGION` on the atelet DaemonSet.
- **Existing `gvisor` RuntimeClass is irrelevant** — substrate uses NO
  RuntimeClassName; `atelet` downloads a `runsc` binary at runtime (default
  anonymous `gs://gvisor/...`) and `ateom-gvisor` shells out to it in a
  **privileged** worker pod (privileged, runAsUser 0, hostPath
  `/var/lib/ateom-gvisor`).
- **Only kind vs GKE install paths exist**; the kind overlay
  (`manifests/ate-install/kind/`) is the reference non-GCP config. No EKS path.
- gVisor availability itself (G1) was never reached — blocked earlier, at the
  control-plane API dependency.

## Options from here

1. **kind spike** — substrate's only tested non-GKE path. A local kind cluster
   *can* enable the alpha/beta gates, so the stock demo (gVisor
   checkpoint/restore, suspend/resume, S3-to-rustfs) can run there. Proves G1 /
   the core suspend-resume value at zero cloud cost; does NOT prove EKS.
2. **Pod-certificate bypass** — investigate running substrate without the
   pod-certificate controller (mTLS identity; there's an `--auth-mode=mtls|jwt`
   knob). Uncertain, diverges from stock, v0.0.0 yak-shave.
3. **Shelve** — substrate is not viable on managed EKS until K8s 1.37+ reaches
   EKS with these gates enabled. Revisit then. The working
   agentgateway/agent-sandbox stack on `main` is unaffected.

## Recommendation

Shelve the EKS path (option 3) as the production track — it's blocked by an
upstream/EKS timeline we don't control. If we still want to evaluate substrate's
suspend/resume model on its merits, do the **kind spike (option 1)** as a
separate, local-only exercise and decide based on that whether substrate is
worth waiting for 1.37. Do not spend effort on option 2 against a pre-production
project.

**No resources were created; nothing to roll back.** PRD stages 1–4 remain
not-started and are gated on this decision.
