#!/usr/bin/env bash
# Rehearse session C on the local kind cluster (simulated GPUs, CPU PyTorch, fake health),
# running the SAME experiment.sh as the paid session.
#
#   ./experiments/real-gpu/session-c/test/rehearse-kind.sh          # step by step
#   AUTO=1 ./experiments/real-gpu/session-c/test/rehearse-kind.sh   # straight through (~20 min)
#
# kind has one GPU node: after the rescue the job waits for the repair instead of moving.
# Needs gpufit-torch-cpu:dev (session-b/test/rehearse-kind.sh builds it) and ./deploy/agent/build.sh.
set -euo pipefail
cd "$(dirname "$0")/.."
source ../../../scripts/guided-lib.sh
source ../session-b/k8s/render.sh

CLASS=gpu.example.com
IMAGE=gpufit-torch-cpu:dev
TRAIN_ARGS="--hidden 256 --layers 2 --micro-batch 256 --accum 4 --total-steps 300 --ckpt-every 20"
DEGRADE_AT=60
CHART=../../../charts/gpufit
HELM_VALUES=(-f ../../../charts/gpufit/ci/kind-values.yaml --set rescue.grace=30s --set pools.recoverAfter=60s --set pools.degradeAfter=20s)
PRICE_PER_HOUR=1.006
RESULTS=${RESULTS:-$PWD/../results/session-c-rehearsal-$(date +%Y%m%d-%H%M)}
WORK=$(mktemp -d)
START=$(date +%s)
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$WORK"' EXIT
cost() { echo "$(( ( $(date +%s) - START ) / 60 )) min so far (kind: \$0)"; }

# Fake health lives in ConfigMap gpufit-fake-metrics (created by the chart). Degrade the GPU the
# job holds (thermal 0.5 -> training 50, like the A10G at 150 W): kind has 8 GPUs per node, and
# the report only counts jobs whose own GPU is degraded.
degrade_node() { DRIVER_NS=gpufit set_health "$2" thermal 0.5 >/dev/null; note "degraded $1/$2 (fake: thermal 0.5)"; }
repair_node()  { DRIVER_NS=gpufit set_all_healthy >/dev/null; note "repaired $1"; }

step C0 "Clean start: no GPUFit release, no lab jobs; image loaded."
helm uninstall gpufit -n gpufit --wait >/dev/null 2>&1 || true
kubectl delete job,svc,deploy -n "$NS_LAB" --all --wait >/dev/null 2>&1 || true
kubectl delete devicetaintrules -l app.kubernetes.io/managed-by=gpufit-controller --ignore-not-found >/dev/null
kubectl get deploy -n dra-example-driver gpufit-controller >/dev/null 2>&1 && { echo "the kind demo controller (deploy/controller) is running; remove it first"; exit 1; }
run "kind load docker-image $IMAGE --name dra-example-driver-cluster 2>&1 | tail -1"
mkdir -p "$RESULTS"; note "results: $RESULTS"

source experiment.sh

step C9 "Uninstall GPUFit (the pre-delete hook removes its taints and labels)."
run "helm uninstall gpufit -n gpufit --wait 2>&1 | tail -1"
run "kubectl delete job,svc,deploy -n $NS_LAB --all --wait >/dev/null; kubectl delete resourceclaimtemplate -n $NS_LAB train-gpu infer-gpu; kubectl delete cm -n $NS_LAB workloads"
run "kubectl get devicetaintrules 2>&1 | grep gpufit || echo 'no gpufit taints left'"
printf '\n\033[1;36m━━ DONE ━━\033[0m\n'
