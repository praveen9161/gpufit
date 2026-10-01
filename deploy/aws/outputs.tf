output "instance_ids" {
  value = aws_instance.gpu[*].id
}

output "instance_type" {
  value = var.instance_type
}

output "availability_zone" {
  value = local.az
}

output "ami" {
  value = "${data.aws_ami.gpu.name} (${data.aws_ami.gpu.id})"
}

output "public_ips" {
  description = "Reachable from the operator's IP over SSH only."
  value       = aws_instance.gpu[*].public_ip
}

output "private_ips" {
  description = "Addresses the VMs use to talk to each other."
  value       = aws_instance.gpu[*].private_ip
}

output "ssh" {
  description = "Log in to the GPU VMs."
  value       = [for ip in aws_instance.gpu[*].public_ip : "ssh -i ${trimsuffix(pathexpand(var.ssh_public_key_path), ".pub")} ubuntu@${ip}"]
}

output "self_terminates_after_minutes" {
  value = var.max_minutes
}
