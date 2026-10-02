#!/usr/bin/env bash
# Guided experiment: GPUFit RESCUES a running training job off a degraded GPU.
# Design: docs/design/v0.1-tool.md (Rescue).
#
#   ./experiments/rescue/run.sh          # step by step
#   AUTO=1 ./experiments/rescue/run.sh   # run straight through
#
# A 2-pod PyTorch DDP job (CPU, tiny model) checkpoints every few steps to a
# small checkpoint store. A GPU on its node degrades; GPUFit moves the node to
# the inference pool and, after a grace period, evicts the job (it opted in with
# gpufit.io/rescue=true). The Job recreates the pods; they wait (Pending) until
# training-pool GPUs exist, then resume from the last checkpoint, not step 0.
#
# kind has ONE GPU node, so "move" here means wait for the repair. On a cluster
# with several nodes (session C) the pods land on another node right away.
# Demo timers: degrade after 10s, rescue grace 20s, recover after 30s.
set -euo pipefail
cd "$(dirname "$0")"
source ../../scripts/guided-lib.sh
source ../real-gpu/session-b/k8s/render.sh

IMAGE=gpufit-torch-cpu:dev   # built by experiments/real-gpu/session-b/test/rehearse-kind.sh
JOB=train-job
TRAIN_ARGS="--hidden 256 --layers 2 --micro-batch 256 --accum 4 --total-steps 600 --ckpt-every 20"

BIN_DIR=$(mktemp -d)
ORIG_METRICS="$BIN_DIR/metrics.json"
trap 'rm -rf "$BIN_DIR"' EXIT
(cd ../../agent && go build -o "$BIN_DIR" ./cmd/kubectl-gpufit)
kubectl-gpufit() { "$BIN_DIR/kubectl-gpufit" "$@"; }
kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS" >/dev/null
kubectl get cm -n "$DRIVER_NS" gpufit-fake-metrics -o jsonpath='{.data.metrics\.json}' > "$ORIG_METRICS"

# Rank 0's progress lines (the pod with completion index 0, whichever incarnation).
rank0_log() { kubectl logs -n "$NS" -l "job-name=$JOB,batch.kubernetes.io/job-completion-index=0" --tail=-1 2>/dev/null | grep '^{' || true; }
wait_for_log() {   # wait_for_log <jq filter> <timeout s>
  for _ in $(seq 1 "$2"); do
    rank0_log | jq -e "select($1)" >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "timed out waiting for: $1"; return 1
}
job_pods() { kubectl get pods -n "$NS" -l "job-name=$JOB" -o custom-columns=POD:.metadata.name,STATUS:.status.phase,NODE:.spec.nodeName --no-headers; }

step 1 "Start the controller (enforce + rescue, grace 20s), make every GPU healthy," \
       "and start a checkpoint store (stand-in for S3/NFS)."
docker image inspect "$IMAGE" >/dev/null 2>&1 || { echo "missing $IMAGE: run experiments/real-gpu/session-b/test/rehearse-kind.sh first"; exit 1; }
run "kind load docker-image $IMAGE --name dra-example-driver-cluster 2>&1 | tail -1"
run kubectl apply -f ../../deploy/controller/controller.yaml
run kubectl rollout status deploy/gpufit-controller -n "$DRIVER_NS" --timeout=90s
run "kubectl get deploy -n $DRIVER_NS gpufit-controller -o jsonpath='{.spec.template.spec.containers[0].args}'; echo"
run set_all_healthy
run wait_for_pool training
run "render_claims gpu.example.com | kubectl apply -f -"
run "render_workloads_configmap | kubectl apply -f -"
run "render_ckpt_store $IMAGE | kubectl apply -f -"
run kubectl rollout status deploy/ckpt-store -n "$NS" --timeout=120s
pause

step 2 "A 2-pod training job starts. It opted in to rescue:" \
       "  labels gpufit.io/workload=training, gpufit.io/rescue=true" \
       "  podFailurePolicy: an eviction (DisruptionTarget) is not a failure -> the Job recreates the pod"
run "render_train_job $JOB 2 $IMAGE $TRAIN_ARGS | kubectl apply -f -"
run "kubectl get job -n $NS $JOB -o jsonpath='{.spec.podFailurePolicy}'; echo"
run "wait_for_log '.event==\"checkpoint\"' 300"
run "rank0_log | tail -3"
run job_pods
pause

step 3 "gpu-5 overheats (thermal 0.7 -> training score 70). Expect:" \
       "  ~10-20s: node -> INFERENCE pool (new training can't start there)" \
       "  +20s grace: GPUFit evicts the job's pods (only because it opted in)"
run set_health gpu-5 thermal 0.7
run wait_for_pool inference
T_POOL=$(date +%s)
note "Waiting for the rescue (grace period 20s)..."
for _ in $(seq 1 90); do
  kubectl logs -n "$DRIVER_NS" deploy/gpufit-controller 2>/dev/null | grep -q "rescue: .*moved off" && break
  sleep 1
done
note "Rescued $(( $(date +%s) - T_POOL ))s after the node left the training pool."
run "kubectl logs -n $DRIVER_NS deploy/gpufit-controller | grep 'rescue:'"
run "rank0_log | grep checkpoint | tail -1"
pause

step 4 "The Job recreates the pods once the evicted ones are gone (podReplacementPolicy: Failed," \
       "~20s). On kind there is no other training-pool node, so they wait (Pending) instead" \
       "of running slowly on the degraded GPU. 'why' shows the reason:"
for _ in $(seq 1 120); do
  [[ $(kubectl get pods -n "$NS" -l "job-name=$JOB" --field-selector=status.phase=Pending -o name | wc -l) -ge 2 ]] && break
  sleep 1
done
run job_pods
run "kubectl-gpufit why -n $NS \$(kubectl get pods -n $NS -l job-name=$JOB --field-selector=status.phase=Pending -o name | head -1 | cut -d/ -f2) | head -25"
run "kubectl-gpufit report -n $DRIVER_NS"
pause

step 5 "Repair gpu-5. After the 30s recovery hold the node rejoins the TRAINING pool." \
       "Expect: the Pending pods start by themselves and RESUME from the last checkpoint."
run set_all_healthy
run wait_for_pool training
run "kubectl wait -n $NS --for=condition=complete job/$JOB --timeout=600s"
run "rank0_log | jq -c 'select(.event==\"start\")'"
run "rank0_log | tail -1"
pause

step 6 "Clean up: job, store, templates, original fake metrics, controller."
run "kubectl delete job,svc -n $NS $JOB --wait"
run "kubectl delete deploy,svc -n $NS ckpt-store --wait"
run "kubectl delete resourceclaimtemplate -n $NS train-gpu infer-gpu; kubectl delete cm -n $NS workloads"
run "kubectl patch cm -n $DRIVER_NS gpufit-fake-metrics --type merge -p \"\$(jq -n --rawfile m $ORIG_METRICS '{data: {\"metrics.json\": \$m}}')\""
run kubectl delete -f ../../deploy/controller/controller.yaml
run "kubectl delete devicetaintrules -l app.kubernetes.io/managed-by=gpufit-controller --ignore-not-found"
run "kubectl label node $(gpu_node) gpufit.io/pool- && kubectl annotate node $(gpu_node) gpufit.io/pool-reason-"

printf '\n\033[1;36m━━ DONE ━━\033[0m\n'
echo "Check each step against its 'Expect' line: the second 'start' line must show resumed_from > 0."
