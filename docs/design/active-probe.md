# Design: active probe

Status: **draft for review, not implemented.** Motivated by session A
(experiments/real-gpu/results/session-a-20260930-1055/README.md).

## Why passive signals aren't enough

On a real Tesla T4, locking the graphics clock to 50% cost a small CNN training
loop 14% throughput, while nvidia-smi showed nothing unusual: no "locked" flag,
no event reason, applications clocks "deprecated". GPUFit reported compute 1.00.
Some degradations (clock locks, firmware or driver issues, silent hardware
faults) can only be seen by **running something and timing it**.

## What the probe is

A short, fixed benchmark (~15 s) run on one GPU while it's **idle**, whose
throughput is compared with a **healthy baseline for the same GPU model and
driver**. The result is a measured ratio, e.g. `probe compute 0.86`.

## Lesson from session A: pick the right kernels

| Kernel | Sensitive to | Session A evidence |
|---|---|---|
| Large fp16 GEMM (8192^2) | power limit | -17% at 86% power; **0% at a 50% clock lock** (already power-capped at 660 MHz) |
| Small CNN step (clock-bound, below power cap) | clock locks, and power | -14% at a 50% clock lock; -17% at 86% power |
| Memory copy (bandwidth) | memory clock / HBM health | not measured yet |

A probe made only of large GEMMs would have **missed** the clock lock. v1 runs
three short parts and reports each: a clock-bound part (small kernels well
below the power cap), a power-bound part (large GEMM), and a bandwidth part.
`compute = min(clock-bound, power-bound)`, `memory = bandwidth ratio`.

## Baseline: what "healthy" means

A fixed table per GPU model would be wrong across drivers, clocks and cooling.
Instead: **the fleet median for the same model + driver**, with a fallback to
the GPU's own best result in the last 7 days when it's alone. A ratio is only
reported once there are at least N (e.g. 3) samples. On a single-GPU test box,
that means "this GPU vs itself when healthy", stated as such.

## How it runs in Kubernetes

- `gpufit-controller` creates a **probe Job** per GPU on a schedule (e.g. every
  6 h, and on demand when passive scores change). The Job's ResourceClaim
  selects **exactly that device** and tolerates GPUFit's own taints, so a
  degraded GPU can still be probed.
- **Only on idle GPUs:** the controller only creates a probe for a device with
  no allocated claim. Low PriorityClass, 15 s runtime, so it doesn't hold a
  GPU that real work is waiting for for long.
- Results go to ConfigMap `gpufit-probes` (`<node>.json`, with a timestamp).
  The agent merges them: `compute = min(passive, probe)`, and a probe result
  expires after e.g. 24 h.
- Image: PyTorch for v1 (already used by bench.py); a small CUDA-only binary
  later to cut image size.

## Limits (to state honestly)

- A GPU that is never idle is never probed; passive signals remain its only source.
- Detects **persistent** degradation (locks, caps, faulty parts), not
  transient hiccups between probes.
- The probe itself uses the GPU for ~15 s per run: a small, explicit cost.

## Validation plan (session A2)

Run the probe on a healthy GPU, then with 90/70/50% clock locks and 90/80/70%
power limits, 3 runs each. Pass criteria: the clock-bound part detects a 50%
clock lock (session A: -14% on the CNN), and the probe's noise stays below the
smallest degradation we want to flag.
