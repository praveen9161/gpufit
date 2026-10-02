# Session C runs, sourced by orchestrate.sh (4x A10G) and test/rehearse-kind.sh (kind, CPU),
# so the rehearsal exercises exactly the code the paid session runs.
#
#   R0 reference:       2-GPU training job, all GPUs healthy                    -> T0
#   R1 observe mode:    same job, one of its GPUs degraded mid-run; GPUFit only reports -> T1
#   R2 enforce+rescue:  same job, same degradation; GPUFit moves the job to healthy GPUs -> T2
#
# Measured: T1-T0 (straggler cost), T2-T0 (rescue cost and where it goes), the report's
# waste estimate vs the measured waste.
#
# The caller sets: CLASS, IMAGE, TRAIN_ARGS, DEGRADE_AT (step), CHART, HELM_VALUES (array),
# RESULTS, WORK, PRICE_PER_HOUR, and functions degrade_node <node> <gpu>, repair_node <node>, cost.
# Needs guided-lib.sh and session-b/k8s/render.sh sourced.

GPUFIT_NS=gpufit
mkdir -p "$RESULTS/logs"
(cd "$(dirname "${BASH_SOURCE[0]}")/../../../agent" && go build -o "$WORK" ./cmd/kubectl-gpufit)
kubectl-gpufit() { "$WORK/kubectl-gpufit" "$@"; }

now() { date +%s; }
mark() {  # mark <run> <event> [node]: one line of the timeline, Mac clock
  jq -cn --arg r "$1" --arg e "$2" --arg n "${3:-}" --argjson t "$(now)" '{run: $r, event: $e, node: $n, time: $t}' >> "$RESULTS/timeline.jsonl"
}

# Every pod's log is streamed into $RESULTS/logs/<pod>.log, so the logs of evicted pods survive.
follow_logs() {  # follow_logs <job>: runs in the background until the job's pods are gone
  local job=$1 seen=" "
  while kubectl get job -n "$NS_LAB" "$job" >/dev/null 2>&1; do
    for p in $(kubectl get pods -n "$NS_LAB" -l "job-name=$job" --field-selector=status.phase=Running -o name 2>/dev/null | cut -d/ -f2); do
      [[ "$seen" == *" $p "* ]] && continue
      seen+="$p "
      kubectl logs -f -n "$NS_LAB" "$p" > "$RESULTS/logs/$p.log" 2>&1 &
    done
    sleep 2
  done
}
rank0_events() { cat "$RESULTS/logs/$1"-0-*.log 2>/dev/null | grep '^{' || true; }
wait_for_step() {  # wait_for_step <job> <step>: until rank 0 checkpointed at least <step>
  for _ in $(seq 1 1800); do
    rank0_events "$1" | jq -e --argjson s "$2" 'select(.event=="checkpoint" and .step >= $s)' >/dev/null 2>&1 && return 0
    sleep 2
  done
  echo "timed out waiting for step $2 of $1"; return 1
}
pod_node() {  # pod_node <job> <index>: node of the running pod with that completion index
  kubectl get pods -n "$NS_LAB" -l "job-name=$1,batch.kubernetes.io/job-completion-index=$2" \
    --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.nodeName}'
}
# Running and not terminating: an evicted pod still says Running while it shuts down.
live_pods() { kubectl get pods -n "$NS_LAB" -l "job-name=$1" --field-selector=status.phase=Running -o json | jq -c '[.items[] | select(.metadata.deletionTimestamp == null)]'; }
pod_gpu() {  # pod_gpu <job> <index>: the DRA device allocated to that pod, e.g. gpu-0
  local pod claim
  pod=$(live_pods "$1" | jq -r --arg i "$2" '.[] | select(.metadata.labels["batch.kubernetes.io/job-completion-index"] == $i) | .metadata.name' | head -1)
  claim=$(kubectl get pod -n "$NS_LAB" "$pod" -o jsonpath='{.status.resourceClaimStatuses[0].resourceClaimName}')
  kubectl get resourceclaim -n "$NS_LAB" "$claim" -o jsonpath='{.status.allocation.devices.results[0].device}'
}
placement() { live_pods "$1" | jq -c '[.[].spec.nodeName] | sort'; }
job_wall_s() {
  kubectl get job -n "$NS_LAB" "$1" -o json |
    jq '(.status.completionTime | fromdateiso8601) - (.status.startTime | fromdateiso8601)'
}
gpu_nodes() { kubectl get resourceslices -o json | jq -r --arg d "$CLASS" '[.items[] | select(.spec.driver == $d) | .spec.pool.name] | unique[]'; }
all_in_pool() {  # all_in_pool training: every GPU node labelled with this pool (enforce mode)
  for n in $(gpu_nodes); do
    [[ $(kubectl get node "$n" -o jsonpath='{.metadata.labels.gpufit\.io/pool}') == "$1" ]] || return 1
  done
}
status_json() { kubectl get cm -n "$GPUFIT_NS" gpufit-status -o jsonpath='{.data.status\.json}'; }
all_scores_healthy() {  # every GPU's training score >= 80 in the controller's view
  status_json | jq -e '[.nodes[].scores[].training] | length > 0 and all(. >= 80)' >/dev/null 2>&1
}
wait_until() {  # wait_until <seconds> <command...>
  local t=$1; shift
  for _ in $(seq 1 "$t"); do "$@" && return 0; sleep 1; done
  echo "timed out after ${t}s: $*"; return 1
}

start_job() {  # start_job <name>: the 2-GPU resumable training job, logs followed
  render_train_job "$1" 2 "$IMAGE" $TRAIN_ARGS | kubectl apply -f - >/dev/null
  follow_logs "$1" &
  wait_until 600 bash -c "[[ \$(kubectl get pods -n $NS_LAB -l job-name=$1 --field-selector=status.phase=Running -o name | wc -l) -ge 2 ]]"
  note "$1 running on $(placement "$1")"
}
finish_job() {  # finish_job <run> <name> <degraded node or ""> <placement before> : wait, record
  local run=$1 job=$2 deg=$3 before=$4
  kubectl wait -n "$NS_LAB" --for=condition=complete "job/$job" --timeout=60m >/dev/null
  local wall; wall=$(job_wall_s "$job")
  jq -cn --arg r "$run" --arg j "$job" --arg d "$deg" --argjson w "$wall" --argjson b "$before" \
    --argjson a "$(kubectl get pods -n "$NS_LAB" -l "job-name=$job" -o json | jq -c '[.items[] | select(.status.phase=="Succeeded") | .spec.nodeName] | sort')" \
    '{run: $r, job: $j, degraded_node: $d, wall_s: $w, placement_start: $b, placement_end: $a}' >> "$RESULTS/runs.jsonl"
  note "$run: $job finished in ${wall}s   ($(cost))"
}

# ---------------------------------------------------------------------------------
step C3 "Install GPUFit with the Helm chart, OBSERVE mode (the default)."
run "helm install gpufit $CHART -n $GPUFIT_NS --create-namespace ${HELM_VALUES[*]} --wait --timeout 5m 2>&1 | grep -E '^(STATUS|NAME)'"
run "kubectl get pods -n $GPUFIT_NS -o wide | awk '{print \$1, \$3, \$7}'"
wait_until 180 all_scores_healthy
run "kubectl-gpufit status -n $GPUFIT_NS"
run "kubectl create ns $NS_LAB --dry-run=client -o yaml | kubectl apply -f - >/dev/null; render_workloads_configmap | kubectl apply -f -; render_claims $CLASS | kubectl apply -f -"
run "render_ckpt_store $IMAGE | kubectl apply -f - && kubectl rollout status deploy/ckpt-store -n $NS_LAB --timeout=10m"
note "($(cost))"
pause

step C4 "R0 REFERENCE: the 2-GPU training job on healthy GPUs."
start_job train-ref
finish_job R0 train-ref "" "$(placement train-ref)"
pause

step C5 "R1 OBSERVE: same job; at step $DEGRADE_AT one of its GPUs degrades. GPUFit only reports."
start_job train-observe
before=$(placement train-observe)
wait_for_step train-observe "$DEGRADE_AT"
Y=$(pod_node train-observe 1); JOB_NOW=train-observe
degrade_node "$Y" "$(pod_gpu "$JOB_NOW" 1)"; mark R1 degrade "$Y"
note "Degraded $Y. The job keeps running on it (straggler); GPUFit should say it WOULD act."
sleep 60
run "kubectl-gpufit status -n $GPUFIT_NS | head -20"
finish_job R1 train-observe "$Y" "$before"
status_json > "$RESULTS/status-after-R1.json"
run "kubectl-gpufit report -n $GPUFIT_NS | tee $RESULTS/report-after-R1.txt"
repair_node "$Y"; mark R1 repair "$Y"
pause

step C6 "Switch GPUFit to ENFORCE + RESCUE with one helm upgrade."
run "helm upgrade gpufit $CHART -n $GPUFIT_NS --reuse-values --set mode=enforce,rescue.enabled=true --wait 2>&1 | grep -E '^STATUS' || true"
note "Waiting until every GPU scores healthy again and every node is in the training pool..."
wait_until 600 all_scores_healthy
wait_until 600 all_in_pool training
run "kubectl get nodes -L gpufit.io/pool"
note "($(cost))"
pause

step C7 "R2 ENFORCE + RESCUE: same job, same degradation at step $DEGRADE_AT." \
       "Expect: node -> inference pool, the job is evicted after the grace period," \
       "recreated on two healthy GPUs, and resumes from its last checkpoint."
start_job train-rescue
before=$(placement train-rescue)
wait_for_step train-rescue "$DEGRADE_AT"
Y=$(pod_node train-rescue 1); JOB_NOW=train-rescue
ORIG=$(kubectl get pods -n "$NS_LAB" -l job-name=train-rescue -o jsonpath='{.items[*].metadata.name}')
degrade_node "$Y" "$(pod_gpu "$JOB_NOW" 1)"; mark R2 degrade "$Y"
wait_until 600 bash -c "[[ \$(kubectl get node $Y -o jsonpath='{.metadata.labels.gpufit\.io/pool}') == inference ]]"; mark R2 pool_inference "$Y"
note "$Y left the training pool; waiting for the rescue..."
evicted() { for p in $ORIG; do [[ -z $(kubectl get pod -n "$NS_LAB" "$p" -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null) ]] && kubectl get pod -n "$NS_LAB" "$p" >/dev/null 2>&1 && return 1; done; return 0; }
wait_until 600 evicted; mark R2 evicted "$Y"
run "kubectl logs -n $GPUFIT_NS deploy/gpufit-controller | grep 'rescue:'"
repair_node "$Y"; mark R2 repair "$Y"   # the job is no longer there; on a 1-node rehearsal this lets it resume
replaced() { [[ $(live_pods train-rescue | jq --arg o " $ORIG " '[.[] | select(.metadata.name as $n | $o | contains(" " + $n + " ") | not)] | length') -ge 2 ]]; }
wait_until 900 replaced
mark R2 running_again; note "Running again on $(placement train-rescue)"
finish_job R2 train-rescue "$Y" "$before"
run "rank0_events train-rescue | jq -c 'select(.event==\"start\")'"
run "kubectl-gpufit report -n $GPUFIT_NS | tee $RESULTS/report-after-R2.txt"
pause

step C8 "Collect logs and analyse."
status_json > "$RESULTS/status-final.json"
kubectl logs -n "$GPUFIT_NS" deploy/gpufit-controller --tail=-1 > "$RESULTS/controller.log"
kubectl logs -n "$GPUFIT_NS" -l app.kubernetes.io/component=agent --prefix --tail=-1 > "$RESULTS/agent.log"
kubectl get events -A --sort-by=.lastTimestamp > "$RESULTS/events.txt"
helm get values gpufit -n "$GPUFIT_NS" -o yaml > "$RESULTS/helm-values.yaml"
jq -n --arg c "$CLASS" --arg t "$TRAIN_ARGS" --argjson d "$DEGRADE_AT" --argjson p "${PRICE_PER_HOUR:-0}" \
  '{class: $c, train_args: $t, degrade_at_step: $d, price_per_gpu_hour: $p}' > "$RESULTS/setup.json"
run "python3 $(dirname "${BASH_SOURCE[0]}")/analyze.py $RESULTS | tee $RESULTS/summary.md"
kill $(jobs -p) 2>/dev/null || true
