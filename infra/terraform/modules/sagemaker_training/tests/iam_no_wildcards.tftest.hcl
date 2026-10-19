# FR-12: the SageMaker execution role has no wildcard action and no "*" resource, and only SageMaker jobs of
# this account can assume it. mock_provider: no AWS credentials.
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

run "execution_role_is_least_privilege" {
  command = apply

  assert {
    condition = alltrue(flatten([for s in jsondecode(aws_iam_role_policy.training.policy).Statement :
    [for a in s.Action : !strcontains(a, "*")]]))
    error_message = "no IAM action may contain a wildcard"
  }
  assert {
    condition = alltrue(flatten([for s in jsondecode(aws_iam_role_policy.training.policy).Statement :
    [for r in s.Resource : r != "*" && !endswith(r, ":::*") && !endswith(r, ":*:*")]]))
    error_message = "no IAM resource may be \"*\" or account-wide"
  }
  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.training.policy).Statement :
    s.Effect == "Allow" && length(s.Resource) > 0])
    error_message = "every statement must name its resources"
  }
  assert {
    condition = (one(jsondecode(aws_iam_role.training.assume_role_policy).Statement).Principal.Service == "sagemaker.amazonaws.com"
    && one(jsondecode(aws_iam_role.training.assume_role_policy).Statement).Condition.StringEquals["aws:SourceAccount"] == "123456789012")
    error_message = "only SageMaker jobs of this account may assume the role"
  }
  assert {
    condition     = aws_iam_role.training.name == "fraud-scoring-dev-sagemaker-training"
    error_message = "role name must match training/sagemaker_launch.py"
  }
}
