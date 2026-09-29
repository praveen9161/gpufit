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
