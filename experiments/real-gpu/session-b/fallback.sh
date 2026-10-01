#!/usr/bin/env bash
# FALLBACK for session B: if the Kubernetes part fails on the VMs, run the same
# cost measurement WITHOUT Kubernetes (torchrun / python straight on the VMs,
# using the Deep Learning AMI's PyTorch), so the paid session still produces
# the numbers. Placement is then done by hand, the way GPUFit would decide it.
#
# Same env as orchestrate.sh (HOSTS, PRIVATE_IPS, SSH_OPTS, RESULTS); run it with
#   bash session-b/fallback.sh     (run.sh points to it if orchestrate.sh fails)
set -euo pipefail
cd "$(dirname "$0")"
source ../../../scripts/guided-lib.sh
read -ra H <<< "$HOSTS"; read -ra P <<< "$PRIVATE_IPS"; read -ra SSHO <<< "$SSH_OPTS"
A=${H[0]} B=${H[1]} PA=${P[0]}
DEGRADED_W=150 REPS=${REPS:-2}
TRAIN_ARGS="" INFER_ARGS=""
[[ "${FAST:-0}" == 1 ]] && TRAIN_ARGS="--steps 5 --warmup-steps 1" INFER_ARGS="--seconds 10 --warmup 3"
on() { local h=$1; shift; ssh "${SSHO[@]}" "$h" "$@"; }
PYBIN=/opt/pytorch/bin   # Deep Learning AMI (sessions A/A2 found torch here)

# Interface that carries the private IP, for NCCL.
iface() { on "$1" "ip -o -4 addr show | awk '\$4 ~ /^$2\\// {print \$2}'"; }

record() {  # record <scenario> <rep> <json> <where>
  jq -cn --arg s "$1" --argjson r "$2" --argjson res "$3" --arg w "$4" \
    '$res + {scenario: $s, rep: $r, placement: {direct: $w}, kubernetes: false}' >> "$RESULTS/results.jsonl"
}

train() {  # train <scenario> <rep> <hosts...>: data-parallel over the given hosts (A first = master)
  local scenario=$1 rep=$2; shift 2
  local n=$# i=0 pids=() out="$RESULTS/fallback-$scenario-$rep"
  for h in "$@"; do
    local ifc; ifc=$(iface "$h" "$(on "$h" "hostname -I | awk '{print \$1}'")")
    on "$h" "NCCL_SOCKET_IFNAME=$ifc NCCL_IB_DISABLE=1 $PYBIN/torchrun --nnodes=$n --nproc-per-node=1 --node-rank=$i \
      --master-addr=$PA --master-port=29500 ~/train_ddp.py $TRAIN_ARGS" > "$out-$i.log" 2>&1 &
    pids+=($!); i=$((i + 1))
  done
  wait "${pids[@]}"
  local res; res=$(grep '^{' "$out-0.log" | tail -1)
  record "$scenario" "$rep" "$res" "$*"
  note "$scenario #$rep: $(jq .tokens_per_s <<< "$res") tokens/s on $*"
}

infer() {  # infer <scenario> <rep> <host>
  local res; res=$(on "$3" "$PYBIN/python ~/infer.py $INFER_ARGS" | grep '^{' | tail -1)
  record "$1" "$2" "$res" "$3"
  note "$1 #$2: $(jq .tokens_per_s <<< "$res") tokens/s on $3"
}

step F1 "Fallback: stop k3s if it got installed (frees the GPUs and ports), copy the workloads to both VMs."
for h in "$A" "$B"; do
  run "on $h 'if [ -x /usr/local/bin/k3s-killall.sh ]; then sudo /usr/local/bin/k3s-killall.sh >/dev/null 2>&1; echo k3s stopped; else echo no k3s; fi'"
  run "scp ${SSHO[*]} workloads/train_ddp.py workloads/infer.py $h:"
done
run "on $B 'sudo nvidia-smi -pl 300 >/dev/null; nvidia-smi --query-gpu=name,power.limit --format=csv,noheader'"
pause

step F2 "Reference, both healthy."
for r in $(seq 1 "$REPS"); do train ref-train-2gpu "$r" "$A" "$B"; done
train ref-train-1gpu 1 "$A"
infer ref-infer 1 "$B"
pause

step F3 "Degrade B to ${DEGRADED_W} W. WITHOUT GPUFit: training on A+B."
run "on $B 'sudo nvidia-smi -pm 1 >/dev/null; sudo nvidia-smi -pl $DEGRADED_W'"
for r in $(seq 1 "$REPS"); do train without-train-2gpu "$r" "$A" "$B"; done
pause

step F4 "WITH GPUFit's placement (applied by hand): training on A, inference on B, at the same time."
for r in $(seq 1 "$REPS"); do
  train with-train-1gpu "$r" "$A" &
  sleep 5
  infer with-infer "$r" "$B"
  wait
done
run "python3 analyze.py $RESULTS ${PRICE_PER_HOUR:-1.006} | tee $RESULTS/summary.md"
