# Development environment for the SageMaker training module. Never applied by this repository; see
# docs/runbook.md (manual training-job flow) before running `terraform plan` with real credentials.
terraform {
  required_version = ">= 1.9"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.68"
    }
  }

  # Remote state, once a state bucket and lock table exist:
  # backend "s3" {
  #   bucket       = "<state-bucket>"
  #   key          = "fraud-scoring/dev/terraform.tfstate"
  #   region       = "us-east-1"
  #   encrypt      = true
  #   use_lockfile = true
  # }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { managed_by = "terraform", repository = "fraud-scoring-service" }
  }
}

module "sagemaker_training" {
  source      = "../../modules/sagemaker_training"
  environment = var.environment
  tags        = var.tags
}
