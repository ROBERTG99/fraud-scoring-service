output "bucket_name" {
  description = "Artifacts bucket: datasets/ in, models/ out."
  value       = aws_s3_bucket.artifacts.bucket
}

output "role_arn" {
  description = "SageMaker execution role for CreateTrainingJob (RoleArn)."
  value       = aws_iam_role.training.arn
}

output "repository_url" {
  description = "ECR repository for training/Dockerfile (TrainingImage without the tag)."
  value       = aws_ecr_repository.training.repository_url
}

output "kms_key_arn" {
  description = "KMS key for the bucket and the repository; pass as --kms-key-arn to the launcher."
  value       = aws_kms_key.artifacts.arn
}
