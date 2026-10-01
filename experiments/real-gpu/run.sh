#!/usr/bin/env bash
# Real-GPU session, run ON YOUR MAC. Guided: every step is explained and waits for Enter.
#   preflight -> terraform apply (billing starts) -> upload -> run session.sh on
#   the GPU -> download results -> terraform destroy (billing ends) -> verify
#
#   ./experiments/real-gpu/run.sh session-a2      # any folder with session.env + session.sh
#   AVAILABILITY_ZONE=us-east-1b ./experiments/real-gpu/run.sh session-a2   # if a zone has no capacity
set -euo pipefail
cd "$(dirname "$0")"
source ../../scripts/guided-lib.sh
HERE=$PWD
SESSION=${1:?usage: run.sh <session-folder>, e.g. session-a2}
SESSION=${SESSION%/}
[[ -f "$SESSION/session.env" && ( -f "$SESSION/session.sh" || -f "$SESSION/orchestrate.sh" ) ]] ||
  { echo "$SESSION needs session.env plus session.sh (single VM) or orchestrate.sh (multi-VM)" >&2; exit 1; }
# shellcheck source=/dev/null
source "$SESSION/session.env"   # INSTANCE_TYPE, PRICE_PER_HOUR, AMI_NAME_PATTERN, EXPECTED_MINUTES
TF=../../deploy/aws
export AWS_PROFILE=gpufit
INSTANCE_COUNT=${INSTANCE_COUNT:-1}   # session.env may set 2 (Kubernetes session)
export TF_VAR_instance_type="$INSTANCE_TYPE" TF_VAR_ami_name_pattern="$AMI_NAME_PATTERN" TF_VAR_instance_count="$INSTANCE_COUNT"
TOTAL_PER_HOUR=$(awk "BEGIN { print $PRICE_PER_HOUR * $INSTANCE_COUNT }")
# Optional: AVAILABILITY_ZONE=us-east-1b ./run.sh ...  when AWS has no capacity in the default zone.
export TF_VAR_availability_zone="${AVAILABILITY_ZONE:-}"

STAMP=$(date +%Y%m%d-%H%M)
RESULTS="$HERE/results/$SESSION-$STAMP"
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
  echo "$mins min so far, about \$$(awk "BEGIN { printf \"%.2f\", $mins / 60 * $TOTAL_PER_HOUR }") compute"
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
need=$(( 4 * INSTANCE_COUNT ))
note "GPU quota (Running On-Demand G and VT instances): $quota vCPUs (need >= $need for $INSTANCE_COUNT VM(s))"
if awk "BEGIN { exit !($quota < $need) }"; then echo "Quota too low for $INSTANCE_COUNT VM(s)." >&2; exit 1; fi
ip=$(curl -s https://checkip.amazonaws.com | tr -d '[:space:]')
run "printf 'ssh_cidr = \"%s/32\"\n' $ip > $TF/terraform.tfvars && cat $TF/terraform.tfvars"
run "(cd ../../agent && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $WORK/gpufit-agent ./cmd/gpufit-agent) && file -b $WORK/gpufit-agent | cut -c1-60"
pause

step 2 "Create $INSTANCE_COUNT GPU VM(s) ($INSTANCE_TYPE, about \$$TOTAL_PER_HOUR/hour total). BILLING STARTS when you type 'yes'." \
       "Check the plan: $(( 10 + INSTANCE_COUNT )) to add, instance_type $INSTANCE_TYPE, a PyTorch Deep Learning AMI."
( cd "$TF" && terraform init -input=false >/dev/null )
APPLIED=1   # from here on, the exit trap warns about a running VM
( cd "$TF" && terraform apply )
START=$(date +%s)
read -ra HOSTS <<< "$(cd "$TF" && terraform output -json public_ips | jq -r '.[]' | tr '\n' ' ')"
read -ra PRIVATE_IPS <<< "$(cd "$TF" && terraform output -json private_ips | jq -r '.[]' | tr '\n' ' ')"
HOSTS=("${HOSTS[@]/#/ubuntu@}")
host=${HOSTS[0]}
note "VM(s) up: ${HOSTS[*]}   ($(cost))"
pause

step 3 "Wait for SSH and for first-boot setup (cloud-init) to finish."
for h in "${HOSTS[@]}"; do
  for i in $(seq 1 60); do ssh "${SSH_OPTS[@]}" "$h" true 2>/dev/null && break; sleep 5; done
  run "ssh ${SSH_OPTS[*]} $h 'cloud-init status --wait >/dev/null; cat /etc/gpufit-terminates-at; nvidia-smi -L'"
done
note "($(cost))"
pause

if [[ -f "$SESSION/orchestrate.sh" ]]; then
  # Multi-VM sessions drive the VMs from this Mac; they write straight into $RESULTS.
  step 4 "Run $SESSION (about $EXPECTED_MINUTES min). It has its own guided steps."
  mkdir -p "$RESULTS"
  HOSTS="${HOSTS[*]}" PRIVATE_IPS="${PRIVATE_IPS[*]}" SSH_OPTS="${SSH_OPTS[*]}" RESULTS="$RESULTS" PRICE_PER_HOUR="$PRICE_PER_HOUR" \
    AGENT_BIN="$WORK/gpufit-agent" COST_CMD="$(declare -f cost); START=$START; TOTAL_PER_HOUR=$TOTAL_PER_HOUR; cost" \
    bash "$SESSION/orchestrate.sh" || {
      note "orchestrate.sh failed; results so far are in $RESULTS"
      if [[ -f "$SESSION/fallback.sh" ]]; then
        note "FALLBACK: run the same measurement without Kubernetes, so this paid time still yields numbers."
        pause
        mv "$RESULTS/results.jsonl" "$RESULTS/results-kubernetes-partial.jsonl" 2>/dev/null || true
        HOSTS="${HOSTS[*]}" PRIVATE_IPS="${PRIVATE_IPS[*]}" SSH_OPTS="${SSH_OPTS[*]}" RESULTS="$RESULTS" PRICE_PER_HOUR="$PRICE_PER_HOUR" \
          bash "$SESSION/fallback.sh" || note "fallback.sh failed too; results so far are in $RESULTS"
      fi
    }
  note "($(cost))"
  pause
else
step 4 "Upload and run $SESSION ON THE GPU (about $EXPECTED_MINUTES min). Output streams here."
shopt -s nullglob   # a session may have no .py files of its own
upload=("$WORK/gpufit-agent" common/bench.py "$SESSION"/*.sh "$SESSION"/*.py)
shopt -u nullglob
run "scp ${SSH_OPTS[*]} ${upload[*]} $host:"
run "ssh ${SSH_OPTS[*]} $host 'chmod +x gpufit-agent session.sh && ./session.sh ~/gpufit-results'" || note "session.sh failed; still downloading what exists"
note "($(cost))"
pause

step 5 "Download all results into the repo."
mkdir -p "$RESULTS"
run "scp -r ${SSH_OPTS[*]} '$host:gpufit-results/*' $RESULTS/"
run "ls $RESULTS | head -30"
pause
fi

step 6 "DESTROY the VM(s). Billing ends when you type 'yes'."
ensure_login
( cd "$TF" && terraform destroy )
APPLIED=0
note "Session length: $(cost)"
pause

step 7 "Verify nothing GPUFit is left running."
run "aws ec2 describe-instances --region us-east-1 --filters Name=tag:Project,Values=gpufit Name=instance-state-name,Values=pending,running,stopping,stopped --query 'Reservations[].Instances[].InstanceId' --output text | grep . && echo 'STILL RUNNING, check the console!' || echo 'No GPUFit instances running.'"

printf '\n\033[1;36m━━ DONE ━━\033[0m\n'
echo "Results: $RESULTS (summary.md, raw nvidia-smi XML, permissions.json)"
