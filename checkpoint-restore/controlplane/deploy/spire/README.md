# deploy/spire — SPIFFE/SPIRE for control-plane mTLS (P1.5)

Manifests to secure the sandboxd **control hops** (router→operator `/resume`,
operator→worker control API) with SPIFFE mTLS. mTLS is **opt-in**; these are additive
to the base control-plane deploy (`../smoke/controlplane.yaml`), which stays plain-HTTP
by default. Full guide: [../../../../docs/sandboxd/security-spiffe-spire.md](../../../../docs/sandboxd/security-spiffe-spire.md).

## Files

| File | What |
|------|------|
| `values.yaml` | Helm values for the `spiffe/spire` chart (trust domain `sandboxd`, controller-manager, EBS datastore, gVisor-taint tolerations on agent + CSI driver, OIDC provider off). |
| `clusterspiffeids.yaml` | `ClusterSPIFFEID` CRs minting `spiffe://sandboxd/{operator,router,worker}` by pod label / SA + namespace. |
| `controlplane-mtls-patch.yaml` | Strategic-merge patch that turns on mTLS for the **operator** (adds `--mtls` + SPIRE CSI socket). |
| `router-mtls-patch.yaml` | Same for the **router** (adds `--mtls` + CSI socket, flips `--resume-url` to `https://`). |

Worker-pod mTLS is **not** a manifest here — the operator's `WarmPool` reconciler
provisions it automatically once the operator runs with `--mtls` (SPIRE CSI socket,
`SANDBOXD_MTLS=1`, plain health probe on `:8092`).

## Install

```sh
helm repo add spiffe https://spiffe.github.io/helm-charts-hardened/ && helm repo update spiffe
helm install spire-crds spiffe/spire-crds --version 0.5.0 -n spire-system --create-namespace
helm install spire      spiffe/spire      --version 0.29.0 -n spire-system -f values.yaml

kubectl apply -f clusterspiffeids.yaml

kubectl patch deploy sandboxd-operator -n sandboxd-controlplane-system --patch-file controlplane-mtls-patch.yaml
kubectl patch deploy sandboxd-router   -n sandboxd-controlplane-system --patch-file router-mtls-patch.yaml
# then roll the worker pool (bump the SandboxTemplate workerImage / annotate the pool)
```

Verify + troubleshoot: see the security guide.
