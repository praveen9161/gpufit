#!/usr/bin/env bash
# Guided experiment: gpufit-controller moves a node between the TRAINING and
# INFERENCE pool, and marks single GPUs for repair. Design: docs/design/node-pools.md
#
#   ./experiments/node-pools/run.sh          # step by step
#   AUTO=1 ./experiments/node-pools/run.sh   # run straight through
#
# Demo timers: degrade after 10s, recover after 30s (production: 5m / 30m).
# Keep ./scripts/gpu-dash.sh open in a second terminal (pool line + TAINT column).
set -euo pipefail
cd "$(dirname "$0")"
source ../../scripts/guided-lib.sh

BIN_DIR=$(mktemp -d)
ORIG_METRICS="$BIN_DIR/metrics.json"
trap 'rm -rf "$BIN_DIR"' EXIT
(cd ../../agent && go build -o "$BIN_DIR" ./cmd/gpufit-claim)
gpufit-claim() { "$BIN_DIR/gpufit-claim" "$@"; }
kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS" >/dev/null
kubectl get cm -n "$DRIVER_NS" gpufit-fake-metrics -o jsonpath='{.data.metrics\.json}' > "$ORIG_METRICS"

step 1 "Start the controller and make every GPU healthy." \
       "Expect: the node is in the TRAINING pool, no taints."
run kubectl apply -f ../../deploy/controller/controller.yaml
run kubectl rollout status deploy/gpufit-controller -n "$DRIVER_NS" --timeout=90s
run set_all_healthy
run wait_for_pool training
run "kubectl get devicetaintrules 2>&1 | grep gpufit || echo 'no gpufit taints'"
pause

step 2 "A 4-GPU training job starts on the healthy node." \
       "Training claims do NOT tolerate gpufit.io/unfit-training; inference claims DO:"
run "gpufit-claim -workload training -min 80 -count 4 | kubectl apply -f -"
run "gpufit-claim -workload training -min 80 -count 2 | kubectl apply -f -"
run "gpufit-claim -workload inference -min 80 -count 1 | kubectl apply -f -"
run "kubectl get resourceclaimtemplate -n $NS inference-min80-1gpu -o jsonpath='{.spec.spec.devices.requests[0].firstAvailable[0].tolerations}'; echo"
run gpu_pod train-big training-min80-4gpu
run kubectl wait -n "$NS" --for=condition=Ready pod/train-big --timeout=90s
note "train-big got: $(show_got train-big)"
pause

step 3 "gpu-5 starts overheating (thermal 0.7 -> training score 70, a straggler)." \
       "Expect after ~10-20s: node -> INFERENCE pool, all 8 GPUs tainted unfit-training." \
       "The running training job is NOT evicted (NoSchedule only affects new claims)."
run set_health gpu-5 thermal 0.7
run wait_for_pool inference
run "kubectl get node $(gpu_node) -o jsonpath='{.metadata.annotations.gpufit\.io/pool-reason}'; echo"
run "kubectl get events -n default --field-selector involvedObject.kind=Node,source=gpufit-controller --sort-by=.lastTimestamp -o custom-columns=TYPE:.type,REASON:.reason,MESSAGE:.message | tail -3"
run kubectl get pod -n "$NS" train-big
pause

step 4 "New work arrives on the degraded node:" \
       "  a 2-GPU TRAINING job   -> expect Pending (doesn't tolerate unfit-training)" \
       "  a 1-GPU INFERENCE job  -> expect Running (tolerates it: the node keeps earning)"
run gpu_pod train-small training-min80-2gpu
run gpu_pod infer-1 inference-min80-1gpu
run kubectl wait -n "$NS" --for=condition=Ready pod/infer-1 --timeout=90s
sleep 5
run kubectl get pods -n "$NS"
note "infer-1 got: $(show_got infer-1 inference)"
pause

step 5 "gpu-7 gets memory errors (uncorrectable ECC -> inference score 0)." \
       "Expect after ~10-20s: ONLY gpu-7 also gets needs-repair (nobody tolerates that)."
run set_health gpu-7 memory 0
for i in $(seq 1 60); do kubectl get devicetaintrule "gpufit-$(gpu_node)-gpu-7-needs-repair" >/dev/null 2>&1 && break; sleep 2; done
run "kubectl get devicetaintrules -o custom-columns=NAME:.metadata.name,DEVICE:.spec.deviceSelector.device,TAINT:.spec.taint.key | grep -E 'NAME|gpufit'"
pause

step 6 "Repair everything. Recovery waits 30s on purpose (a GPU that just recovered is suspect)." \
       "Expect: node back to TRAINING, taints removed, and the Pending train-small STARTS BY ITSELF."
run set_all_healthy
run wait_for_pool training
run kubectl wait -n "$NS" --for=condition=Ready pod/train-small --timeout=90s
note "train-small got: $(show_got train-small)"
run "kubectl get events -n default --field-selector involvedObject.kind=Node,source=gpufit-controller --sort-by=.lastTimestamp -o custom-columns=TYPE:.type,REASON:.reason,MESSAGE:.message | tail -4"
pause

step 7 "Clean up: pods, templates, restore the original fake metrics, and remove the controller" \
       "(with gpu-3/gpu-5 degraded it would keep the node in the inference pool, blocking the other demos)."
run kubectl delete pod -n "$NS" train-big train-small infer-1 --wait
run kubectl delete resourceclaimtemplate -n "$NS" training-min80-4gpu training-min80-2gpu inference-min80-1gpu
run "kubectl patch cm -n $DRIVER_NS gpufit-fake-metrics --type merge -p \"\$(jq -n --rawfile m $ORIG_METRICS '{data: {\"metrics.json\": \$m}}')\""
run kubectl delete -f ../../deploy/controller/controller.yaml
run "kubectl delete devicetaintrules -l app.kubernetes.io/managed-by=gpufit-controller --ignore-not-found"
run "kubectl label node $(gpu_node) gpufit.io/pool- && kubectl annotate node $(gpu_node) gpufit.io/pool-reason-"

printf '\n\033[1;36m━━ DONE ━━\033[0m\n'
echo "Check each step against its 'Expect' line."
