#!/usr/bin/env bash
# gpu-dash.sh — live view of simulated GPUs, pods and claims.
#
#   ./scripts/gpu-dash.sh            # refresh every 2s (Ctrl-C to quit)
#   ./scripts/gpu-dash.sh --once     # print one snapshot
#   NS=other ./scripts/gpu-dash.sh   # watch another namespace (default gpufit-lab)
#
# Every number here comes straight from the API server:
#   GPUs      <- ResourceSlices        (what the driver advertises, incl. GPUFit scores)
#   who owns  <- ResourceClaims        (.status.allocation + .status.reservedFor)
#   taints    <- DeviceTaintRules
#   why stuck <- FailedScheduling events
set -euo pipefail
NS="${NS:-gpufit-lab}"
INTERVAL="${INTERVAL:-2}"

snapshot() {
  local slices claims rules pods events
  slices=$(kubectl get resourceslices -o json)
  claims=$(kubectl get resourceclaims -A -o json)
  rules=$(kubectl get devicetaintrules -o json 2>/dev/null || echo '{"items":[]}')
  pods=$(kubectl get pods -n "$NS" -o json)
  events=$(kubectl get events -n "$NS" --field-selector reason=FailedScheduling -o json)

  echo "GPUFit dashboard — $(date '+%H:%M:%S')   namespace: $NS"
  echo

  # ---- summary --------------------------------------------------------------
  jq -rn --argjson s "$slices" --argjson c "$claims" --argjson p "$pods" '
    ([$s.items[].spec.devices[]] | length) as $total
    | ([$c.items[].status.allocation.devices.results[]?] | length) as $used
    | ($p.items | map(.status.phase)) as $ph
    | ($ph | map(select(. == "Running")) | length) as $run
    | ($ph | map(select(. == "Pending")) | length) as $pend
    | "GPUs: \($used)/\($total) allocated, \($total - $used) free   |   Pods: \($run) Running, \($pend) Pending"'
  echo

  # ---- per-GPU table ----------------------------------------------------------
  echo "== GPUs =="
  jq -rn --argjson s "$slices" --argjson c "$claims" --argjson r "$rules" '
    # device -> {claim, pod}
    ([ $c.items[] | . as $cl
       | .status.allocation.devices.results[]?
       | {key: .device, value: {claim: "\($cl.metadata.namespace)/\($cl.metadata.name)",
                                pod: ([$cl.status.reservedFor[]?.name] | join(",") | if . == "" then "-" else . end)}} ]
     | from_entries) as $alloc
    | ["DEVICE","STATE","POD","TRAIN","FTUNE","INFER","TAINT"],
      ( $s.items[] | .spec.pool.name as $pool | .spec.driver as $drv
        | .spec.devices[] | .name as $d
        | ([ $r.items[] | .spec as $sp
             | select(($sp.deviceSelector.device // $d) == $d
                      and ($sp.deviceSelector.pool // $pool) == $pool
                      and ($sp.deviceSelector.driver // $drv) == $drv)
             | "\($sp.taint.key)=\($sp.taint.value // ""):\($sp.taint.effect)" ] | join(" ")) as $taint
        | [ $d,
            (if $alloc[$d] then "USED" else "free" end),
            ($alloc[$d].pod // "-"),
            # GPUFit scores published by the driver as gpufit.io/<workload>Score
            (.attributes["gpufit.io/trainingScore"].int // "-" | tostring),
            (.attributes["gpufit.io/finetuneScore"].int // "-" | tostring),
            (.attributes["gpufit.io/inferenceScore"].int // "-" | tostring),
            (if $taint == "" then "-" else $taint end) ] )
    | @tsv' | column -t -s $'\t'
  echo

  # ---- pods -----------------------------------------------------------------
  echo "== Pods in $NS =="
  jq -rn --argjson p "$pods" --argjson c "$claims" '
    # ["gpu-0","gpu-1","gpu-2","gpu-5"] -> "gpu 0-2,5" so the table fits 80 columns
    def compact:
      if length == 0 then "-"
      elif all(test("^gpu-[0-9]+$")) then
        map(ltrimstr("gpu-") | tonumber) | sort
        | reduce .[] as $n ([]; if length > 0 and .[-1][1] + 1 == $n then .[-1][1] = $n else . + [[$n, $n]] end)
        | map(if .[0] == .[1] then "\(.[0])" else "\(.[0])-\(.[1])" end)
        | "gpu " + join(",")
      else join(",") end;
    if ($p.items | length) == 0 then "(none)" else
    ["POD","PHASE","ASKED","GOT","CLAIM"],
    ( $p.items[] | .metadata.name as $pn
      # claims generated from a template are owned by their pod
      | ([ $c.items[] | select(.metadata.ownerReferences // [] | map(.name) | index($pn) != null) ]) as $mine
      | [ $pn,
          .status.phase,
          ([ $mine[].spec.devices.requests[]? | .exactly.count // 1 ] | add // 0 | tostring),
          ([ $mine[].status.allocation.devices.results[]?.device ] | compact),
          ([ $mine[] | if .status.allocation then "allocated" else "pending" end ] | join(",") | if . == "" then "-" else . end) ] )
    | @tsv end' | column -t -s $'\t'
  echo

  # ---- why pending ------------------------------------------------------------
  echo "== Why Pending (latest scheduler message per pending pod) =="
  jq -rn --argjson p "$pods" --argjson e "$events" '
    [ $p.items[] | select(.status.phase == "Pending") | .metadata.name ] as $pend
    | if ($pend | length) == 0 then "(nothing pending)" else
      $pend[] as $n
      | ([ $e.items[] | select(.involvedObject.name == $n) ]
         | sort_by(.lastTimestamp // .eventTime // .metadata.creationTimestamp) | last) as $ev
      | "\($n): \($ev.message // "(no event yet)")" end'
}

if [[ "${1:-}" == "--once" ]]; then
  snapshot
  exit
fi

while true; do
  out=$(snapshot 2>&1)
  clear
  echo "$out"
  echo
  echo "(refreshing every ${INTERVAL}s — Ctrl-C to quit)"
  sleep "$INTERVAL"
done
