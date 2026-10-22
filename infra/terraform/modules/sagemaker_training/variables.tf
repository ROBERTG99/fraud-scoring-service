variable "environment" {
  description = "Environment name; part of every resource name (fraud-scoring-<environment>-...)."
  type        = string
  default     = "dev"
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,15}$", var.environment))
    error_message = "environment must be 2-16 lowercase letters, digits or hyphens."
  }
}

variable "artifact_retention_days" {
  description = "Days before noncurrent object versions in the artifacts bucket expire."
  type        = number
  default     = 90
}

variable "kms_deletion_window_days" {
  description = "Waiting period before the artifacts KMS key is deleted."
  type        = number
  default     = 30
}

variable "tags" {
  description = "Extra tags for every resource."
  type        = map(string)
  default     = {}
}
