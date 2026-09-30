output "instance_id" {
  value = aws_instance.gpu.id
}

output "instance_type" {
  value = aws_instance.gpu.instance_type
}

output "availability_zone" {
  value = local.az
}

output "ami" {
  value = "${data.aws_ami.gpu.name} (${data.aws_ami.gpu.id})"
}

output "ssh" {
  description = "Log in to the GPU VM."
  value       = "ssh -i ${trimsuffix(pathexpand(var.ssh_public_key_path), ".pub")} ubuntu@${aws_instance.gpu.public_ip}"
}

output "self_terminates_after_minutes" {
  value = var.max_minutes
}
