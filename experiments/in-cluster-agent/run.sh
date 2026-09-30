#!/usr/bin/env bash
# Guided experiment: the agent runs INSIDE the cluster. Change a GPU's health
# and watch scores and placement follow, with nothing run from the laptop.
#
#   ./experiments/in-cluster-agent/run.sh          # step by step
#   AUTO=1 ./experiments/in-cluster-agent/run.sh   # run straight through
#
# Prereqs: driver v0.5.0-gpufit-2 and the agent deployed (see deploy/agent/).
# Keep ./scripts/gpu-dash.sh open in a second terminal.
set -euo pipefail
cd "$(dirname "$0")"
source ../../scripts/guided-lib.sh
DNS=dra-example-driver   # driver + agent namespace

BIN_DIR=$(mktemp -d)
trap 'rm -rf "$BIN_DIR"' EXIT
(cd ../../agent && go build -o "$BIN_DIR" ./cmd/gpufit-claim)
gpufit-claim() { "$BIN_DIR/gpufit-claim" "$@"; }

# Set one component of one GPU in the fake metrics, e.g. set_health gpu-6 nvlink 0.5
set_health() {
  local gpu=$1 comp=$2 val=$3 raw
  raw=$(kubectl get cm -n "$DNS" gpufit-fake-metrics -o jsonpath='{.data.metrics\.json}' |
        jq -c --arg g "$gpu" --arg c "$comp" --argjson v "$val" '.[$g][$c] = $v')
  kubectl patch cm -n "$DNS" gpufit-fake-metrics --type merge \
    -p "$(jq -n --arg m "$raw" '{data: {"metrics.json": $m}}')"
}

# Wait until one GPU's published training score equals $2.
wait_for_training() {
  local gpu=$1 want=$2 got=""
  for i in $(seq 1 40); do
    got=$(training_scores | jq -r --arg g "$gpu" '.[$g]')
    if [[ "$got" == "$want" ]]; then echo "$gpu training score is now $got (after ~${i}s)"; return; fi
    sleep 1
  done
  echo "timed out: $gpu training score is $got, wanted $want" >&2
  exit 1
}

kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS" >/dev/null

step 1 "The agent is a pod on the GPU node. Nothing runs on your laptop anymore:"
run kubectl get pods -n "$DNS" -l app=gpufit-agent -o wide
note "Its log says what it scored and why:"
run "kubectl logs -n $DNS -l app=gpufit-agent --tail=9"
pause

step 2 "It writes ONE key per node into gpufit-scores; the driver on that node reads it." \
       "managedFields shows who owns which key (server-side apply):"
run "kubectl get cm -n $DNS gpufit-scores -o json --show-managed-fields | jq '{keys: (.data | keys), owners: [.metadata.managedFields[] | select(.operation == \"Apply\") | .manager]}'"
pause

step 3 "Simulate a failure: gpu-6's NVLink drops to half speed (0.5)." \
       "Chain: fake metrics -> agent (<=5s) -> ConfigMap -> driver (<=5s) -> ResourceSlice." \
       "Watch gpu-6's TRAIN column in the dashboard."
run set_health gpu-6 nvlink 0.5
run wait_for_training gpu-6 0
run "kubectl logs -n $DNS -l app=gpufit-agent --tail=1"
pause

step 4 "A training job asks for 6 GPUs, training >= 80. Healthy for training right now:" \
       "  gpu-0,1,2,4,7 (100) = only 5. gpu-3 (0), gpu-6 (0, just broke), gpu-5 (70, straggler)." \
       "No option can give 6 GPUs >= 80 -> expect Pending."
run "gpufit-claim -workload training -min 80 -count 6 | kubectl apply -f -"
run kubectl apply -f ../claim-generator/11-train-job.yaml
sleep 6
run kubectl get pod -n "$NS" train-job
pause

step 5 "Repair gpu-6 (NVLink back to 1.0). Its training score returns to 100 -> 6 GPUs qualify." \
       "Expect: the Pending job starts BY ITSELF via min95 on gpu-0,1,2,4,6,7. Nothing is re-submitted."
run set_health gpu-6 nvlink 1.0
run wait_for_training gpu-6 100
run kubectl wait -n "$NS" --for=condition=Ready pod/train-job --timeout=90s
note "train-job got: $(show_got train-job)"
pause

step 6 "Clean up (the fake metrics are back to the defaults already)."
run kubectl delete -f ../claim-generator/11-train-job.yaml --wait
run kubectl delete resourceclaimtemplate -n "$NS" training-min80-6gpu

printf '\n\033[1;36m━━ DONE ━━\033[0m\n'
echo "Check each step against its 'expect' line."
