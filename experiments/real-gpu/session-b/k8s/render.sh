# Renders session B's Kubernetes workloads. Source it, then e.g.:
#   render_claims gpu.nvidia.com | kubectl apply -f -
#   render_train train-2gpu 2 pytorch/pytorch:2.12.1-cuda13.0-cudnn9-runtime | kubectl apply -f -
#
# Same YAML for the local rehearsal (kind, CPU, gpu.example.com) and the real
# session (A10Gs, gpu.nvidia.com); only the device class, image and sizes differ.
NS_LAB="${NS_LAB:-gpufit-lab}"
WORKLOADS_DIR="${WORKLOADS_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../workloads" && pwd)}"

# One GPU per pod. Training claims tolerate nothing; inference claims tolerate
# gpufit.io/unfit-training, so a node in GPUFit's inference pool still serves them.
render_claims() {
  local class=$1
  cat <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata: {name: train-gpu, namespace: $NS_LAB}
spec:
  spec:
    devices:
      requests:
      - name: gpu
        exactly: {deviceClassName: $class, allocationMode: ExactCount, count: 1}
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata: {name: infer-gpu, namespace: $NS_LAB}
spec:
  spec:
    devices:
      requests:
      - name: gpu
        exactly:
          deviceClassName: $class
          allocationMode: ExactCount
          count: 1
          tolerations:
          - {key: gpufit.io/unfit-training, operator: Exists, effect: NoSchedule}
EOF
}

# The benchmark scripts, mounted into every pod.
render_workloads_configmap() {
  kubectl create configmap workloads -n "$NS_LAB" --dry-run=client -o yaml \
    --from-file="$WORKLOADS_DIR/train_ddp.py" --from-file="$WORKLOADS_DIR/infer.py"
}

# render_train <name> <gpus> <image> [train_ddp.py args...]
# An Indexed Job with one pod per GPU; pod 0 is the torchrun master, reached
# through a headless Service as <name>-0.<name>. If not every pod can be placed,
# the placed ones wait at the rendezvous: a data-parallel job needs all its GPUs.
render_train() {
  local name=$1 n=$2 image=$3; shift 3
  local args="$*"
  cat <<EOF
apiVersion: v1
kind: Service
metadata: {name: $name, namespace: $NS_LAB}
spec:
  clusterIP: None
  publishNotReadyAddresses: true
  selector: {job-name: $name}
---
apiVersion: batch/v1
kind: Job
metadata: {name: $name, namespace: $NS_LAB}
spec:
  completionMode: Indexed
  completions: $n
  parallelism: $n
  backoffLimit: 0
  template:
    metadata: {labels: {app: gpufit-train, run: $name}}
    spec:
      subdomain: $name
      restartPolicy: Never
      terminationGracePeriodSeconds: 0
      containers:
      - name: train
        image: $image
        command: ["sh", "-c"]
        args:
        - >-
          torchrun --nnodes=$n --nproc-per-node=1 --node-rank=\$JOB_COMPLETION_INDEX
          --master-addr=$name-0.$name --master-port=29500
          /workloads/train_ddp.py $args
        env:
        - {name: NCCL_SOCKET_IFNAME, value: eth0}
        - {name: NCCL_IB_DISABLE, value: "1"}
        - {name: PYTHONUNBUFFERED, value: "1"}
        resources: {claims: [{name: gpu}]}
        volumeMounts: [{name: workloads, mountPath: /workloads}]
      resourceClaims: [{name: gpu, resourceClaimTemplateName: train-gpu}]
      volumes: [{name: workloads, configMap: {name: workloads}}]
EOF
}

# render_infer <name> <image> [infer.py args...]
render_infer() {
  local name=$1 image=$2; shift 2
  local args="$*"
  cat <<EOF
apiVersion: batch/v1
kind: Job
metadata: {name: $name, namespace: $NS_LAB}
spec:
  backoffLimit: 0
  template:
    metadata: {labels: {app: gpufit-infer, run: $name}}
    spec:
      restartPolicy: Never
      terminationGracePeriodSeconds: 0
      containers:
      - name: infer
        image: $image
        command: ["sh", "-c", "python /workloads/infer.py $args"]
        env: [{name: PYTHONUNBUFFERED, value: "1"}]
        resources: {claims: [{name: gpu}]}
        volumeMounts: [{name: workloads, mountPath: /workloads}]
      resourceClaims: [{name: gpu, resourceClaimTemplateName: infer-gpu}]
      volumes: [{name: workloads, configMap: {name: workloads}}]
EOF
}

# The JSON result line a finished job printed (pod 0 for training).
job_result() {
  kubectl logs -n "$NS_LAB" "job/$1" 2>/dev/null | grep '^{' | tail -1
}
