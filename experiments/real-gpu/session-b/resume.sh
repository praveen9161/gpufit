#!/usr/bin/env bash
# RESUME of session B (generated from orchestrate.sh): reconnects to the EXISTING cluster
# and continues after the 2-GPU reference runs. Used when orchestrate.sh stopped midway.
# Session B, driven FROM THE MAC by ../run.sh after the two VMs exist.
# Plan: docs/design/session-b-cost.md
#
#   k3s on 2 VMs -> NVIDIA DRA driver -> GPUFit (agent on, controller off)
#   reference runs (both GPUs healthy) -> degrade GPU B to 150 W
#   WITHOUT GPUFit: 2-GPU training lands on A+B (straggler)
#   WITH GPUFit:    B -> inference pool; training on A, inference on B
#   collect everything into $RESULTS
#
# Env from run.sh: HOSTS ("ubuntu@ipA ubuntu@ipB"), PRIVATE_IPS, SSH_OPTS, RESULTS, COST_CMD.
# REPS (default 2) repeats the key runs. FAST=1 shrinks the workloads (smoke test).
set -euo pipefail
cd "$(dirname "$0")"
source ../../../scripts/guided-lib.sh
source k8s/render.sh

read -ra H <<< "$HOSTS"
read -ra P <<< "$PRIVATE_IPS"
read -ra SSHO <<< "$SSH_OPTS"
A=${H[0]} B=${H[1]} PA=${P[0]} PB=${P[1]}
K3S_VERSION=v1.37.1+k3s1
NVIDIA_CHART=oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu
NVIDIA_CHART_VERSION=0.5.0
TORCH_IMAGE=pytorch/pytorch:2.12.1-cuda13.0-cudnn9-runtime
DEGRADED_W=150
REPS=${REPS:-2}
TRAIN_ARGS="" INFER_ARGS=""
[[ "${FAST:-0}" == 1 ]] && TRAIN_ARGS="--steps 5 --warmup-steps 1" INFER_ARGS="--seconds 10 --warmup 3"

WORK=$(mktemp -d)
export KUBECONFIG="$WORK/kubeconfig"   # never written into the repo
TUNNEL_PORT=16443
cleanup() {
  [[ -n "${TUNNEL_PID:-}" ]] && kill "$TUNNEL_PID" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

on() { local h=$1; shift; ssh "${SSHO[@]}" "$h" "$@"; }   # on <host> <command>
cost() { eval "$COST_CMD"; }
record() {  # record <scenario> <rep> <json> [extra json fields]
  local extra=${4:-"{}"}
  jq -cn --arg s "$1" --argjson r "$2" --argjson res "$3" --argjson x "$extra" \
    '$res + $x + {scenario: $s, rep: $r}' >> "$RESULTS/results.jsonl"
}
placement() {  # node of each pod of a job, as JSON {"pod": "node", ...}
  kubectl get pods -n "$NS_LAB" -l "job-name=$1" -o json |
    jq -c '[.items[] | {key: .metadata.name, value: (.spec.nodeName // "PENDING")}] | from_entries'
}
node_of() { [[ "$1" == "$NODE_A" ]] && echo A || { [[ "$1" == "$NODE_B" ]] && echo B || echo "$1"; }; }

run_job() {  # run_job <scenario> <rep> <render command...> : apply, wait, record result + placement
  local scenario=$1 rep=$2; shift 2
  local name=$2
  "$@" | kubectl apply -f - >/dev/null
  if ! kubectl wait -n "$NS_LAB" --for=condition=Complete "job/$name" --timeout=20m; then
    note "$name did not complete; pods:"; kubectl get pods -n "$NS_LAB" -l "job-name=$name" -o wide
    kubectl logs -n "$NS_LAB" -l "job-name=$name" --tail=30 --prefix > "$RESULTS/failed-$scenario-$rep.log" 2>&1 || true
    return 1
  fi
  local res pl; res=$(job_result "$name"); pl=$(placement "$name")
  record "$scenario" "$rep" "$res" "{\"placement\": $pl}"
  note "$scenario #$rep: $(jq -r '"\(.workload) \(.tokens_per_s) tokens/s"' <<< "$res")  on $(jq -r '[.[]] | join(",")' <<< "$pl" | sed "s/$NODE_A/A/g; s/$NODE_B/B/g")"
  kubectl delete job,svc -n "$NS_LAB" "$name" --ignore-not-found --wait >/dev/null 2>&1
}

# ---------------------------------------------------------------------------------
step R "Reconnect to the running cluster (no reinstall)."
on "$A" cat /etc/rancher/k3s/k3s.yaml | sed "s#https://127.0.0.1:6443#https://127.0.0.1:$TUNNEL_PORT#" > "$KUBECONFIG"
ssh "${SSHO[@]}" -fN -L "$TUNNEL_PORT:127.0.0.1:6443" -o ExitOnForwardFailure=yes "$A"
TUNNEL_PID=$(pgrep -f "$TUNNEL_PORT:127.0.0.1:6443" | head -1)
for _ in $(seq 1 60); do [[ $(kubectl get nodes --no-headers 2>/dev/null | grep -c ' Ready') == 2 ]] && break; sleep 3; done
run kubectl get nodes -o wide
NODE_A=$(kubectl get nodes -o json | jq -r --arg ip "$PA" '.items[] | select(.status.addresses[] | .address == $ip) | .metadata.name')
NODE_B=$(kubectl get nodes -o json | jq -r --arg ip "$PB" '.items[] | select(.status.addresses[] | .address == $ip) | .metadata.name')
note "A = $NODE_A, B = $NODE_B"
pause

step B4 "REFERENCE (continued): 1-GPU training and inference (the 2-GPU references are already recorded)."
run_job ref-train-1gpu 1 render_train train-1gpu 1 "$TORCH_IMAGE" $TRAIN_ARGS
run_job ref-infer 1 render_infer infer "$TORCH_IMAGE" $INFER_ARGS
note "($(cost))"
pause

step B5 "Degrade GPU B: power limit 300 W -> ${DEGRADED_W} W. Watch the agent on B rescore it."
run "on $B 'sudo nvidia-smi -pm 1 >/dev/null; sudo nvidia-smi -pl $DEGRADED_W'"
sleep 15
run "kubectl logs -n gpufit -l app=gpufit-agent --prefix --tail=2 | grep -i -E 'gpu-|training' | tail -4"
pause

step B6 "WITHOUT GPUFit (controller off): Kubernetes can't know B is slow; the 2-GPU job uses A+B."
for r in $(seq 1 "$REPS"); do run_job without-train-2gpu "$r" render_train train-2gpu 2 "$TORCH_IMAGE" $TRAIN_ARGS; done
note "($(cost))"
pause

step B7 "WITH GPUFit: controller on -> B moves to the inference pool (device taint)."
run kubectl scale deploy/gpufit-controller -n gpufit --replicas=1
for _ in $(seq 1 60); do [[ $(kubectl get node "$NODE_B" -o jsonpath='{.metadata.labels.gpufit\.io/pool}') == inference ]] && break; sleep 3; done
run "kubectl get nodes -L gpufit.io/pool"
run "kubectl get devicetaintrules -o custom-columns=NAME:.metadata.name,POOL:.spec.deviceSelector.pool,TAINT:.spec.taint.key"
note "A 2-GPU training job now can't get 2 fit GPUs: one pod stays Pending (a data-parallel job needs all its GPUs)."
render_train train-2gpu 2 "$TORCH_IMAGE" $TRAIN_ARGS | kubectl apply -f - >/dev/null
sleep 25
run "kubectl get pods -n $NS_LAB -l job-name=train-2gpu -o wide | awk '{print \$1, \$3, \$7}'"
pending=$(kubectl get pods -n "$NS_LAB" -l job-name=train-2gpu --field-selector=status.phase=Pending -o name | head -1 | cut -d/ -f2)
if [[ -n "$pending" ]]; then
  (cd ../../../agent && go build -o "$WORK" ./cmd/kubectl-gpufit)
  run "$WORK/kubectl-gpufit why $pending -n $NS_LAB | tee $RESULTS/why-train-2gpu.txt"
fi
kubectl delete job,svc -n "$NS_LAB" train-2gpu --wait >/dev/null
pause

step B8 "WITH GPUFit, steady state: training on the healthy GPU (A), inference on the degraded one (B)."
for r in $(seq 1 "$REPS"); do
  render_train train-1gpu 1 "$TORCH_IMAGE" $TRAIN_ARGS | kubectl apply -f - >/dev/null
  sleep 10   # let training take A first; inference then only fits on B
  render_infer infer "$TORCH_IMAGE" $INFER_ARGS | kubectl apply -f - >/dev/null
  kubectl wait -n "$NS_LAB" --for=condition=Complete job/train-1gpu job/infer --timeout=20m
  record with-train-1gpu "$r" "$(job_result train-1gpu)" "{\"placement\": $(placement train-1gpu)}"
  record with-infer "$r" "$(job_result infer)" "{\"placement\": $(placement infer)}"
  note "with GPUFit #$r: train $(job_result train-1gpu | jq .tokens_per_s) tokens/s on $(node_of "$(placement train-1gpu | jq -r '[.[]][0]')"), infer $(job_result infer | jq .tokens_per_s) tokens/s on $(node_of "$(placement infer | jq -r '[.[]][0]')")"
  kubectl delete job -n "$NS_LAB" train-1gpu infer --wait >/dev/null
done
note "($(cost))"
pause

step B9 "Collect: placement evidence, GPUFit logs, events, and the cost summary."
kubectl get nodes -L gpufit.io/pool -o wide > "$RESULTS/nodes.txt"
kubectl get devicetaintrules -o yaml > "$RESULTS/devicetaintrules.yaml"
kubectl get events -A --sort-by=.lastTimestamp > "$RESULTS/events.txt"
kubectl logs -n gpufit -l app=gpufit-agent --prefix --tail=-1 > "$RESULTS/agent.log"
kubectl logs -n gpufit deploy/gpufit-controller --tail=-1 > "$RESULTS/controller.log"
kubectl get cm -n gpufit gpufit-scores -o json | jq '.data' > "$RESULTS/scores.json"
kubectl get resourceslices -o yaml > "$RESULTS/resourceslices.yaml"
jq -n --arg a "$NODE_A" --arg b "$NODE_B" --argjson w "$DEGRADED_W" '{node_a: $a, node_b: $b, degraded: "B", degraded_power_w: $w}' > "$RESULTS/setup.json"
run "python3 analyze.py $RESULTS ${PRICE_PER_HOUR:-1.006} | tee $RESULTS/summary.md"
