output "bucket_name" {
  description = "Upload the dataset to s3://<bucket_name>/datasets/demo/."
  value       = module.sagemaker_training.bucket_name
}

output "role_arn" {
  description = "SageMaker execution role."
  value       = module.sagemaker_training.role_arn
}

output "repository_url" {
  description = "Push training/Dockerfile here with an immutable tag."
  value       = module.sagemaker_training.repository_url
}

output "kms_key_arn" {
  description = "Pass as --kms-key-arn to training/sagemaker_launch.py."
  value       = module.sagemaker_training.kms_key_arn
}
