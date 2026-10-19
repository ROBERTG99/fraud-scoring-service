---
paths:
  - "infra/**"
---
# Terraform rules

- Never run `terraform apply` or `terraform destroy`.
- `terraform test` uses `mock_provider "aws"`; no AWS credentials needed.
- No wildcard IAM actions or resources; S3 encrypted (SSE-KMS), versioned, public access blocked.
- `make tf-check` (fmt, validate, test, tflint, checkov) must pass.
