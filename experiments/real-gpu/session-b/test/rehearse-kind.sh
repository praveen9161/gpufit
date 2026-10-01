#!/usr/bin/env bash
# Rehearse session B's Kubernetes part on the local kind cluster: the same
# manifests (render.sh), CPU instead of GPUs, the example DRA driver instead of
# NVIDIA's. Checks what can go wrong before paid time:
#   - the 2-pod training job finds its master (headless Service + Indexed Job)
#   - jobs print their JSON results
#   - with GPUFit's controller on and the node in the inference pool,
#     training stays Pending (and `kubectl gpufit why` says why) while
#     inference (tolerates unfit-training) still runs
#
#   ./experiments/real-gpu/session-b/test/rehearse-kind.sh        (AUTO=1 to run straight through)
set -euo pipefail
cd "$(dirname "$0")"
source ../../../../scripts/guided-lib.sh
source ../k8s/render.sh
IMAGE=gpufit-torch-cpu:dev
TINY_TRAIN="--hidden 256 --layers 2 --micro-batch 256 --accum 4 --warmup-steps 1 --steps 5"
TINY_INFER="--hidden 512 --layers 2 --warmup 0.5 --seconds 1"
BIN_DIR=$(mktemp -d)
ORIG_METRICS="$BIN_DIR/metrics.json"
trap 'rm -rf "$BIN_DIR"' EXIT
(cd ../../../../agent && go build -o "$BIN_DIR" ./cmd/kubectl-gpufit)
export PATH="$BIN_DIR:$PATH"

wait_job() {  # wait_job <name> <timeout>
  kubectl wait -n "$NS_LAB" --for=condition=Complete "job/$1" --timeout="$2" ||
    { kubectl get pods -n "$NS_LAB" -l "job-name=$1" -o wide; kubectl logs -n "$NS_LAB" -l "job-name=$1" --tail=20 --prefix; return 1; }
}

step 1 "CPU image with PyTorch (pytorch/pytorch is x86-only), loaded into kind."
run "docker build -q -t $IMAGE -f Dockerfile.cpu . && kind load docker-image $IMAGE --name dra-example-driver-cluster 2>&1 | tail -1"
run "kubectl create ns $NS_LAB --dry-run=client -o yaml | kubectl apply -f -"
run "render_workloads_configmap | kubectl apply -f -"
run "render_claims gpu.example.com | kubectl apply -f -"
pause

step 2 "2-pod data-parallel training job (Indexed Job + headless Service rendezvous)."
run "render_train train-2gpu 2 $IMAGE $TINY_TRAIN | kubectl apply -f -"
run wait_job train-2gpu 300s
note "result: $(job_result train-2gpu)"
run "kubectl get pods -n $NS_LAB -l job-name=train-2gpu -o wide | awk '{print \$1, \$3, \$7}'"
pause

step 3 "1-pod training job and an inference job."
run "render_train train-1gpu 1 $IMAGE $TINY_TRAIN | kubectl apply -f -"
run "render_infer infer $IMAGE $TINY_INFER | kubectl apply -f -"
run wait_job train-1gpu 300s
run wait_job infer 300s
note "train-1gpu: $(job_result train-1gpu)"
note "infer:      $(job_result infer)"
run "kubectl delete job,svc -n $NS_LAB train-2gpu train-1gpu infer --ignore-not-found --wait"
pause

step 4 "GPUFit on: controller + the default fake metrics (gpu-3/gpu-5 degraded) put the node in the" \
       "INFERENCE pool. Expect: 2-pod training Pending (why: tainted), inference still runs."
kubectl get cm -n "$DRIVER_NS" gpufit-fake-metrics -o jsonpath='{.data.metrics\.json}' > "$ORIG_METRICS"
run kubectl apply -f ../../../../deploy/controller/controller.yaml
run wait_for_pool inference
run "render_train train-2gpu 2 $IMAGE $TINY_TRAIN | kubectl apply -f -"
sleep 8
run "kubectl get pods -n $NS_LAB -l job-name=train-2gpu"
run "kubectl gpufit why \$(kubectl get pods -n $NS_LAB -l job-name=train-2gpu -o name | head -1 | cut -d/ -f2) -n $NS_LAB | tail -4"
run "render_infer infer $IMAGE $TINY_INFER | kubectl apply -f -"
run wait_job infer 300s
note "infer on the inference-pool node: $(job_result infer)"
pause

step 5 "Clean up (controller removed, taints and pool labels cleared, fake metrics restored)."
run "kubectl delete job,svc -n $NS_LAB train-2gpu infer --ignore-not-found --wait"
run "kubectl delete resourceclaimtemplate -n $NS_LAB train-gpu infer-gpu --ignore-not-found"
run "kubectl delete configmap -n $NS_LAB workloads --ignore-not-found"
run kubectl delete -f ../../../../deploy/controller/controller.yaml --ignore-not-found
run "kubectl delete devicetaintrules -l app.kubernetes.io/managed-by=gpufit-controller --ignore-not-found"
run "kubectl label node $(gpu_node) gpufit.io/pool- && kubectl annotate node $(gpu_node) gpufit.io/pool-reason-"
run "kubectl patch cm -n $DRIVER_NS gpufit-fake-metrics --type merge -p \"\$(jq -n --rawfile m $ORIG_METRICS '{data: {\"metrics.json\": \$m}}')\""

printf '\n\033[1;36m━━ DONE ━━\033[0m\n'
