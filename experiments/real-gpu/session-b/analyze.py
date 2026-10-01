#!/usr/bin/env python3
"""Session B summary: the cost of a degraded GPU with and without GPUFit.

  python3 analyze.py RESULTS_DIR [price_per_gpu_hour] > summary.md

"Useful work" is measured in healthy-GPU equivalents: each workload's
throughput divided by its throughput on ONE healthy GPU (reference runs).
Efficiency = useful work / GPUs billed. $ per useful GPU-hour = price / efficiency.
"""
import json
import statistics
import sys
from collections import defaultdict

results_dir = sys.argv[1]
price = float(sys.argv[2]) if len(sys.argv) > 2 else 1.006  # g5.xlarge on-demand, us-east-1

runs = defaultdict(list)
for line in open(f"{results_dir}/results.jsonl"):
    r = json.loads(line)
    runs[r["scenario"]].append(r["tokens_per_s"])


def med(scenario):
    v = runs.get(scenario)
    return statistics.median(v) if v else None


def spread(scenario):
    v = runs.get(scenario, [])
    return (max(v) - min(v)) / statistics.median(v) * 100 if len(v) > 1 else None


ref2, ref1, ref_inf = med("ref-train-2gpu"), med("ref-train-1gpu"), med("ref-infer")
without2 = med("without-train-2gpu")
with_train, with_inf = med("with-train-1gpu"), med("with-infer")

print("## Measurements (tokens/s, median of repeats)\n")
print("| scenario | runs | median | spread |")
print("|---|---|---|---|")
for s in ("ref-train-2gpu", "ref-train-1gpu", "ref-infer", "without-train-2gpu", "with-train-1gpu", "with-infer"):
    sp = spread(s)
    print(f"| {s} | {len(runs.get(s, []))} | {med(s) if med(s) is not None else '-'} | "
          f"{f'{sp:.1f}%' if sp is not None else '-'} |")

if None in (ref2, ref1, ref_inf, without2, with_train, with_inf):
    print("\nIncomplete: some scenarios are missing, no cost comparison.")
    sys.exit(0)

scaling = ref2 / (2 * ref1)
straggler = without2 / ref2
useful_without = without2 / ref1
useful_with = with_train / ref1 + with_inf / ref_inf
eff_without, eff_with = useful_without / 2, useful_with / 2

print("\n## What the numbers say\n")
print(f"- 2-GPU scaling when both are healthy: {scaling:.2f} (1.00 = perfect; the network costs {100 * (1 - scaling):.0f}%).")
print(f"- Straggler effect: with GPU B at reduced power, the 2-GPU job runs at {straggler:.2f}x its healthy speed.")
print(f"- The degraded GPU still delivers {with_inf / ref_inf:.2f}x a healthy GPU's inference throughput.")

print("\n## Cost: 2 GPUs billed, one of them degraded\n")
print("| | useful work (healthy-GPU equivalents) | efficiency | $ per useful GPU-hour |")
print("|---|---|---|---|")
print(f"| without GPUFit (2-GPU training on A+B) | {useful_without:.2f} training | {100 * eff_without:.0f}% | ${price / eff_without:.2f} |")
print(f"| with GPUFit (training on A, inference on B) | {with_train / ref1:.2f} training + {with_inf / ref_inf:.2f} inference = {useful_with:.2f} | {100 * eff_with:.0f}% | ${price / eff_with:.2f} |")
saving = 1 - (price / eff_with) / (price / eff_without)
print(f"\nGPUFit lowers the cost per useful GPU-hour by **{100 * saving:.0f}%** in this setup (price ${price}/GPU-hour).")
print(f"\nCaveat: with no spare GPU, the training job itself runs at {with_train / without2:.2f}x the speed it had on A+B "
      f"({with_train:.0f} vs {without2:.0f} tokens/s). GPUFit's gain here is efficiency, not job speed; "
      "with a healthy spare, training would keep full speed too.")
