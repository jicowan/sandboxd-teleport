# sandboxd microVM node AMI (Packer)

Builds an EKS worker-node AMI with **KVM enabled**, for running Cloud Hypervisor /
Firecracker **microVM** sandboxes. This is the node-prerequisite half of
[PRD-microvm-runtime-cloud-hypervisor.md](../docs/sandboxd/PRD/PRD-microvm-runtime-cloud-hypervisor.md);
the runtime binaries themselves live in the worker container image, not here.

## What this AMI is (and is not)

**Is:** the current EKS-optimized Amazon Linux 2023 node AMI + KVM enablement:
- `kvm` + `kvm_intel` + `kvm_amd` loaded at boot (`/etc/modules-load.d/kvm.conf`);
  one AMI serves both Intel and AMD metal — the kernel ignores the non-matching module.
- `/dev/kvm` present, owned `root:kvm`, mode `0666` (`/etc/udev/rules.d/65-kvm.rules`).
- a boot-time `sandboxd-kvm-check.service` that logs loudly to the journal if
  `/dev/kvm` is absent (i.e. the node isn't bare-metal), so misprovisioning is
  diagnosable instead of silently failing every microVM `/run`.

**Is not:** it does **not** bake `cloud-hypervisor`, `firecracker`, `virtiofsd`, the
guest kernel, or the kata-agent rootfs. Those ship in the sandboxd **worker container
image** (like the pinned `runsc` today) so the runtime version stays coupled to the
image — which is what keeps checkpoint/restore teleport version-safe. nodeadm / kubelet /
containerd are unchanged from the stock EKS AMI, so the node joins EKS normally.

## Requirements

- [Packer](https://www.packer.io) ≥ 1.3, AWS credentials with EC2/AMI build permissions.
- The produced AMI must **run on a bare-metal instance** (`*.metal`, e.g.
  `c7a.metal-48xl` (AMD) or `c5.metal` (Intel)). AWS only exposes `/dev/kvm` on
  bare-metal; standard Nitro instances do **not** provide nested virtualization.
- `k8s_version` **must match your EKS control-plane minor version** (it selects the
  base AMI).

## Build

```sh
# from repo root
make ami-microvm AWS_REGION=us-west-2 K8S_VERSION=1.31
# or directly:
cd packer
packer init microvm-node.pkr.hcl
packer build -var region=us-west-2 -var k8s_version=1.31 microvm-node.pkr.hcl
```

The produced AMI id is written to `packer/manifest.json`. Validate/format without
building:

```sh
make ami-microvm-validate    # packer init + validate + fmt -check
```

## Using the AMI (Karpenter EC2NodeClass)

This repo is **bring-your-own-cluster** (Terraform wires only IAM/S3/Pod-Identity; it
does not create node groups — see `terraform/`). The AMI is consumed by a **Karpenter
`EC2NodeClass`** that references it in `amiSelectorTerms`, paired with a `NodePool`
constrained to **bare-metal** instance types (the only ones that expose `/dev/kvm`).

```yaml
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: sandboxd-microvm
spec:
  amiFamily: AL2023            # must match the AMI's base family
  amiSelectorTerms:
    - id: ami-xxxxxxxxxxxxxxxxx           # this AMI (see packer/manifest.json)
    # or, to always take the newest build, select by the tags Packer sets:
    # - tags:
    #     "sandboxd.io/node-runtime": microvm
    #     "sandboxd.io/k8s-version": "1.31"
  role: <your-node-instance-role>
  # ... subnetSelectorTerms / securityGroupSelectorTerms as for your cluster
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: sandboxd-microvm
spec:
  template:
    metadata:
      labels:
        sandbox: microvm          # workers select this (nodeSelector)
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: sandboxd-microvm
      taints:
        - key: sandbox
          value: microvm
          effect: NoSchedule
      requirements:
        # BARE METAL ONLY — /dev/kvm is not exposed on non-metal Nitro instances.
        - key: karpenter.k8s.aws/instance-family
          operator: In
          values: ["c7a"]         # c7a.metal-48xl (AMD); keep ONE family for teleport
        - key: karpenter.k8s.aws/instance-size
          operator: In
          values: ["metal-48xl"]
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
```

> **`amiFamily`/`amiSelectorTerms`.** Because we bake KVM onto the EKS-optimized AL2023
> AMI (bootstrap unchanged), set `amiFamily: AL2023` and pin the AMI by id or by the
> `sandboxd.io/node-runtime: microvm` tag Packer applies. Karpenter runs the stock
> AL2023 bootstrap, so the node joins EKS normally; our `modules-load.d` + udev config
> apply on first boot.

A microVM `SandboxTemplate` then pins its workers to these nodes via
`spec.scheduling.nodeSelector: {sandbox: microvm}` + the matching toleration, and (Phase
0+) declares `spec.runtime: microvm`. The `/dev/kvm` pod-shape is injected by the
operator for microVM pools in a later phase.

> **CPU-feature homogeneity (teleport).** As with gVisor, a microVM checkpoint restores
> only on a CPU-compatible host, so constrain the NodePool to a **single instance
> family** (all AMD `c7a.metal-*` or all Intel `c5.metal`) so snapshots teleport across
> the pool's nodes.

## Verify on a live node

```sh
# on the metal node (SSM/ssh):
lsmod | grep kvm            # kvm + kvm_amd (or kvm_intel)
ls -l /dev/kvm              # crw-rw-rw- root kvm
systemctl status sandboxd-kvm-check   # "/dev/kvm present ..."
```
