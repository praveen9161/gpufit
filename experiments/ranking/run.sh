#!/usr/bin/env bash
# Guided experiment: does the DRA scheduler prefer GPUs with higher GPUFit scores?
#
#   ./experiments/ranking/run.sh          # step by step, press Enter between steps
#   AUTO=1 ./experiments/ranking/run.sh   # run straight through
#
# Keep ./scripts/gpu-dash.sh open in a second terminal to watch the cluster.
set -euo pipefail
cd "$(dirname "$0")"
AUTO="${AUTO:-0}"
NS=gpufit-lab

# ---- helpers ------------------------------------------------------------------
step() { printf '\n\033[1;36m━━ STEP %s ━━\033[0m\n' "$1"; shift; printf '%s\n' "$@"; }
run()  { printf '\n\033[1;33m$ %s\033[0m\n' "$*"; eval "$@"; }
note() { printf '\n\033[1;32m→ %s\033[0m\n' "$*"; }
pause() { [[ "$AUTO" == 1 ]] || read -rp $'\n[Enter] to continue, Ctrl-C to stop... ' _; }

# Print "gpu-N (training score)" for each GPU a pod's claim was given.
show_got() {
  local pod=$1 claim
  claim=$(kubectl get pod -n "$NS" "$pod" -o jsonpath='{.status.resourceClaimStatuses[0].resourceClaimName}')
  local devices scores
  devices=$(kubectl get resourceclaim -n "$NS" "$claim" -o json | jq -c '[.status.allocation.devices.results[].device]')
  scores=$(kubectl get resourceslices -o json | jq -c '[.items[].spec.devices[] | {key: .name, value: .attributes["gpufit.io/trainingScore"].int}] | from_entries')
  jq -rn --argjson d "$devices" --argjson s "$scores" \
    '"\($d | map("\(.) (training \($s[.]))") | join(", "))"'
}

# Wait until the ResourceSlice shows the training scores we just applied.
wait_for_scores() {
  local want=$1 got=""
  for _ in $(seq 1 30); do
    got=$(kubectl get resourceslices -o json | jq -c '[.items[].spec.devices[] | {key: .name, value: .attributes["gpufit.io/trainingScore"].int}] | from_entries')
    [[ "$got" == "$want" ]] && { echo "slice updated: $got"; return; }
    sleep 1
  done
  echo "timed out waiting for scores; last seen: $got" >&2
  exit 1
}

# ---- experiment ---------------------------------------------------------------
kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS" >/dev/null

step 1 "Give every GPU a PASSING training score, best scores on the highest numbers:" \
       "  gpu-0=80  gpu-1=83  gpu-2=86  gpu-3=89  gpu-4=92  gpu-5=95  gpu-6=98  gpu-7=100" \
       "We write the scores ConfigMap by hand (not via the agent) so the numbers are exact." \
       "The driver checks it every 5s and republishes the ResourceSlice. Watch the TRAIN column."
pause
run kubectl apply -f 00-scores-ascending.json
want=$(jq -r '.data["scores.json"]' 00-scores-ascending.json | jq -c 'map_values(.training)')
run wait_for_scores "'$want'"
pause

step 2 "Ask for 2 GPUs with trainingScore >= 80. All 8 GPUs pass, so the scheduler must choose." \
       "  If it RANKS by score      -> gpu-6, gpu-7 (98, 100)" \
       "  If it takes FIRST that fit -> gpu-0, gpu-1 (80, 83)" \
       "The CEL rule in the claim:"
run grep expression 01-training-2-min80.yaml
pause
run kubectl apply -f 01-training-2-min80.yaml
run kubectl wait -n "$NS" --for=condition=Ready pod/training-2 --timeout=90s
note "training-2 got: $(show_got training-2)"
pause

step 3 "Is that choice repeatable? Delete and recreate the same pod 3 times."
for i in 1 2 3; do
  kubectl delete -f 01-training-2-min80.yaml --wait >/dev/null
  kubectl apply -f 01-training-2-min80.yaml >/dev/null
  kubectl wait -n "$NS" --for=condition=Ready pod/training-2 --timeout=90s >/dev/null
  note "run $i: training-2 got: $(show_got training-2)"
done
run kubectl delete -f 01-training-2-min80.yaml --wait
pause

step 4 "Today the only way to get better GPUs is to raise the threshold." \
       "Ask for 2 GPUs with trainingScore >= 95. Only gpu-5 (95), gpu-6 (98), gpu-7 (100) pass."
run grep expression 02-training-2-min95.yaml
pause
run kubectl apply -f 02-training-2-min95.yaml
run kubectl wait -n "$NS" --for=condition=Ready pod/training-2-strict --timeout=90s
note "training-2-strict got: $(show_got training-2-strict)"
pause

step 5 "Clean up and put back the normal scores from the agent (gpu-3 degraded)."
run kubectl delete -f 02-training-2-min95.yaml --wait
run "(cd ../../agent && go run ./cmd/gpufit-agent -o configmap | kubectl apply -f -)"
want=$(cd ../../agent && go run ./cmd/gpufit-agent -o configmap | jq -r '.data["scores.json"]' | jq -c 'map_values(.training)')
run wait_for_scores "'$want'"

printf '\n\033[1;36m━━ DONE ━━\033[0m\n'
echo "Compare what steps 2-3 picked with the two predictions in step 2."
