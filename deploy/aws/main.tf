# One GPU VM in its own small network. Everything here is created by
# `terraform apply` and removed by `terraform destroy`. No NAT gateway, load
# balancer or managed Kubernetes: nothing that keeps billing on its own.

# ---- lookups -------------------------------------------------------------------

data "aws_ami" "gpu" {
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = [var.ami_name_pattern]
  }
  filter {
    name   = "architecture"
    values = ["x86_64"]
  }
}

# Not every availability zone offers every GPU type; pick one that does.
data "aws_ec2_instance_type_offerings" "gpu" {
  location_type = "availability-zone"

  filter {
    name   = "instance-type"
    values = [var.instance_type]
  }
}

locals {
  az = var.availability_zone != "" ? var.availability_zone : sort(data.aws_ec2_instance_type_offerings.gpu.locations)[0]
}

# Fail at plan time (free) instead of at launch if the chosen zone doesn't offer the type.
check "zone_offers_instance_type" {
  assert {
    condition     = contains(data.aws_ec2_instance_type_offerings.gpu.locations, local.az)
    error_message = "${var.instance_type} is not offered in ${local.az}."
  }
}

# ---- network: VPC with one public subnet ----------------------------------------

resource "aws_vpc" "this" {
  cidr_block           = "10.42.0.0/16"
  enable_dns_hostnames = true
  tags                 = { Name = "gpufit" }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = { Name = "gpufit" }
}

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.this.id
  cidr_block              = "10.42.1.0/24"
  availability_zone       = local.az
  map_public_ip_on_launch = true
  tags                    = { Name = "gpufit-public" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  tags   = { Name = "gpufit-public" }

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

# ---- firewall: SSH from your IP only; everything else closed --------------------
# (The Kubernetes API is reached through an SSH tunnel, never opened publicly.)

resource "aws_security_group" "gpu" {
  name        = "gpufit-gpu"
  description = "GPUFit GPU VM: SSH from one IP"
  vpc_id      = aws_vpc.this.id
}

resource "aws_vpc_security_group_ingress_rule" "ssh" {
  security_group_id = aws_security_group.gpu.id
  description       = "SSH from the operator IP only"
  cidr_ipv4         = var.ssh_cidr
  ip_protocol       = "tcp"
  from_port         = 22
  to_port           = 22
}

# The VMs may talk to each other freely (Kubernetes API/kubelet, pod network,
# GPU-to-GPU training traffic). Still nothing from the internet but SSH.
resource "aws_vpc_security_group_ingress_rule" "between_vms" {
  security_group_id            = aws_security_group.gpu.id
  description                  = "All traffic between GPUFit VMs"
  referenced_security_group_id = aws_security_group.gpu.id
  ip_protocol                  = "-1"
}

resource "aws_vpc_security_group_egress_rule" "all" {
  security_group_id = aws_security_group.gpu.id
  description       = "Outbound (packages, container images)"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

# ---- the GPU VM ---------------------------------------------------------------

resource "aws_key_pair" "gpufit" {
  key_name   = "gpufit"
  public_key = file(pathexpand(var.ssh_public_key_path))
}

resource "aws_instance" "gpu" {
  count                  = var.instance_count
  ami                    = data.aws_ami.gpu.id
  instance_type          = var.instance_type
  subnet_id              = aws_subnet.public.id
  vpc_security_group_ids = [aws_security_group.gpu.id]
  key_name               = aws_key_pair.gpufit.key_name

  # Safety net, part 1: a shutdown from inside the VM TERMINATES it (stops
  # all compute billing and deletes the disk) instead of leaving a stopped VM.
  instance_initiated_shutdown_behavior = "terminate"

  # Safety net, part 2: the VM schedules its own shutdown at boot.
  user_data = templatefile("${path.module}/user-data.sh.tftpl", {
    max_minutes = var.max_minutes
  })

  root_block_device {
    volume_type           = "gp3"
    volume_size           = 100
    encrypted             = true
    delete_on_termination = true
  }

  metadata_options {
    http_tokens = "required" # IMDSv2 only
  }

  tags = { Name = "gpufit-gpu-${count.index}" }

  # When a zone is out of GPU capacity, AWS returns InsufficientInstanceCapacity
  # and the provider keeps retrying silently. Give up after a few minutes so the
  # operator can pick another zone or GPU type.
  timeouts {
    create = "4m"
  }
}
