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
| `agent/` | Go: fitness scoring (`internal/fitness`), `gpufit-agent` (writes the scores ConfigMap), `gpufit-claim` (writes tiered claim templates) |
| `driver-patch/` | Patch for dra-example-driver: publishes the ConfigMap scores as `gpufit.io/<workload>Score` attributes |
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
DRIVER_IMAGE_TAG=v0.5.0-gpufit ./demo/scripts/build-driver-image.sh
DRIVER_IMAGE_TAG=v0.5.0-gpufit ./demo/scripts/load-driver-image-into-kind.sh
helm upgrade --install dra-example-driver deployments/helm/dra-example-driver \
  -n dra-example-driver --create-namespace \
  --set image.tag=v0.5.0-gpufit --set kubeletPlugin.gpufitScoresConfigMap=gpufit-scores
cd ..

# scores -> cluster, then schedule by fitness
(cd agent && go run ./cmd/gpufit-agent -o configmap | kubectl apply -f -)
kubectl create namespace gpufit-lab
(cd agent && go run ./cmd/gpufit-claim -workload training -min 80 -count 6 | kubectl apply -f -)
kubectl apply -f experiments/claim-generator/11-train-job.yaml
./scripts/gpu-dash.sh --once
```

Or walk through it step by step: `./experiments/claim-generator/run.sh`
(keep `./scripts/gpu-dash.sh` open in a second terminal).

DRA doesn't rank devices by score yet, so `gpufit-claim` lists
`firstAvailable` options from strictest to loosest (score >= 95, >= 90, >= min):
the job gets the best tier that is still free.
