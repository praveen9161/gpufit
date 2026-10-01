#!/usr/bin/env bash
# Resume session B on the EXISTING VMs/cluster (after orchestrate.sh stopped midway),
# appending to the latest results/session-b-* folder, then guide the destroy.
#
#   ./experiments/real-gpu/resume-session-b.sh
set -euo pipefail
cd "$(dirname "$0")"
source ../../scripts/guided-lib.sh
TF=../../deploy/aws
export AWS_PROFILE=gpufit
source session-b/session.env
TOTAL_PER_HOUR=$(awk "BEGIN { print $PRICE_PER_HOUR * $INSTANCE_COUNT }")
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"; printf "\n\033[1;41m  If you stopped before the destroy step, the VMs are STILL BILLING.  \033[0m\n  Destroy: aws sso login --profile gpufit; cd %s && AWS_PROFILE=gpufit terraform destroy\n" "$(cd "$TF" && pwd)"' EXIT

read -ra HOSTS <<< "$(cd "$TF" && terraform output -json public_ips | jq -r '.[]' | tr '\n' ' ')"
read -ra PRIV <<< "$(cd "$TF" && terraform output -json private_ips | jq -r '.[]' | tr '\n' ' ')"
HOSTS=("${HOSTS[@]/#/ubuntu@}")
RESULTS=$(ls -d "$PWD"/results/session-b-* | tail -1)
SSH_OPTS="-i $HOME/.ssh/gpufit -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=$WORK/known_hosts -o ConnectTimeout=10"
START=$(date +%s)
cost() {
  local mins=$(( ( $(date +%s) - START ) / 60 ))
  echo "$mins min since resume, about \$$(awk "BEGIN { printf \"%.2f\", $mins / 60 * $TOTAL_PER_HOUR }") compute"
}

note "VMs: ${HOSTS[*]}   results: $RESULTS"
HOSTS="${HOSTS[*]}" PRIVATE_IPS="${PRIV[*]}" SSH_OPTS="$SSH_OPTS" RESULTS="$RESULTS" PRICE_PER_HOUR="$PRICE_PER_HOUR" \
  COST_CMD="$(declare -f cost); START=$START; TOTAL_PER_HOUR=$TOTAL_PER_HOUR; cost" \
  bash session-b/resume.sh || note "resume.sh stopped; results so far are in $RESULTS"

step D "DESTROY both VMs. Billing ends when you type 'yes'."
if ! aws sts get-caller-identity >/dev/null 2>&1; then aws sso login --profile gpufit; fi
( cd "$TF" && TF_VAR_instance_count=2 TF_VAR_instance_type="$INSTANCE_TYPE" TF_VAR_ami_name_pattern="$AMI_NAME_PATTERN" \
    TF_VAR_availability_zone="${AVAILABILITY_ZONE:-us-east-1c}" terraform destroy )
run "aws ec2 describe-instances --region us-east-1 --filters Name=tag:Project,Values=gpufit Name=instance-state-name,Values=pending,running,stopping,stopped --query 'Reservations[].Instances[].InstanceId' --output text | grep . && echo 'STILL RUNNING, check the console!' || echo 'No GPUFit instances running.'"
trap 'rm -rf "$WORK"' EXIT
printf '\n\033[1;36m━━ DONE ━━\033[0m\n'
echo "Results: $RESULTS (summary.md)"
