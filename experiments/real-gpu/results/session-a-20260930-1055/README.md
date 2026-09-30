# Session A: first real GPU (2026-09-30)

**Hardware:** AWS g4dn.xlarge, 1x NVIDIA Tesla T4, driver 595.91.07, PyTorch 2.12.1 (CUDA 13.0),
Deep Learning OSS Nvidia Driver AMI GPU PyTorch 2.12 (Ubuntu 24.04). **Cost:** ~$0.25-0.30 (~33 min).
**Method:** `experiments/real-gpu/session-a/` (bench.py, session.sh). Each run: 10 s warm-up + 20 s
measured. **One run per setting** (baseline: 3 GEMM runs, spread 1.7%), so treat differences
below ~2% as noise.

Workloads: `gemm` = fp16 8192x8192 matrix multiply (TFLOPS); `train` = small CNN, batch 128,
synthetic data, mixed precision (steps/s).

## 1. Permissions

`nvidia-smi -pl` (power limit) and `nvidia-smi -lgc` (lock clocks) both **work** on this AWS VM.

## 2. Power limit vs throughput (T4 range: 60-70 W)

| Power limit | GEMM measured | train measured | GPUFit compute (predicted) |
|---|---|---|---|
| 70 W (baseline) | 23.76 TFLOPS = 1.00 | 62.66 steps/s = 1.00 | 1.00 |
| 68 W (97%) | 0.99 | 0.99 | 0.97 |
| 65 W (93%) | 0.93 | 0.96 | 0.93 |
| 62 W (89%) | 0.87 | 0.89 | 0.89 |
| 60 W (86%) | 0.83 | 0.83 | 0.86 |

Fit `throughput ~ power_ratio^a`: **a = 1.11 (GEMM), 1.03 (train)**. GPUFit's default a = 1.0 is
within noise here. Only valid for the T4 and only for 86-100% of the default limit (the T4 doesn't
allow lower); other GPUs need their own curve.

## 3. Clock lock vs throughput (max graphics clock 1590 MHz)

| Clock lock | GEMM | train | GPUFit compute |
|---|---|---|---|
| 90% (1431 MHz) | 1.00 | 1.00 | 1.00 |
| 80% (1272 MHz) | 0.99 | 1.00 | 1.00 |
| 70% (1113 MHz) | 0.99 | 0.99 | 1.00 |
| 60% (954 MHz) | 0.99 | 0.94 | 1.00 |
| 50% (795 MHz) | 0.99 | **0.86** | **1.00** |

- **GEMM is power-bound, not clock-bound:** under load the T4 already ran GEMM at **660 MHz** with
  `sw_power_cap` active (captured XML), below every lock we tried, so locks had no effect.
- **The small CNN did slow down** once the lock went below what it otherwise runs at: 0.86 at 50%.
- **GPUFit's blind spot is confirmed on real hardware:** a 50% clock lock cost training 14% while
  GPUFit reported compute 1.00 / training 100.

## 4. What nvidia-smi does and doesn't show (driver 595.91.07)

- `applications_clocks` / `default_applications_clocks`: **"Requested functionality has been
  deprecated"**. The collector's clock ratio (built on them) has no data on new drivers; it
  correctly falls back to n/a.
- A clock lock only appears as the current `clocks/graphics_clock` (795 MHz). There is no
  "locked" flag or event reason, so a single snapshot can't tell a lock from normal behavior.
- `gpu_power_readings/instant_power_draw` is present; `average_power_draw` is N/A.
- **`clocks_event_reasons_counters_*`** exist: cumulative microseconds spent in each throttle
  reason. The difference between two readings gives "throttled X% of the time", a better signal
  than the instantaneous Active/Not Active flags.
- `nvlink` / `network`: not applicable / not measured (single PCIe GPU).

## 5. Takeaways

1. The power-limit part of the compute score predicted real slowdowns within a few percent.
2. The same degradation (50% clock lock) cost one workload 14% and another ~1%. Degradation
   impact **depends on the workload**, which is GPUFit's core premise. One GPU, one run each, so
   this is a first signal, not proof.
3. Passive nvidia-smi signals **can't see clock locks**. Detecting them needs an active probe (a
   short fixed benchmark compared with the GPU's healthy baseline) or clock-vs-power history.
4. Collector to-dos: use the throttle-reason counters over the window; drop the deprecated
   applications-clocks ratio; add real captures as test data (done: agent/testdata/nvidia-smi/real-*).
5. Measurement to-dos: multiple runs per setting; sample clocks and power continuously during a
   run (one mid-run snapshot is noisy); a GPU with a wider power range (L4/A10G); the straggler
   test on 2+ GPUs.

Raw data: `results.jsonl`, `summary.md`, `session.log`, `permissions.json`, every nvidia-smi XML
(`idle.xml`, `load-<setting>-<workload>.xml`) with GPUFit's explanation next to it.
