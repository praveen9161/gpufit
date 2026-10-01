# Session A2: NVIDIA A10G (2026-10-01)

**Hardware:** AWS g5.xlarge (us-east-1c), 1x NVIDIA A10G, power limit 100-300 W, max graphics
clock 1710 MHz. PyTorch 2.12.1 (CUDA 13.0). Planned for an L4 (g6.xlarge), but AWS had no L4
capacity in us-east-1a or 1b (`InsufficientInstanceCapacity`), so we switched to the A10G.
**Cost:** ~$1.12 (67 min). **Method:** `experiments/real-gpu/session-a2/`, 3 repeats per setting,
200 ms telemetry sampling, an nvidia-smi XML snapshot every 5 s judged by the Go collector.
**Noise:** repeats agree within 0.0-0.4%.

## 1. Same degradation, very different cost per workload

Throughput vs baseline (median of 3). `gemm` = fp16 8192^2 matrix multiply (heavy, training-like);
`train` = small CNN step (lighter).

| Setting | gemm | train (CNN) | GPUFit compute (v1 prediction) |
|---|---|---|---|
| power 250 W (83%) | 0.94 | 1.00 | 0.83 |
| power 200 W (67%) | 0.84 | 0.99 | 0.67 |
| power 150 W (50%) | **0.68** | **0.94** | 0.50 |
| power 100 W (33%) | 0.35 | 0.71 | 0.33 |
| clock lock 90% | 0.97 | 0.98 | 0.89 / 0.55 |
| clock lock 70% | 0.76 | 0.87 | 0.57 / 0.41 |
| clock lock 50% | 0.54 | 0.74 | 0.42 / 0.34 |

Memory bandwidth (copy, 481 GB/s) was 1.00 at every setting: power limits and clock locks
don't touch memory.

**Cost reading (arithmetic on the measured numbers):** at 150 W this GPU runs heavy training work
at 0.68. In a synchronous multi-GPU job every step waits for it, so the whole job takes ~1/0.68 =
1.47x as long and every GPU in the job is billed for the extra time. The same GPU still delivers
0.94 on the lighter workload.

## 2. GPUFit v1's predictions

- **Power model (`compute = power ratio`):** matched the T4 (session A), but is far too
  pessimistic here (150 W: predicted 0.50, measured 0.68 / 0.94). How much a lower limit hurts
  depends on the GPU and on whether the workload actually draws that power.
- **Rule B ("unexplained clock deficit"), first test on new data:**
  - detection: flagged **every** snapshot during clock locks (90/70/50%) and **none** of the
    ~210 power-sweep snapshots. It reliably detects *that* clocks are held back.
  - size estimate (power draw / limit): far too pessimistic on a GPU with lots of power headroom
    (90% lock: estimated 0.55, measured 0.98 on the CNN). Designed on T4 data; doesn't transfer.

## 3. The probe

The probe parts (short gemm / CNN / copy runs) matched the full benchmarks at every setting.
Expected, since they are the same workloads, but it is the point: a short per-workload run gives
the per-workload impact that passive signals can't estimate.

**Design consequence for v1:** passive signals (incl. rule B) = *trigger* ("something is off");
probe = *per-workload score* ("costs heavy training 32%, light work 6%").

## 4. Throttle counters (open question from session A)

On the A10G, `clocks_event_reasons_counters.sw_power_cap` grows whenever software holds the clock
back, by a power cap **or a clock lock** (50% lock: flag "Not Active" in 100% of samples, counter
+63 s over a 64 s run), and stays 0 when nothing limits the clock. So it is a second passive
clock-lock signal on this driver. It behaved differently on the T4, so treat it as per-driver.

## Raw data

`results.jsonl` (every run), `summary.md`, `samples/*.csv` (200 ms telemetry),
`snapshots.tar.gz` (625 nvidia-smi XML snapshots), `session.log`, `permissions.json`.
