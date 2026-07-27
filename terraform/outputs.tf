output "bucket_name" {
  description = "S3 checkpoint bucket — pass to the chart as worker.bucket / operator --worker-bucket."
  value       = aws_s3_bucket.checkpoints.bucket
}

output "worker_role_arn" {
  description = "Worker IAM role ARN (bound to the worker ServiceAccount via Pod Identity)."
  value       = aws_iam_role.worker.arn
}

output "operator_role_arn" {
  description = "Operator IAM role ARN (GC + copy-on-promote), bound to the operator ServiceAccount."
  value       = aws_iam_role.operator.arn
}

output "region" {
  value = var.region
}

output "helm_hint" {
  description = "Values to pass to the Helm install."
  value       = "helm upgrade --install sandboxd charts/sandboxd -n ${var.operator_namespace} --create-namespace --set aws.region=${var.region} --set aws.bucket=${aws_s3_bucket.checkpoints.bucket} --set worker.serviceAccount=${var.worker_service_account}"
}
