variable "region" {
  description = "AWS region (must match the existing cluster's region)."
  type        = string
  default     = "us-west-2"
}

variable "cluster_name" {
  description = "Name of the EXISTING EKS cluster to wire Pod Identity into. Terraform does NOT create the cluster."
  type        = string
}

variable "bucket_name" {
  description = "S3 checkpoint bucket name. Empty => derived as sandboxd-checkpoints-<account>-<region>."
  type        = string
  default     = ""
}

variable "name_prefix" {
  description = "Prefix for created IAM roles/policies."
  type        = string
  default     = "sandboxd"
}

variable "worker_namespace" {
  description = "Namespace the worker ServiceAccount lives in (must match operator --resume-namespace)."
  type        = string
  default     = "default"
}

variable "worker_service_account" {
  description = "Worker ServiceAccount name (Pod Identity subject for S3 + cred vending)."
  type        = string
  default     = "sandboxd-worker"
}

variable "operator_namespace" {
  description = "Namespace the operator runs in."
  type        = string
  default     = "sandboxd-controlplane-system"
}

variable "operator_service_account" {
  description = "Operator ServiceAccount name (Pod Identity subject for GC + copy-on-promote)."
  type        = string
  default     = "sandboxd-operator"
}

variable "enable_ecr_pull" {
  description = "Attach an ecr-pull policy to the worker role (needed only for PRIVATE-ECR workload images)."
  type        = bool
  default     = true
}

variable "enable_s3_gateway_endpoint" {
  description = "Create an S3 Gateway VPC endpoint on the cluster VPC's route tables (improves worker->S3 path). Opt-in."
  type        = bool
  default     = false
}

variable "tags" {
  description = "Tags applied to created resources."
  type        = map(string)
  default     = { "app.kubernetes.io/part-of" = "sandboxd" }
}
