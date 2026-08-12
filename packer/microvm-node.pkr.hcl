# microvm-node.pkr.hcl — build an EKS worker-node AMI with KVM enabled, for
# running Cloud Hypervisor / Firecracker microVM sandboxes (see
# docs/sandboxd/PRD/PRD-microvm-runtime-cloud-hypervisor.md).
#
# SCOPE (deliberately minimal): this AMI enables the HARDWARE-VIRTUALIZATION
# prerequisites ONLY — the KVM kernel modules load at boot and /dev/kvm is
# present and group-accessible. It does NOT bake the VMM binaries: the
# cloud-hypervisor / firecracker / virtiofsd binaries, the guest kernel, and the
# kata-agent guest rootfs travel in the sandboxd WORKER CONTAINER IMAGE (exactly
# as the pinned `runsc` binary does today), so the runtime version stays coupled
# to the image — which is what keeps checkpoint/restore teleport version-safe.
# The node stays generic; only KVM capability is baked in.
#
# BASE: the current EKS-optimized Amazon Linux 2023 node AMI (resolved from the
# public SSM parameter), so nodeadm/kubelet/containerd are unchanged and the node
# joins EKS exactly like a stock node. We only layer KVM config on top.
#
# TARGET NODES: /dev/kvm is exposed on standard Nitro instances (C7i/M7i/R7i/…) when
# NESTED VIRTUALIZATION is enabled (Karpenter v1.14+ EC2NodeClass
# cpuOptions.nestedVirtualization: enabled, or run-instances --cpu-options
# NestedVirtualization=enabled) — no bare metal required (bare-metal *.metal also
# works). This AMI is built on a cheap instance (no KVM needed at BUILD time — we only
# write config); KVM is exercised at RUNTIME on a nested-virt (or metal) node.

packer {
  required_plugins {
    amazon = {
      source  = "github.com/hashicorp/amazon"
      version = ">= 1.3.0"
    }
  }
}

variable "region" {
  type        = string
  default     = "us-west-2"
  description = "AWS region to build (and register) the AMI in."
}

variable "k8s_version" {
  type        = string
  default     = "1.31"
  description = "EKS Kubernetes minor version — selects the EKS-optimized AL2023 base AMI. MUST match your cluster's control-plane version."
}

variable "build_instance_type" {
  type        = string
  default     = "m5.large"
  description = "Instance type used to BUILD the AMI. Need not expose /dev/kvm — the build only writes config; /dev/kvm is exercised at RUNTIME on nested-virt (or *.metal) nodes."
}

variable "subnet_id" {
  type        = string
  default     = ""
  description = "Subnet to launch the BUILD instance in. MUST be a PUBLIC subnet (routes to an IGW, auto-assigns public IP) so Packer can SSH in. Empty => Packer infers one from the VPC, which may pick a private subnet and hang on SSH — set this explicitly for a reliable build."
}

variable "ami_name_prefix" {
  type        = string
  default     = "sandboxd-microvm-al2023-x86_64"
  description = "Prefix for the produced AMI name; a k8s-version + timestamp suffix is appended."
}

variable "root_volume_size" {
  type        = number
  default     = 120
  description = "Root EBS volume size (GiB). MicroVM guest-RAM memory snapshots + re-pulled rootfs layers are large; size generously (gVisor nodes use ~100Gi)."
}

variable "kvm_device_mode" {
  type        = string
  default     = "0666"
  description = "udev mode for /dev/kvm. 0666 lets any process open it; workers are privileged so 0660 also works — 0666 keeps a non-privileged fallback simple."
}

variable "tags" {
  type        = map(string)
  default     = { "app.kubernetes.io/part-of" = "sandboxd", "sandboxd.io/node-runtime" = "microvm" }
  description = "Tags applied to the AMI and its snapshot."
}

# Resolve the current EKS-optimized AL2023 (x86_64, standard) node AMI id from the
# public SSM parameter — always the AWS-recommended, patched base for this k8s version.
data "amazon-parameterstore" "eks_al2023" {
  name   = "/aws/service/eks/optimized-ami/${var.k8s_version}/amazon-linux-2023/x86_64/standard/recommended/image_id"
  region = var.region
}

locals {
  build_time = formatdate("YYYYMMDD-hhmmss", timestamp())
  ami_name   = "${var.ami_name_prefix}-k8s${var.k8s_version}-${local.build_time}"
}

source "amazon-ebs" "microvm" {
  region        = var.region
  instance_type = var.build_instance_type
  source_ami    = data.amazon-parameterstore.eks_al2023.value
  ssh_username  = "ec2-user"

  # Reach the transient build instance over its public IP. Pin a PUBLIC subnet
  # (subnet_id) so Packer doesn't auto-pick a private one and hang on SSH; force a
  # public IP on launch. subnet_id="" lets Packer infer (works only if the inferred
  # subnet is public).
  subnet_id                   = var.subnet_id != "" ? var.subnet_id : null
  associate_public_ip_address = true
  ssh_interface               = "public_ip"

  ami_name                = local.ami_name
  ami_description         = "EKS AL2023 node + KVM enabled for sandboxd Cloud Hypervisor/Firecracker microVMs (k8s ${var.k8s_version}). Run on *.metal."
  ami_virtualization_type = "hvm"

  # The build instance must use IMDSv2 (http_tokens=required): AWS accounts with
  # httpTokensEnforced reject IMDSv1 launches. This governs only the transient
  # BUILD instance, not the produced AMI's runtime metadata behavior.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }

  launch_block_device_mappings {
    device_name           = "/dev/xvda"
    volume_size           = var.root_volume_size
    volume_type           = "gp3"
    delete_on_termination = true
  }

  tags = merge(var.tags, {
    Name                      = local.ami_name
    "sandboxd.io/base-ami"    = data.amazon-parameterstore.eks_al2023.value
    "sandboxd.io/k8s-version" = var.k8s_version
  })
  snapshot_tags = var.tags
}

build {
  name    = "sandboxd-microvm-node"
  sources = ["source.amazon-ebs.microvm"]

  provisioner "shell" {
    # -E preserves env; run as root so we can write /etc and touch modules.
    execute_command = "sudo -E bash '{{ .Path }}'"
    environment_vars = [
      "KVM_DEVICE_MODE=${var.kvm_device_mode}",
      "K8S_VERSION=${var.k8s_version}",
    ]
    script = "${path.root}/scripts/provision-kvm.sh"
  }

  post-processor "manifest" {
    output     = "${path.root}/manifest.json"
    strip_path = true
  }
}
