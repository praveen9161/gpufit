variable "account_id" {
  description = "The only AWS account this configuration may touch. Set it in account.auto.tfvars (git-ignored), e.g. account_id = \"123456789012\"."
  type        = string

  validation {
    condition     = can(regex("^[0-9]{12}$", var.account_id))
    error_message = "account_id must be a 12-digit AWS account ID."
  }
}

variable "aws_profile" {
  description = "AWS CLI profile to use (set up with: aws configure sso --profile gpufit)."
  type        = string
  default     = "gpufit"
}

variable "region" {
  description = "Region where the GPU quota was granted."
  type        = string
  default     = "us-east-1"
}

variable "availability_zone" {
  description = "Zone for the GPU VM. Empty = first zone that offers instance_type. Set it when AWS reports InsufficientInstanceCapacity in a zone."
  type        = string
  default     = ""
}

variable "instance_count" {
  description = "Number of identical GPU VMs (1 for single-GPU measurements, 2-4 for the Kubernetes sessions)."
  type        = number
  default     = 1

  validation {
    condition     = var.instance_count >= 1 && var.instance_count <= 4
    error_message = "instance_count must be 1-4 (the GPU quota is 16 vCPUs: four 4-vCPU instances)."
  }
}

variable "instance_type" {
  description = "Single-GPU instance. g4dn.xlarge = 1x T4 (cheapest); g6.xlarge = 1x L4; g5.xlarge = 1x A10G. All 4 vCPUs."
  type        = string
  default     = "g4dn.xlarge"
}

variable "ami_name_pattern" {
  description = "AWS Deep Learning Base AMI: NVIDIA driver, CUDA and Docker preinstalled, so paid minutes aren't spent installing drivers."
  type        = string
  default     = "Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) *"
}

variable "ssh_cidr" {
  description = "Only this address may SSH in, e.g. \"203.0.113.7/32\" (your public IP)."
  type        = string

  validation {
    condition     = can(cidrhost(var.ssh_cidr, 0)) && endswith(var.ssh_cidr, "/32")
    error_message = "ssh_cidr must be a single address like 203.0.113.7/32."
  }
}

variable "ssh_public_key_path" {
  description = "Public half of the SSH key used to log in."
  type        = string
  default     = "~/.ssh/gpufit.pub"
}

variable "max_minutes" {
  description = "Safety net: the instance powers off and TERMINATES itself after this many minutes."
  type        = number
  default     = 180

  validation {
    condition     = var.max_minutes >= 15 && var.max_minutes <= 480
    error_message = "max_minutes must be between 15 and 480."
  }
}
