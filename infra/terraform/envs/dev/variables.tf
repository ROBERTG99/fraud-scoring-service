variable "region" {
  description = "AWS region for the training resources."
  type        = string
  default     = "us-east-1"
}

variable "environment" {
  description = "Environment name; must match --environment of training/sagemaker_launch.py."
  type        = string
  default     = "dev"
}

variable "tags" {
  description = "Extra tags for every resource."
  type        = map(string)
  default     = {}
}
