#!/usr/bin/env bash
# Session C, driven FROM THE MAC by ../run.sh after the four VMs exist.
#
#   k3s on 4 VMs -> NVIDIA DRA driver -> GPUFit via the Helm chart (observe)
#   R0 reference 2-GPU job -> R1 observe, one GPU degraded mid-job (straggler)
#   -> helm upgrade to enforce + rescue -> R2 same degradation, GPUFit moves the job
#   to the two spare healthy GPUs, it resumes from its checkpoint -> analyse
#
# Rehearse first: test/rehearse-kind.sh runs the same experiment.sh on kind.
# Env from run.sh: HOSTS, PRIVATE_IPS, SSH_OPTS, RESULTS, COST_CMD, PRICE_PER_HOUR.
set -euo pipefail
cd "$(dirname "$0")"
source ../../../scripts/guided-lib.sh
source ../session-b/k8s/render.sh

read -ra H <<< "$HOSTS"
read -ra P <<< "$PRIVATE_IPS"
read -ra SSHO <<< "$SSH_OPTS"
(( ${#H[@]} == 4 )) || { echo "session C needs 4 VMs, got ${#H[@]}"; exit 1; }
K3S_VERSION=v1.37.1+k3s1
NVIDIA_CHART=oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu
NVIDIA_CHART_VERSION=0.5.0
TORCH_IMAGE=pytorch/pytorch:2.12.1-cuda13.0-cudnn9-runtime
DEGRADED_W=${DEGRADED_W:-150}   # session B: 150 W made the 2-GPU job run at 0.84

# For run_job/experiment.sh
CLASS=gpu.nvidia.com
IMAGE=$TORCH_IMAGE
# Session B's model (134 M params, ~2.0 s/step on 2x A10G): 400 steps ~ 13-14 min healthy.
# Checkpoints are ~1.1 GB (weights + SGD momentum), so every 50 steps.
TRAIN_ARGS=${TRAIN_ARGS:-"--total-steps 400 --ckpt-every 50"}
DEGRADE_AT=${DEGRADE_AT:-100}
CHART=../../../charts/gpufit

WORK=$(mktemp -d)
export KUBECONFIG="$WORK/kubeconfig"   # never written into the repo
TUNNEL_PORT=16443
cleanup() {
  kill $(jobs -p) 2>/dev/null || true
  [[ -n "${TUNNEL_PID:-}" ]] && { kill "$TUNNEL_PID" 2>/dev/null || true; }
  rm -rf "$WORK"
}
trap cleanup EXIT
on() { local h=$1; shift; ssh "${SSHO[@]}" "$h" "$@"; }
cost() { eval "$COST_CMD"; }
host_of() {  # host_of <node name>: ssh target of the VM behind a Kubernetes node
  local ip; ip=$(kubectl get node "$1" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
  for i in "${!P[@]}"; do [[ "${P[$i]}" == "$ip" ]] && { echo "${H[$i]}"; return; }; done
  echo "no VM for node $1 ($ip)" >&2; return 1
}
degrade_node() {
  run "on $(host_of "$1") 'sudo nvidia-smi -pm 1 >/dev/null; sudo nvidia-smi -pl $DEGRADED_W | tail -1'"
}
repair_node() {
  run "on $(host_of "$1") 'sudo nvidia-smi -pl \$(nvidia-smi --query-gpu=power.default_limit --format=csv,noheader,nounits | cut -d. -f1) | tail -1'"
}

# ---------------------------------------------------------------------------------
step C1 "Kubernetes (k3s $K3S_VERSION): server on VM 1, agents on VMs 2-4, over the private network."
# k3s' default pod network (10.42.0.0/16) overlaps the VPC: use ranges that can't clash (session B).
K3S_NET="--cluster-cidr 10.244.0.0/16 --service-cidr 10.96.0.0/16"
on "${H[0]}" 'if [ -x /usr/local/bin/k3s-uninstall.sh ]; then sudo /usr/local/bin/k3s-uninstall.sh >/dev/null 2>&1; fi; true'
for h in "${H[@]:1}"; do on "$h" 'if [ -x /usr/local/bin/k3s-agent-uninstall.sh ]; then sudo /usr/local/bin/k3s-agent-uninstall.sh >/dev/null 2>&1; fi; true'; done
run "on ${H[0]} 'curl -sfL https://get.k3s.io | sudo INSTALL_K3S_VERSION=$K3S_VERSION sh -s - server --node-ip ${P[0]} $K3S_NET --disable traefik --disable servicelb --write-kubeconfig-mode 644 >/dev/null && sudo k3s --version | head -1'"
TOKEN=$(on "${H[0]}" sudo cat /var/lib/rancher/k3s/server/node-token)
note "Joining VMs 2-4 with the cluster token (not printed)."
for i in 1 2 3; do
  on "${H[$i]}" "curl -sfL https://get.k3s.io | sudo INSTALL_K3S_VERSION=$K3S_VERSION K3S_URL=https://${P[0]}:6443 K3S_TOKEN=$TOKEN sh -s - agent --node-ip ${P[$i]} >/dev/null && echo agent $i installed"
done
on "${H[0]}" cat /etc/rancher/k3s/k3s.yaml | sed "s#https://127.0.0.1:6443#https://127.0.0.1:$TUNNEL_PORT#" > "$KUBECONFIG"
ssh "${SSHO[@]}" -fN -L "$TUNNEL_PORT:127.0.0.1:6443" -o ExitOnForwardFailure=yes "${H[0]}"
TUNNEL_PID=$(pgrep -f "$TUNNEL_PORT:127.0.0.1:6443" | head -1)
for _ in $(seq 1 60); do [[ $(kubectl get nodes --no-headers 2>/dev/null | grep -c ' Ready') == 4 ]] && break; sleep 3; done
run kubectl get nodes -o wide
note "Pulling the PyTorch image (~3 GB) on all nodes in the background."
for h in "${H[@]}"; do on "$h" "nohup sudo k3s crictl pull $TORCH_IMAGE > /tmp/pull.log 2>&1 &"; done
note "($(cost))"
pause

step C2 "NVIDIA's DRA driver ($NVIDIA_CHART_VERSION) and the GPUFit image on all nodes."
run "kubectl label nodes --all nvidia.com/gpu.present=true --overwrite"
run "helm install dra-driver-nvidia-gpu $NVIDIA_CHART --version $NVIDIA_CHART_VERSION --create-namespace --namespace dra-driver-nvidia-gpu --set gpuResourcesEnabledOverride=true --set nvidiaDriverRoot=/ --wait --timeout 5m 2>&1 | tail -4"
for _ in $(seq 1 60); do [[ $(kubectl get resourceslices -o json | jq '[.items[] | select(.spec.driver == "gpu.nvidia.com") | .spec.devices[]] | length') -ge 4 ]] && break; sleep 3; done
run "kubectl get resourceslices -o json | jq -r '.items[] | select(.spec.driver == \"gpu.nvidia.com\") | .spec.pool.name as \$p | .spec.devices[] | \"\(\$p)  \(.name)  \(.attributes.productName.string // \"\")\"'"
run "(cd ../../../ && ARCH=amd64 SAVE=$WORK/gpufit.tar ./deploy/agent/build.sh >/dev/null 2>&1) && ls -lh $WORK/gpufit.tar | awk '{print \$5}'"
for h in "${H[@]}"; do run "on $h 'sudo k3s ctr -n k8s.io images import -' < $WORK/gpufit.tar | tail -1"; done
note "Waiting for the PyTorch image on all nodes..."
for h in "${H[@]}"; do
  for _ in $(seq 1 120); do on "$h" "sudo k3s crictl images | grep -q 2.12.1-cuda13.0" && break; sleep 5; done
done
run "for h in ${H[*]}; do ssh ${SSH_OPTS} \$h 'sudo k3s crictl images | grep -c pytorch'; done | paste -sd' ' -"
note "($(cost))"
pause

# GPUFit from the chart in this repo (same as the published one), with the image just imported.
# Short timers so a run fits in minutes; the analysis notes what production timers would add.
HELM_VALUES=(--set image.repository=gpufit-agent --set image.tag=dev --set image.pullPolicy=Never
  --set driver=gpu.nvidia.com --set pricePerGpuHour="$PRICE_PER_HOUR"
  --set agent.window=30s --set pools.degradeAfter=20s --set pools.recoverAfter=60s --set rescue.grace=30s)

source experiment.sh
