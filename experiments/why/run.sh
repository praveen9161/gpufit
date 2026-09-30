#!/usr/bin/env bash
# Guided experiment: two Pending GPU pods, IDENTICAL scheduler message,
# completely different reasons. `kubectl gpufit why` tells them apart.
#
#   ./experiments/why/run.sh          # step by step
#   AUTO=1 ./experiments/why/run.sh   # run straight through
set -euo pipefail
cd "$(dirname "$0")"
source ../../scripts/guided-lib.sh

BIN_DIR=$(mktemp -d)
ORIG_METRICS="$BIN_DIR/metrics.json"
trap 'rm -rf "$BIN_DIR"' EXIT
(cd ../../agent && go build -o "$BIN_DIR" ./cmd/gpufit-claim ./cmd/kubectl-gpufit)
export PATH="$BIN_DIR:$PATH"   # kubectl runs any kubectl-<name> on PATH as "kubectl <name>"
kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS" >/dev/null
kubectl get cm -n "$DRIVER_NS" gpufit-fake-metrics -o jsonpath='{.data.metrics\.json}' > "$ORIG_METRICS"

step 1 "Setup: controller on, all GPUs healthy, a 4-GPU training job and a 1-GPU inference job running."
run kubectl apply -f ../../deploy/controller/controller.yaml
run set_all_healthy
run wait_for_pool training
run "gpufit-claim -workload training -min 80 -count 4 | kubectl apply -f -"
run "gpufit-claim -workload training -min 80 -count 2 | kubectl apply -f -"
run "gpufit-claim -workload inference -min 80 -count 1 | kubectl apply -f -"
run gpu_pod train-big training-min80-4gpu
run gpu_pod infer-1 inference-min80-1gpu
run kubectl wait -n "$NS" --for=condition=Ready pod/train-big pod/infer-1 --timeout=90s
note "train-big: $(show_got train-big)"
note "infer-1:   $(show_got infer-1 inference)"
pause

step 2 "Pending pod #1 (cluster is BUSY): another 4-GPU training job. Only 3 GPUs are free."
run gpu_pod train-busy training-min80-4gpu
sleep 6
run kubectl get pod -n "$NS" train-busy
note "The scheduler's explanation:"
run "kubectl get events -n $NS --field-selector involvedObject.name=train-busy,reason=FailedScheduling -o jsonpath='{.items[-1:].message}'; echo"
note "GPUFit's explanation:"
run kubectl gpufit why train-busy -n "$NS"
run kubectl delete pod -n "$NS" train-busy --wait
pause

step 3 "Pending pod #2 (GPUs UNFIT): gpu-5 overheats -> the node moves to the inference pool." \
       "Then a 2-GPU training job arrives. 3 GPUs are free again, so it's NOT a capacity problem."
run set_health gpu-5 thermal 0.7
run wait_for_pool inference
run gpu_pod train-unfit training-min80-2gpu
sleep 6
run kubectl get pod -n "$NS" train-unfit
note "The scheduler's explanation (compare with step 2):"
run "kubectl get events -n $NS --field-selector involvedObject.name=train-unfit,reason=FailedScheduling -o jsonpath='{.items[-1:].message}'; echo"
note "GPUFit's explanation:"
run kubectl gpufit why train-unfit -n "$NS"
pause

step 4 "Clean up: pods, templates, original fake metrics, controller."
run kubectl delete pod -n "$NS" train-big infer-1 train-unfit --wait
run kubectl delete resourceclaimtemplate -n "$NS" training-min80-4gpu training-min80-2gpu inference-min80-1gpu
run "kubectl patch cm -n $DRIVER_NS gpufit-fake-metrics --type merge -p \"\$(jq -n --rawfile m $ORIG_METRICS '{data: {\"metrics.json\": \$m}}')\""
run kubectl delete -f ../../deploy/controller/controller.yaml
run "kubectl delete devicetaintrules -l app.kubernetes.io/managed-by=gpufit-controller --ignore-not-found"
run "kubectl label node $(gpu_node) gpufit.io/pool- && kubectl annotate node $(gpu_node) gpufit.io/pool-reason-"

printf '\n\033[1;36m━━ DONE ━━\033[0m\n'
echo "Same scheduler message in steps 2 and 3; kubectl gpufit why shows busy vs unfit."
