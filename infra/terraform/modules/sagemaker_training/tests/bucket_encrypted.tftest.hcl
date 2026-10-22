# FR-12: the artifacts bucket is encrypted with the module's KMS key, versioned and never public.
# mock_provider: no AWS credentials, nothing is created (spec §16).
mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = { account_id = "123456789012" }
  }
  mock_data "aws_region" {
    defaults = { region = "us-east-1" }
  }
  mock_data "aws_partition" {
    defaults = { partition = "aws" }
  }
}

run "bucket_is_encrypted_versioned_and_private" {
  command = apply

  assert {
    condition     = aws_s3_bucket.artifacts.bucket == "fraud-scoring-dev-artifacts-123456789012"
    error_message = "bucket name must match training/sagemaker_launch.py"
  }
  assert {
    condition = alltrue([for r in aws_s3_bucket_server_side_encryption_configuration.artifacts.rule :
      alltrue([for d in r.apply_server_side_encryption_by_default :
    d.sse_algorithm == "aws:kms" && d.kms_master_key_id == aws_kms_key.artifacts.arn])])
    error_message = "bucket must use SSE-KMS with the module's key"
  }
  assert {
    condition     = aws_kms_key.artifacts.enable_key_rotation
    error_message = "KMS key rotation must be on"
  }
  assert {
    condition     = one(aws_s3_bucket_versioning.artifacts.versioning_configuration).status == "Enabled"
    error_message = "bucket versioning must be enabled"
  }
  assert {
    condition = alltrue([
      aws_s3_bucket_public_access_block.artifacts.block_public_acls,
      aws_s3_bucket_public_access_block.artifacts.block_public_policy,
      aws_s3_bucket_public_access_block.artifacts.ignore_public_acls,
      aws_s3_bucket_public_access_block.artifacts.restrict_public_buckets,
    ])
    error_message = "every public access block setting must be on"
  }
  assert {
    condition = anytrue([for s in jsondecode(aws_s3_bucket_policy.artifacts.policy).Statement :
    s.Effect == "Deny" && s.Action == "s3:*" && s.Condition.Bool["aws:SecureTransport"] == "false"])
    error_message = "the bucket policy must deny every S3 action over plain HTTP"
  }
  assert {
    condition = (aws_ecr_repository.training.image_tag_mutability == "IMMUTABLE"
    && one(aws_ecr_repository.training.image_scanning_configuration).scan_on_push)
    error_message = "ECR tags must be immutable and images scanned on push"
  }
}
