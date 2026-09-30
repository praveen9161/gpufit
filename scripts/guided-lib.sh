# Helpers for the guided experiment scripts (experiments/*/run.sh). Source it:
#   source "$(dirname "$0")/../../scripts/guided-lib.sh"
#
# AUTO=1 runs straight through without waiting for Enter.
AUTO="${AUTO:-0}"
NS="${NS:-gpufit-lab}"

step() { printf '\n\033[1;36m━━ STEP %s ━━\033[0m\n' "$1"; shift; printf '%s\n' "$@"; }
run()  { printf '\n\033[1;33m$ %s\033[0m\n' "$*"; eval "$@"; }
note() { printf '\n\033[1;32m→ %s\033[0m\n' "$*"; }
pause() { [[ "$AUTO" == 1 ]] || read -rp $'\n[Enter] to continue, Ctrl-C to stop... ' _; }

# {"gpu-0": <score>, ...} as currently published in the ResourceSlices.
# $1 = workload (default training).
workload_scores() {
  kubectl get resourceslices -o json | jq -c --arg attr "gpufit.io/${1:-training}Score" \
    '[.items[].spec.devices[] | {key: .name, value: .attributes[$attr].int}] | from_entries'
}
training_scores() { workload_scores training; }

# Print "gpu-N (<workload> S), ..." for the GPUs a pod's claim was given, plus
# " via <option>" when the claim used a firstAvailable option.
# $2 = workload whose score to show (default training).
show_got() {
  local pod=$1 workload=${2:-training} claim alloc scores
  claim=$(kubectl get pod -n "$NS" "$pod" -o jsonpath='{.status.resourceClaimStatuses[0].resourceClaimName}')
  alloc=$(kubectl get resourceclaim -n "$NS" "$claim" -o json | jq -c '[.status.allocation.devices.results[]? | {device, request}]')
  scores=$(workload_scores "$workload")
  jq -rn --argjson a "$alloc" --argjson s "$scores" --arg w "$workload" '
    if ($a | length) == 0 then "nothing (not allocated)" else
    ($a | map("\(.device) (\($w) \($s[.device]))") | join(", "))
    + ([$a[].request | select(contains("/")) | split("/")[1]] | unique
       | if length > 0 then "  via option " + join(",") else "" end) end'
}

# Wait until the ResourceSlices show exactly these training scores.
wait_for_scores() {
  local want=$1 got=""
  for _ in $(seq 1 30); do
    got=$(training_scores)
    [[ "$got" == "$want" ]] && { echo "slice updated: $got"; return; }
    sleep 1
  done
  echo "timed out waiting for scores; last seen: $got" >&2
  exit 1
}

# Latest FailedScheduling message for a pod.
why_pending() {
  kubectl get events -n "$NS" --field-selector "involvedObject.name=$1,reason=FailedScheduling" \
    -o jsonpath='{.items[-1:].message}'
}

# ---- hand-set scores (ranking/tiers demos) ------------------------------------
# The in-cluster agent rewrites its node's key in gpufit-scores every 5s, so
# hand-written scores need the agent paused. pause_agent moves the agent pod
# off the node (nodeSelector nobody matches); resume_agent brings it back, and
# it re-publishes the real scores on its first tick.
DRIVER_NS="${DRIVER_NS:-dra-example-driver}"

gpu_node() { kubectl get resourceslices -o jsonpath='{.items[0].spec.nodeName}'; }

pause_agent() {
  kubectl patch ds gpufit-agent -n "$DRIVER_NS" --type merge \
    -p '{"spec":{"template":{"spec":{"nodeSelector":{"gpufit.io/agent-paused":"true"}}}}}'
  kubectl wait pod -n "$DRIVER_NS" -l app=gpufit-agent --for=delete --timeout=60s
}

resume_agent() {
  kubectl patch ds gpufit-agent -n "$DRIVER_NS" --type json \
    -p '[{"op":"remove","path":"/spec/template/spec/nodeSelector"}]'
  kubectl rollout status ds/gpufit-agent -n "$DRIVER_NS" --timeout=90s
}

# set_scores '<scores.json content>': write it as this node's key.
set_scores() {
  kubectl patch cm gpufit-scores -n "$DRIVER_NS" --type merge \
    -p "$(jq -n --arg k "$(gpu_node).json" --arg v "$1" '{data: {($k): $v}}')"
}

# ---- fake GPU health (collector=fake) ----------------------------------------
# set_health gpu-6 nvlink 0.5   -> change one component of one GPU
set_health() {
  local gpu=$1 comp=$2 val=$3 raw
  raw=$(kubectl get cm -n "$DRIVER_NS" gpufit-fake-metrics -o jsonpath='{.data.metrics\.json}' |
        jq -c --arg g "$gpu" --arg c "$comp" --argjson v "$val" '.[$g][$c] = $v')
  kubectl patch cm -n "$DRIVER_NS" gpufit-fake-metrics --type merge \
    -p "$(jq -n --arg m "$raw" '{data: {"metrics.json": $m}}')"
}

# set_all_healthy -> every component of every GPU = 1.0
set_all_healthy() {
  local raw
  raw=$(kubectl get cm -n "$DRIVER_NS" gpufit-fake-metrics -o jsonpath='{.data.metrics\.json}' |
        jq -c 'map_values(map_values(1))')
  kubectl patch cm -n "$DRIVER_NS" gpufit-fake-metrics --type merge \
    -p "$(jq -n --arg m "$raw" '{data: {"metrics.json": $m}}')"
}

# ---- node pools (gpufit-controller) -------------------------------------------
node_pool() { kubectl get node "$(gpu_node)" -o jsonpath='{.metadata.labels.gpufit\.io/pool}'; }

# wait_for_pool inference -> waits until the controller moved the node there
wait_for_pool() {
  local want=$1 got="" start=$SECONDS
  while (( SECONDS - start < 120 )); do
    got=$(node_pool)
    if [[ "$got" == "$want" ]]; then echo "node is in the $got pool (after ~$((SECONDS - start))s)"; return; fi
    sleep 2
  done
  echo "timed out: pool is '$got', wanted '$want'" >&2
  exit 1
}

# gpu_pod <pod-name> <claim-template> -> a pod that just holds its GPUs
gpu_pod() {
  kubectl apply -f - <<YAML
apiVersion: v1
kind: Pod
metadata: {namespace: $NS, name: $1}
spec:
  terminationGracePeriodSeconds: 0
  containers: [{name: ctr, image: "ubuntu:24.04", command: [sleep, "9999"], resources: {claims: [{name: gpus}]}}]
  resourceClaims: [{name: gpus, resourceClaimTemplateName: $2}]
YAML
}
