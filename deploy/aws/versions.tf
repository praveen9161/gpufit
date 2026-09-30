terraform {
  required_version = ">= 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}

provider "aws" {
  region  = var.region
  profile = var.aws_profile

  # Safety: refuse to touch any account except the GPUFit one, even if the
  # profile is misconfigured (this Mac also has an unrelated "default" profile).
  allowed_account_ids = [var.account_id]

  default_tags {
    tags = {
      Project   = "gpufit"
      ManagedBy = "terraform"
    }
  }
}
