# SageMaker training for fraud-gbdt (spec FR-12, T-44): the execution role, the artifacts bucket and the ECR
# repository that training/sagemaker_launch.py names. Never applied by this repository (spec §0 rule 8);
# `make tf-check` validates and tests it with a mocked provider and no AWS credentials.

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}
data "aws_partition" "current" {}

locals {
  # Names match training/sagemaker_launch.py (LaunchConfig.bucket, role_arn, image_uri).
  name_prefix = "fraud-scoring-${var.environment}"
  account_id  = data.aws_caller_identity.current.account_id
  partition   = data.aws_partition.current.partition
  region      = data.aws_region.current.region
  bucket_name = "${local.name_prefix}-artifacts-${local.account_id}"
  tags = merge(var.tags, {
    project     = "fraud-scoring"
    environment = var.environment
    data        = "synthetic"
  })
}

# ---------------------------------------------------------------- KMS
resource "aws_kms_key" "artifacts" {
  description             = "${local.name_prefix} training data and model artifacts"
  enable_key_rotation     = true
  deletion_window_in_days = var.kms_deletion_window_days
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "AccountAdministers"
        Effect    = "Allow"
        Principal = { AWS = "arn:${local.partition}:iam::${local.account_id}:root" }
        Action    = ["kms:*"] # key policies need the account root to keep the key manageable
        Resource  = "*"       # in a key policy, "*" means this key only
      },
    ]
  })
  tags = local.tags
}

resource "aws_kms_alias" "artifacts" {
  name          = "alias/${local.name_prefix}-artifacts"
  target_key_id = aws_kms_key.artifacts.key_id
}

# ---------------------------------------------------------------- S3
resource "aws_s3_bucket" "artifacts" {
  # checkov:skip=CKV_AWS_18: access logging needs a separate log bucket per account; out of scope for this module
  # checkov:skip=CKV_AWS_144: cross-region replication is not needed for synthetic, reproducible training data
  # checkov:skip=CKV2_AWS_62: no consumer subscribes to bucket events
  bucket = local.bucket_name
  tags   = local.tags
}

resource "aws_s3_bucket_versioning" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.artifacts.arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "artifacts" {
  bucket                  = aws_s3_bucket.artifacts.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  rule {
    id     = "expire-noncurrent-versions"
    status = "Enabled"
    filter {}
    noncurrent_version_expiration {
      noncurrent_days = var.artifact_retention_days
    }
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}

resource "aws_s3_bucket_policy" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "DenyInsecureTransport"
        Effect    = "Deny"
        Principal = { AWS = "*" }
        Action    = ["s3:GetObject", "s3:PutObject", "s3:ListBucket", "s3:DeleteObject"]
        Resource  = [aws_s3_bucket.artifacts.arn, "${aws_s3_bucket.artifacts.arn}/*"]
        Condition = { Bool = { "aws:SecureTransport" = "false" } }
      },
    ]
  })
  depends_on = [aws_s3_bucket_public_access_block.artifacts]
}

# ---------------------------------------------------------------- ECR
resource "aws_ecr_repository" "training" {
  name                 = "${local.name_prefix}-training"
  image_tag_mutability = "IMMUTABLE"
  image_scanning_configuration {
    scan_on_push = true
  }
  encryption_configuration {
    encryption_type = "KMS"
    kms_key         = aws_kms_key.artifacts.arn
  }
  tags = local.tags
}

resource "aws_ecr_lifecycle_policy" "training" {
  repository = aws_ecr_repository.training.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep the 20 most recent training images"
      selection    = { tagStatus = "any", countType = "imageCountMoreThan", countNumber = 20 }
      action       = { type = "expire" }
    }]
  })
}

# ---------------------------------------------------------------- IAM
resource "aws_iam_role" "training" {
  name = "${local.name_prefix}-sagemaker-training"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "sagemaker.amazonaws.com" }
      Action    = "sts:AssumeRole"
      # Only SageMaker jobs of this account may assume the role (confused-deputy guard).
      Condition = {
        StringEquals = { "aws:SourceAccount" = local.account_id }
        ArnLike      = { "aws:SourceArn" = "arn:${local.partition}:sagemaker:${local.region}:${local.account_id}:training-job/*" }
      }
    }]
  })
  tags = local.tags
}

locals {
  # Least privilege for one training job: read the dataset prefix, write the model prefix, pull the training
  # image, use the key, write its own logs. No wildcard actions and no "*" resources.
  training_statements = [
    {
      Sid      = "ListArtifacts"
      Effect   = "Allow"
      Action   = ["s3:ListBucket"]
      Resource = [aws_s3_bucket.artifacts.arn]
    },
    {
      Sid      = "ReadDatasets"
      Effect   = "Allow"
      Action   = ["s3:GetObject"]
      Resource = ["${aws_s3_bucket.artifacts.arn}/datasets/*"]
    },
    {
      Sid      = "WriteModels"
      Effect   = "Allow"
      Action   = ["s3:GetObject", "s3:PutObject"]
      Resource = ["${aws_s3_bucket.artifacts.arn}/models/*"]
    },
    {
      Sid      = "PullTrainingImage"
      Effect   = "Allow"
      Action   = ["ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"]
      Resource = [aws_ecr_repository.training.arn]
    },
    {
      Sid      = "UseArtifactsKey"
      Effect   = "Allow"
      Action   = ["kms:Decrypt", "kms:GenerateDataKey", "kms:DescribeKey"]
      Resource = [aws_kms_key.artifacts.arn]
    },
    {
      Sid      = "WriteJobLogs"
      Effect   = "Allow"
      Action   = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"]
      Resource = ["arn:${local.partition}:logs:${local.region}:${local.account_id}:log-group:/aws/sagemaker/TrainingJobs:log-stream:*"]
    },
  ]
}

resource "aws_iam_role_policy" "training" {
  name   = "${local.name_prefix}-sagemaker-training"
  role   = aws_iam_role.training.id
  policy = jsonencode({ Version = "2012-10-17", Statement = local.training_statements })
}

# The repository grants the pull to this role only.
resource "aws_ecr_repository_policy" "training" {
  repository = aws_ecr_repository.training.name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "TrainingRolePulls"
      Effect    = "Allow"
      Principal = { AWS = aws_iam_role.training.arn }
      Action    = ["ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"]
    }]
  })
}
