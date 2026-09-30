#!/usr/bin/env bash
# Session A, run ON YOUR MAC. Guided: every step is explained and waits for Enter.
#   preflight -> terraform apply (billing starts) -> upload -> run session.sh on
#   the GPU -> download results -> terraform destroy (billing ends) -> verify
#
#   ./experiments/real-gpu/session-a/run.sh
set -euo pipefail
cd "$(dirname "$0")"
source ../../../scripts/guided-lib.sh
HERE=$PWD
TF=../../../deploy/aws
export AWS_PROFILE=gpufit
PRICE_PER_HOUR=0.526   # g4dn.xlarge on-demand, us-east-1 (checked 2026-09-30)
# Session A uses the PyTorch image so the benchmark needs no installs.
export TF_VAR_ami_name_pattern="Deep Learning OSS Nvidia Driver AMI GPU PyTorch 2.12 (Ubuntu 24.04) *"

STAMP=$(date +%Y%m%d-%H%M)
RESULTS="$HERE/../results/session-a-$STAMP"
WORK=$(mktemp -d)
APPLIED=0 START=0
SSH_OPTS=(-i ~/.ssh/gpufit -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$WORK/known_hosts" -o ConnectTimeout=10)

# SSO logins expire (~1h). Refresh before steps that must not fail, above all destroy.
ensure_login() {
  if ! aws sts get-caller-identity >/dev/null 2>&1; then
    note "AWS login expired; a browser will open to log in again."
    aws sso login --profile gpufit
  fi
}

cost() {
  local mins=$(( ( $(date +%s) - START ) / 60 ))
  echo "$mins min so far, about \$$(awk "BEGIN { printf \"%.2f\", $mins / 60 * $PRICE_PER_HOUR }") compute"
}

# If anything fails after apply, make it impossible to miss that a VM may be running.
on_exit() {
  rm -rf "$WORK"
  if [[ $APPLIED == 1 ]]; then
    printf '\n\033[1;41m  A GPU VM MAY STILL BE RUNNING AND BILLING.  \033[0m\n'
    echo "  Destroy it now:   aws sso login --profile gpufit"
    echo "                    cd $(cd "$TF" && pwd) && AWS_PROFILE=gpufit terraform destroy"
    echo "  (It also terminates itself after max_minutes, 180 by default.)"
  fi
}
trap on_exit EXIT

step 1 "Preflight (free): login, quota, your current IP, agent binary for the VM."
ensure_login
run "aws sts get-caller-identity --query Account --output text"
quota=$(aws service-quotas get-service-quota --service-code ec2 --quota-code L-DB2E81BA --region us-east-1 --query Quota.Value --output text)
note "GPU quota (Running On-Demand G and VT instances): $quota vCPUs (need >= 4)"
if awk "BEGIN { exit !($quota < 4) }"; then echo "Quota too low; wait for the AWS support case." >&2; exit 1; fi
ip=$(curl -s https://checkip.amazonaws.com | tr -d '[:space:]')
run "printf 'ssh_cidr = \"%s/32\"\n' $ip > $TF/terraform.tfvars && cat $TF/terraform.tfvars"
run "(cd ../../../agent && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $WORK/gpufit-agent ./cmd/gpufit-agent) && file $WORK/gpufit-agent | cut -c1-80"
pause

step 2 "Create the GPU VM. BILLING STARTS when you type 'yes'." \
       "Check the plan: 10 to add, instance_type g4dn.xlarge, a PyTorch Deep Learning AMI."
( cd "$TF" && terraform init -input=false >/dev/null )
APPLIED=1   # from here on, the exit trap warns about a running VM
( cd "$TF" && terraform apply )
START=$(date +%s)
host=$(cd "$TF" && terraform output -raw ssh | awk '{print $NF}')
note "VM is up: $host   ($(cost))"
pause

step 3 "Wait for SSH and for first-boot setup (cloud-init) to finish."
for i in $(seq 1 60); do ssh "${SSH_OPTS[@]}" "$host" true 2>/dev/null && break; sleep 5; done
run "ssh ${SSH_OPTS[*]} $host 'cloud-init status --wait >/dev/null; cat /etc/gpufit-terminates-at; nvidia-smi -L'"
note "($(cost))"
pause

step 4 "Upload and run the session ON THE GPU (about 15-20 min). Output streams here."
run "scp ${SSH_OPTS[*]} $WORK/gpufit-agent session.sh bench.py $host:"
run "ssh ${SSH_OPTS[*]} $host 'chmod +x gpufit-agent session.sh && ./session.sh ~/gpufit-results'" || note "session.sh failed; still downloading what exists"
note "($(cost))"
pause

step 5 "Download all results into the repo."
mkdir -p "$RESULTS"
run "scp -r ${SSH_OPTS[*]} '$host:gpufit-results/*' $RESULTS/"
run "ls $RESULTS | head -30"
pause

step 6 "DESTROY the VM. Billing ends when you type 'yes'."
ensure_login
( cd "$TF" && terraform destroy )
APPLIED=0
note "Session length: $(cost)"
pause

step 7 "Verify nothing GPUFit is left running."
run "aws ec2 describe-instances --region us-east-1 --filters Name=tag:Project,Values=gpufit Name=instance-state-name,Values=pending,running,stopping,stopped --query 'Reservations[].Instances[].InstanceId' --output text | grep . && echo 'STILL RUNNING, check the console!' || echo 'No GPUFit instances running.'"

printf '\n\033[1;36m━━ DONE ━━\033[0m\n'
echo "Results: $RESULTS (summary.md, raw nvidia-smi XML, permissions.json)"
