#!/usr/bin/env bash
# Guided experiment: users say "N GPUs for <workload>, score >= X" and
# gpufit-claim writes the tiered ResourceClaimTemplate for them.
#
#   ./experiments/claim-generator/run.sh          # step by step
#   AUTO=1 ./experiments/claim-generator/run.sh   # run straight through
#
# Keep ./scripts/gpu-dash.sh open in a second terminal.
set -euo pipefail
cd "$(dirname "$0")"
source ../../scripts/guided-lib.sh

# Build the CLI once so each step shows a short command.
BIN_DIR=$(mktemp -d)
trap 'rm -rf "$BIN_DIR"' EXIT
(cd ../../agent && go build -o "$BIN_DIR" ./cmd/gpufit-claim ./cmd/gpufit-agent)
gpufit-claim() { "$BIN_DIR/gpufit-claim" "$@"; }
gpufit-agent() { (cd ../../agent && "$BIN_DIR/gpufit-agent" "$@"); }

kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS" >/dev/null

step 1 "Publish the normal scores from the agent's fake metrics:" \
       "  gpu-3: NVLink broken  -> training 0,  inference 100" \
       "  gpu-5: slower + hot   -> training 91, inference 85" \
       "  others: healthy       -> 100 everywhere"
run "gpufit-agent -o table"
run "gpufit-agent -o configmap | kubectl apply -f -"
want=$(gpufit-agent -o configmap | jq -r '.data["scores.json"]' | jq -c 'map_values(.training)')
run wait_for_scores "'$want'"
pause

step 2 "A training user asks for 6 GPUs, training score >= 80. They type ONE command:"
run "gpufit-claim -workload training -min 80 -count 6"
note "That YAML (tiers min95 -> min90 -> min80) is what you'd otherwise write by hand."
run "gpufit-claim -workload training -min 80 -count 6 | kubectl apply -f -"
pause

step 3 "Start the training job. Training >= 95: gpu-0,1,2,4,6,7 (exactly 6)." \
       "Expect min95 -> those six. gpu-3 (training 0) and gpu-5 (91) are left alone."
run kubectl apply -f 11-train-job.yaml
run kubectl wait -n "$NS" --for=condition=Ready pod/train-job --timeout=90s
note "train-job got: $(show_got train-job)"
pause

step 4 "An inference user asks for 1 GPU, INFERENCE score >= 80. Same tool, different workload:"
run "gpufit-claim -workload inference -min 80 -count 1 | kubectl apply -f -"
run "kubectl get resourceclaimtemplate -n $NS inference-min80-1gpu -o jsonpath='{range .spec.spec.devices.requests[0].firstAvailable[*]}{.name}: {.selectors[0].cel.expression}{\"\\n\"}{end}'"
pause

step 5 "Free GPUs: gpu-3 (inference 100) and gpu-5 (inference 85)." \
       "Expect min95 -> gpu-3. The GPU that is useless for training is the BEST pick for inference."
run kubectl apply -f 12-infer-job-1.yaml
run kubectl wait -n "$NS" --for=condition=Ready pod/infer-job-1 --timeout=90s
note "infer-job-1 got: $(show_got infer-job-1 inference)"
pause

step 6 "A second inference job. Only gpu-5 (inference 85) is free." \
       "Expect fallback to min80 -> gpu-5. Nothing is wasted: all 8 GPUs are now working."
run kubectl apply -f 13-infer-job-2.yaml
run kubectl wait -n "$NS" --for=condition=Ready pod/infer-job-2 --timeout=90s
note "infer-job-2 got: $(show_got infer-job-2 inference)"
pause

step 7 "Clean up."
run kubectl delete -f 11-train-job.yaml -f 12-infer-job-1.yaml -f 13-infer-job-2.yaml --wait
run kubectl delete resourceclaimtemplate -n "$NS" training-min80-6gpu inference-min80-1gpu

printf '\n\033[1;36m━━ DONE ━━\033[0m\n'
echo "Check each job against its 'Expect' line."
