# deploy/aws: one GPU VM for real-hardware sessions

Creates a VPC, a public subnet, a firewall that only allows SSH from your IP,
and one single-GPU EC2 instance from the AWS Deep Learning Base AMI (NVIDIA
driver, CUDA and Docker preinstalled). No managed services that bill on their
own. `terraform destroy` removes everything.

## Safety nets

- `allowed_account_ids`: Terraform refuses to run against any account but GPUFit's.
- The VM schedules its own shutdown at boot (`max_minutes`, default 180), and
  its shutdown behavior is **terminate**, so it can't be forgotten as a
  stopped-but-billing VM.
- Still run `terraform destroy` at the end of every session and check the EC2
  console shows nothing running.

## Use

```bash
cp terraform.tfvars.example terraform.tfvars   # set ssh_cidr to your IP/32
terraform init
terraform plan      # free, read-only: shows exactly what would be created
terraform apply     # starts billing (~$0.53/h for g4dn.xlarge, Sept 2026)
terraform output ssh
terraform destroy   # ends billing
```

Prereqs: AWS CLI profile `gpufit`, SSH key `~/.ssh/gpufit`, EC2 quota
"Running On-Demand G and VT instances" >= 4 vCPUs in the region.
