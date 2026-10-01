## Measurements (tokens/s, median of repeats)

| scenario | runs | median | spread |
|---|---|---|---|
| ref-train-2gpu | 2 | 129576.05 | 0.9% |
| ref-train-1gpu | 1 | 73520.7 | - |
| ref-infer | 1 | 454.4 | - |
| without-train-2gpu | 2 | 109482.29999999999 | 1.3% |
| with-train-1gpu | 2 | 73355.15 | 0.0% |
| with-infer | 2 | 444.15 | 0.1% |

## What the numbers say

- 2-GPU scaling when both are healthy: 0.88 (1.00 = perfect; the network costs 12%).
- Straggler effect: with GPU B at reduced power, the 2-GPU job runs at 0.84x its healthy speed.
- The degraded GPU still delivers 0.98x a healthy GPU's inference throughput.

## Cost: 2 GPUs billed, one of them degraded

| | useful work (healthy-GPU equivalents) | efficiency | $ per useful GPU-hour |
|---|---|---|---|
| without GPUFit (2-GPU training on A+B) | 1.49 training | 74% | $1.35 |
| with GPUFit (training on A, inference on B) | 1.00 training + 0.98 inference = 1.98 | 99% | $1.02 |

GPUFit lowers the cost per useful GPU-hour by **25%** in this setup (price $1.006/GPU-hour).

Caveat: with no spare GPU, the training job itself runs at 0.67x the speed it had on A+B (73355 vs 109482 tokens/s). GPUFit's gain here is efficiency, not job speed; with a healthy spare, training would keep full speed too.
