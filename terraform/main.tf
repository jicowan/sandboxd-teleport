# sandboxd-teleport infrastructure for a BRING-YOUR-OWN EKS cluster.
#
# Creates: the S3 checkpoint bucket, the worker + operator IAM roles (least
# privilege), and EKS Pod Identity associations binding those roles to the
# sandboxd ServiceAccounts on the EXISTING cluster.
#
# Does NOT create: the EKS cluster, its VPC, or the gVisor node group — those are
# prerequisites the operator brings (see docs/sandboxd/install-guide-sandboxd.md).

data "aws_caller_identity" "current" {}

# Look up the existing cluster (validates it exists + gives us the VPC for the
# optional S3 gateway endpoint).
data "aws_eks_cluster" "this" {
  name = var.cluster_name
}

locals {
  account_id  = data.aws_caller_identity.current.account_id
  bucket_name = var.bucket_name != "" ? var.bucket_name : "sandboxd-checkpoints-${local.account_id}-${var.region}"
  bucket_arn  = "arn:aws:s3:::${local.bucket_name}"
  vpc_id      = data.aws_eks_cluster.this.vpc_config[0].vpc_id
}

# ---------------------------------------------------------------------------
# S3 checkpoint bucket. Layout: sandboxes/<sid>/... (per-session) and bases/<id>/...
# (promoted BaseSnapshots). Private, versioning off (checkpoints are immutable +
# GC'd), SSE on.
# ---------------------------------------------------------------------------
resource "aws_s3_bucket" "checkpoints" {
  bucket = local.bucket_name
  tags   = var.tags
}

resource "aws_s3_bucket_public_access_block" "checkpoints" {
  bucket                  = aws_s3_bucket.checkpoints.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "checkpoints" {
  bucket = aws_s3_bucket.checkpoints.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# ---------------------------------------------------------------------------
# Pod Identity trust policy (shared): principal pods.eks.amazonaws.com,
# sts:AssumeRole + sts:TagSession.
# ---------------------------------------------------------------------------
data "aws_iam_policy_document" "pod_identity_trust" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole", "sts:TagSession"]
    principals {
      type        = "Service"
      identifiers = ["pods.eks.amazonaws.com"]
    }
  }
}

# ===========================================================================
# WORKER role — S3 read/write on the checkpoint bucket, assume per-session
# roles (cred vendor), optional ECR pull for private workload images.
# ===========================================================================
resource "aws_iam_role" "worker" {
  name               = "${var.name_prefix}-worker-checkpoint"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_trust.json
  tags               = var.tags
}

data "aws_iam_policy_document" "worker_s3" {
  statement {
    sid       = "ListBucket"
    effect    = "Allow"
    actions   = ["s3:ListBucket"]
    resources = [local.bucket_arn]
  }
  statement {
    sid       = "RWObjects"
    effect    = "Allow"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    resources = ["${local.bucket_arn}/*"]
  }
}

resource "aws_iam_role_policy" "worker_s3" {
  name   = "s3-checkpoints"
  role   = aws_iam_role.worker.id
  policy = data.aws_iam_policy_document.worker_s3.json
}

# Per-session credential vendor: the worker assumes the session's IAM role and
# vends temporary creds to the sandbox. Scoped to roles the operator hands it; the
# TARGET role's trust policy is the real gate (must name this worker role).
data "aws_iam_policy_document" "worker_assume_session" {
  statement {
    sid       = "AssumeSessionRoles"
    effect    = "Allow"
    actions   = ["sts:AssumeRole", "sts:TagSession"]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "worker_assume_session" {
  name   = "assume-session-roles"
  role   = aws_iam_role.worker.id
  policy = data.aws_iam_policy_document.worker_assume_session.json
}

data "aws_iam_policy_document" "worker_ecr" {
  count = var.enable_ecr_pull ? 1 : 0
  statement {
    sid       = "EcrAuth"
    effect    = "Allow"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }
  statement {
    sid       = "EcrPull"
    effect    = "Allow"
    actions   = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:BatchCheckLayerAvailability"]
    resources = ["arn:aws:ecr:${var.region}:${local.account_id}:repository/*"]
  }
}

resource "aws_iam_role_policy" "worker_ecr" {
  count  = var.enable_ecr_pull ? 1 : 0
  name   = "ecr-pull"
  role   = aws_iam_role.worker.id
  policy = data.aws_iam_policy_document.worker_ecr[0].json
}

resource "aws_eks_pod_identity_association" "worker" {
  cluster_name    = var.cluster_name
  namespace       = var.worker_namespace
  service_account = var.worker_service_account
  role_arn        = aws_iam_role.worker.arn
  tags            = var.tags
}

# ===========================================================================
# OPERATOR role — GC (delete under sandboxes/*) + copy-on-promote (read
# sandboxes/*, read/write/delete bases/*). Separate least-privilege identity so
# the privileged worker never also holds S3 delete.
# ===========================================================================
resource "aws_iam_role" "operator" {
  name               = "${var.name_prefix}-operator"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_trust.json
  tags               = var.tags
}

data "aws_iam_policy_document" "operator_s3" {
  statement {
    sid       = "ListBucket"
    effect    = "Allow"
    actions   = ["s3:ListBucket"]
    resources = [local.bucket_arn]
  }
  # GC reaps orphaned/expired per-session snapshots.
  statement {
    sid       = "GcSandboxes"
    effect    = "Allow"
    actions   = ["s3:DeleteObject"]
    resources = ["${local.bucket_arn}/sandboxes/*"]
  }
  # Copy-on-promote reads a source session's checkpoint...
  statement {
    sid       = "ReadSandboxes"
    effect    = "Allow"
    actions   = ["s3:GetObject"]
    resources = ["${local.bucket_arn}/sandboxes/*"]
  }
  # ...and writes/reads/reclaims the fork-stable base copy under bases/.
  statement {
    sid       = "BasesRW"
    effect    = "Allow"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    resources = ["${local.bucket_arn}/bases/*"]
  }
}

resource "aws_iam_role_policy" "operator_s3" {
  name   = "s3-gc-and-promote"
  role   = aws_iam_role.operator.id
  policy = data.aws_iam_policy_document.operator_s3.json
}

resource "aws_eks_pod_identity_association" "operator" {
  cluster_name    = var.cluster_name
  namespace       = var.operator_namespace
  service_account = var.operator_service_account
  role_arn        = aws_iam_role.operator.arn
  tags            = var.tags
}

# ===========================================================================
# Optional: S3 Gateway VPC endpoint on the cluster VPC (keeps worker->S3 on the
# AWS backbone instead of NAT). Attached to all route tables in the VPC.
# ===========================================================================
data "aws_route_tables" "vpc" {
  count  = var.enable_s3_gateway_endpoint ? 1 : 0
  vpc_id = local.vpc_id
}

resource "aws_vpc_endpoint" "s3" {
  count             = var.enable_s3_gateway_endpoint ? 1 : 0
  vpc_id            = local.vpc_id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = data.aws_route_tables.vpc[0].ids
  tags              = merge(var.tags, { Name = "${var.name_prefix}-s3-gateway" })
}
