# Plan: session B, GPUFit in Kubernetes on real GPUs, with the cost measured

Status: **done** (session B, 2026-10-01): one session, both halves of the project goal.
1. The system: Kubernetes uses GPUFit's scores to place work (degraded GPU -> no big
   training, still inference).
2. The proof: less wasted GPU money, measured on real hardware.

## Setup

- 2x g5.xlarge (1x A10G each, same zone; quota of 8 vCPUs fits exactly two), ~$2/h together.
- Kubernetes on the two machines (one control plane + worker, one worker), NVIDIA's DRA driver
  (`kubernetes-sigs/dra-driver-nvidia-gpu`), GPUFit agent (nvidia-smi collector) + controller.
- GPU **B** is degraded to **150 W** (session A2: heavy training work 0.68, lighter work 0.94).
  GPU **A** stays healthy.
- Workloads run **as Kubernetes pods with DRA claims**: a 2-pod data-parallel training job
  (PyTorch DDP, GEMM-heavy model, enough compute per step that the network isn't the
  bottleneck) and an inference job (forward passes).

## Runs

| Run | Kubernetes does | Measured |
|---|---|---|
| Reference (healthy) | 2-GPU training on A+B; 1-GPU training on A; inference on B | throughput references |
| **Without GPUFit** (B degraded, controller off) | schedules the 2-GPU training on A+B (it can't know) | training throughput -> straggler effect |
| **With GPUFit** (B degraded, controller on) | B -> inference pool (device taint): training avoids B (`kubectl gpufit why` shows why), inference lands on B, training runs on A | training + inference throughput, placement in `kubectl`/dashboard |

## The cost metric

**Useful work per GPU-hour**, in healthy-GPU equivalents (each workload's throughput divided by
its throughput on a healthy GPU), and **$ per useful GPU-hour** = $/h / efficiency.

- Without GPUFit: both GPUs billed, useful = 2-GPU training throughput / 1 healthy GPU's.
  Expected from A2: ~1.36 of 2 = ~68%.
- With GPUFit: 1.0 (training on A) + inference on B (~0.94 expected) = ~1.94 of 2 = ~97%.

Expectations come from A2; session B replaces them with measurements.

## Honest caveat (stated in the write-up)

With 2 GPUs and no spare, GPUFit makes the training job itself slower (1 GPU instead of ~1.36
effective). Its win is efficiency: almost no paid GPU time is wasted. In a real cluster GPUFit
would give training a healthy spare instead, keeping job speed *and* the degraded GPU's value;
the cost calculator models that fleet case with these measured numbers.

## Risk and fallback

Kubernetes + NVIDIA DRA driver setup on paid time is the main risk. Everything is scripted and
rehearsed on the Mac first. If the Kubernetes part fails during the session, the same training
and inference scripts run directly on the machines, so the cost measurement isn't lost.

## Cost

~75-90 min of 2x g5.xlarge ~ **$2.50-3.00**.
