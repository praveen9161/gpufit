# Session B: GPUFit in Kubernetes on real GPUs, cost measured (2026-10-01)

**The project's claim, measured end to end:** with one degraded GPU, Kubernetes + GPUFit wasted
far less paid GPU time than Kubernetes alone. **Cost per useful GPU-hour: $1.35 -> $1.02 (-25%).**

## Setup

- 2x AWS g5.xlarge (1x NVIDIA A10G each), us-east-1c, Deep Learning AMI (PyTorch 2.12, Ubuntu 24.04).
  Ran 16:19-17:13 UTC (54 min) ~ **$1.85-1.95**.
- Kubernetes: **k3s v1.37.1**, server on A (`ip-10-42-1-239`), agent on B (`ip-10-42-1-226`).
- **NVIDIA's DRA driver** (`dra-driver-nvidia-gpu` chart 0.5.0): DeviceClass `gpu.nvidia.com`,
  one device `gpu-0` per node, pool = node name.
- **GPUFit**: agent DaemonSet reading the real `nvidia-smi` (NVIDIA runtime, no GPU allocated),
  controller turned on only for the "with GPUFit" runs (degrade 20 s / recover 60 s).
- Workloads as Kubernetes Jobs with DRA claims (image `pytorch/pytorch:2.12.1-cuda13.0-cudnn9-runtime`):
  - training: data-parallel (DDP, 2 pods across the 2 VMs or 1 pod), GEMM-heavy MLP stack, 134 M
    params, fp16, 16 micro-steps per gradient sync; fixed step count, so the job runs at its
    slowest GPU's pace
  - inference: LLM-style decode (batch 8 through 8.6 GB of fp16 weights), memory-bandwidth-bound
  Both are synthetic stand-ins, not real models.
- Degradation: GPU **B** power limit **300 -> 150 W** (`nvidia-smi -pl 150`).

## Results (tokens/s; repeats agree within 0-1.3%)

| Run | Placement (by Kubernetes) | Result | vs healthy |
|---|---|---|---|
| 2-GPU training, both healthy | A+B | 129,576 (2 runs) | 1.00 |
| 1-GPU training, healthy | B | 73,521 | 1.00 |
| inference, healthy | B | 454.4 | 1.00 |
| **without GPUFit**: 2-GPU training, B at 150 W | A+B | **109,482** (2 runs) | **0.84** |
| **with GPUFit**: 1-GPU training | **A** (B tainted) | 73,355 (2 runs) | 1.00 |
| **with GPUFit**: inference, same time | **B** (degraded) | 444.2 (2 runs) | **0.98** |

- 2-GPU scaling when healthy: 0.88 (the network between the VMs costs 12%).
- **Straggler effect, measured:** one GPU at half power slowed the whole 2-GPU job to 0.84.
- **The degraded GPU still delivers 0.98 of a healthy GPU for (memory-bound) inference.**

## Cost (2 GPUs billed, one degraded; price $1.006/GPU-hour)

Useful work in healthy-GPU equivalents (each workload's throughput / one healthy GPU's):

| | useful work | efficiency | $ per useful GPU-hour |
|---|---|---|---|
| without GPUFit | 1.49 (training) | 74% | $1.35 |
| with GPUFit | 1.00 training + 0.98 inference = 1.98 | 99% | **$1.02** |

**-25% cost per useful GPU-hour.**

**Caveat:** with only 2 GPUs and no spare, GPUFit makes the training job itself slower (0.67x of
its speed on A+B). The gain is efficiency: almost no paid GPU time is wasted. In a real cluster
GPUFit would give training a healthy spare, so the job keeps full speed too; that fleet case is
for the cost calculator (to do), using these measured numbers.

## Placement evidence

- Agent on B after the degradation: `training 100 -> 50 (weakest link: compute 0.50), finetune
  100 -> 78, inference 100 -> 76` (v1's power model overstates the impact; real training loss was
  ~16%. As a trigger it worked.)
- Controller: `node ip-10-42-1-226: pool inference (gpu-0 training 50 < 80)`; DeviceTaintRule
  `gpufit-ip-10-42-1-226-unfit-training` on pool `ip-10-42-1-226` (`devicetaintrules.yaml`).
- A 2-GPU training job with GPUFit on: pod 0 Running on A, pod 1 **Pending**. Scheduler: "0/2 nodes
  are available: 2 cannot allocate all claims". `kubectl gpufit why` (`why-train-2gpu.txt`): B's
  GPU tainted because B is in the inference pool, A's GPU in use by the job's other pod.
- Files: `results.jsonl`, `summary.md`, `agent.log`, `controller.log`, `events.txt`, `nodes.txt`,
  `resourceslices.yaml`, `scores.json`.

## Problems found on the real run (all fixed in the scripts)

1. **k3s' default pod network (10.42.0.0/16) overlapped the VPC (10.42.0.0/16)**: node A routed
   B's real address into flannel, B could never reach the API server. Fixed with
   `--cluster-cidr 10.244.0.0/16 --service-cidr 10.96.0.0/16`. (First attempt aborted:
   `../session-b-20261001-1118-aborted/`.)
2. **NVIDIA's DRA kubelet plugin runs only on nodes with NFD/GFD GPU labels**, which weren't
   installed: no GPUs published. Fixed by labelling `nvidia.com/gpu.present=true`.
3. **orchestrate.sh was edited while running** (to add fix 2), and bash reads scripts
   incrementally: syntax error mid-run. Resumed on the same cluster with `resume.sh`; the two
   2-GPU references recorded before the crash are kept.
4. The k3s join token was printed once in the operator's terminal; no longer printed.
