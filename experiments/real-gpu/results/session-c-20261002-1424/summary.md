# Session C: straggler vs rescue, measured

Job: 2-GPU data-parallel training, `--total-steps 400 --ckpt-every 50`; one of its GPUs degrades at step 100.

| Run | What GPUFit did | Job time | Extra vs R0 | Extra GPU-hours |
|---|---|---|---|---|
| R0 reference, healthy | - | 901 s | - | - |
| R1 one GPU degraded, **observe** | reported only | 1001 s | +100 s | 0.056 ($0.06) |
| R2 one GPU degraded, **enforce + rescue** | moved the job | 987 s | +86 s | 0.048 ($0.05) |

- Healthy step time: 2.25 s (median between checkpoints, R0).
- Checkpoint overhead: 10.0 s per checkpoint (median, all ranks wait), paid equally in every run.
- **Straggler speed (R1): 0.87** of healthy after the degradation (2.59 s/step vs 2.25).
- `kubectl gpufit report` estimated **0.214** wasted GPU-hours for R1 (slowest score 50); measured: **0.056** (3.8x the measured waste).

Rescue timeline (R2):

- degraded -> node out of the training pool: 32 s (agent window + controller degrade-after)
- out of the pool -> job evicted: 36 s (rescue grace)
- evicted -> both pods running again: 22 s (pod replacement, scheduling)
- resumed from step 100 (checkpoint load 3.0 s); about 31 steps redone
- placement: ip-10-42-1-18, ip-10-42-1-192 -> ip-10-42-1-144, ip-10-42-1-18

## Straggler vs rescue

Rescue finished the job **14 s sooner** than leaving it on the degraded GPU (0.008 GPU-hours ($0.01) saved, this job).

*Estimate:* a straggler at 0.87 loses 0.15 s per healthy second of remaining work; the rescue cost a fixed ~86 s here, so rescue pays off when the job has more than ~10 min of (healthy) work left after the degradation.

*Estimate, production timers* (degrade-after 5m, rescue grace 2m instead of 20 s / 30 s): GPUFit waits 370 s longer, during which the job runs degraded; the fixed cost becomes ~141 s and the break-even ~16 min of remaining work. Work redone after the move is bounded by the checkpoint interval either way.
