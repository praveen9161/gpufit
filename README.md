# GPUFit

Workload-aware GPU fitness scoring for Kubernetes.

Today a GPU is either healthy or unhealthy. GPUFit gives each GPU a score per
workload type (training, fine-tuning, inference) from its compute, memory, NVLink,
network and thermal health, and publishes those scores as
[DRA](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
device attributes. Jobs then pick GPUs by fitness:

```yaml
selectors:
- cel:
    expression: device.attributes["gpufit.io"].trainingScore >= 80
```

A GPU with broken NVLink is kept away from distributed training but still used
for inference instead of being wasted.

**Status:** early prototype, working end-to-end on simulated GPUs
([dra-example-driver](https://github.com/kubernetes-sigs/dra-example-driver)) in kind.
Scores currently come from hand-written fake metrics. Real-hardware measurements
are next.

## Layout

| Path | What |
|------|------|
| `agent/` | Go: fitness scoring (`internal/fitness`), health collectors (`internal/collect`), `gpufit-agent` (per-node loop: collect -> score -> publish), `gpufit-claim` (writes tiered claim templates) |
| `deploy/agent/` | Agent image build, DaemonSet + least-privilege RBAC, fake metrics ConfigMap |
| `deploy/controller/` | `gpufit-controller`: moves nodes between training/inference pools, marks GPUs for repair (device taints, hysteresis) |
| `agent/cmd/kubectl-gpufit` | `kubectl gpufit why <pod>`: explains why a DRA GPU claim is Pending (busy vs unfit vs tainted), with scores |
| `driver-patch/` | Patches for dra-example-driver: publish each node's scores as `gpufit.io/<workload>Score` device attributes |
| `experiments/` | Manifests for each experiment (Pending pods, device taints, fitness scheduling) |
| `scripts/gpu-dash.sh` | Live terminal view of GPUs, scores, pods and claims |
| `notes.md` | Experiment log and design notes |

## Run it

Needs kind, kubectl, helm, Go and Docker. Setup of the kind cluster is covered
in dra-example-driver's README.

```bash
# driver with the GPUFit patch (base: upstream 812a3f2)
git clone https://github.com/kubernetes-sigs/dra-example-driver
cd dra-example-driver
git checkout -b gpufit 812a3f2
git am ../driver-patch/*.patch
DRIVER_IMAGE_TAG=v0.5.0-gpufit-2 ./demo/scripts/build-driver-image.sh
DRIVER_IMAGE_TAG=v0.5.0-gpufit-2 ./demo/scripts/load-driver-image-into-kind.sh
helm upgrade --install dra-example-driver deployments/helm/dra-example-driver \
  -n dra-example-driver --create-namespace \
  --set image.tag=v0.5.0-gpufit-2 --set kubeletPlugin.gpufitScoresConfigMap=gpufit-scores
cd ..

# agent: one pod per GPU node, publishes scores every 5s
./deploy/agent/build.sh
kubectl apply -f deploy/agent/fake-metrics.yaml -f deploy/agent/agent.yaml
kubectl logs -n dra-example-driver -l app=gpufit-agent

# schedule by fitness
kubectl create namespace gpufit-lab
(cd agent && go run ./cmd/gpufit-claim -workload training -min 80 -count 6 | kubectl apply -f -)
kubectl apply -f experiments/claim-generator/11-train-job.yaml
./scripts/gpu-dash.sh --once
```

Or walk through it step by step (keep `./scripts/gpu-dash.sh` open in a second terminal):

- `./experiments/claim-generator/run.sh`: training vs inference placement
- `./experiments/in-cluster-agent/run.sh`: break a GPU's NVLink, watch scores and placement follow
- `./experiments/node-pools/run.sh`: a straggler moves the node to the inference pool; repair brings it back
- `./experiments/why/run.sh`: two Pending pods, same scheduler message, `kubectl gpufit why` tells busy from unfit

Health comes from a pluggable collector. On kind it's `fake` (ConfigMap
`gpufit-fake-metrics`, edit it to degrade a GPU); an `nvidia-smi` collector for
real GPUs is next.

DRA doesn't rank devices by score yet, so `gpufit-claim` lists
`firstAvailable` options from strictest to loosest (score >= 95, >= 90, >= min):
the job gets the best tier that is still free.
