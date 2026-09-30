#!/usr/bin/env python3
"""Summarize a session A2 results directory as Markdown.

  python3 analyze.py OUT_DIR > summary.md

Per setting and workload: median of the repeats, spread (noise), throughput vs
baseline, GPUFit's predicted compute (median over the run's XML snapshots) and
how many snapshots the "unexplained clock deficit" rule flagged. For probe runs:
each part (gemm, train, copy) vs its baseline.
"""
import json
import statistics
import sys
from collections import defaultdict

out = sys.argv[1]
rows = [json.loads(line) for line in open(f"{out}/results.jsonl")]


def med(xs):
    xs = [x for x in xs if x is not None]
    return statistics.median(xs) if xs else None


def fmt(x, nd=2):
    return "-" if x is None else f"{x:.{nd}f}"


order, groups = [], defaultdict(list)
for r in rows:
    key = (r["label"], r["workload"])
    if key not in groups:
        order.append(key)
    groups[key].append(r)

base = {w: med(r["value"] for r in groups.get(("baseline", w), [])) for w in ("gemm", "train")}
probe_base = {}
for part in ("gemm", "train", "copy"):
    probe_base[part] = med(r["parts"][part]["value"] for r in groups.get(("baseline", "probe"), []))

print("## Benchmarks (median of repeats)\n")
print("| setting | workload | measured | spread | vs baseline | GPUFit compute | deficit flagged |")
print("|---|---|---|---|---|---|---|")
for label, w in order:
    if w == "probe":
        continue
    rs = groups[(label, w)]
    vals = [r["value"] for r in rs]
    m = med(vals)
    spread = (max(vals) - min(vals)) / m if m else None
    rel = m / base[w] if base.get(w) else None
    flagged = sum(r.get("deficit_snapshots", 0) for r in rs)
    snaps = sum(r.get("snapshots", 0) for r in rs)
    print(f"| {label} | {w} | {m:.3f} {rs[0]['unit']} | {fmt(100 * spread, 1) if spread is not None else '-'}% | "
          f"{fmt(rel)} | {fmt(med(r.get('gpufit_compute') for r in rs))} | {flagged}/{snaps} |")

print("\n## Probe parts vs baseline (median of repeats)\n")
print("| setting | gemm (power-bound) | train (clock-sensitive) | copy (bandwidth) |")
print("|---|---|---|---|")
for label, w in order:
    if w != "probe":
        continue
    cells = []
    for part in ("gemm", "train", "copy"):
        m = med(r["parts"][part]["value"] for r in groups[(label, w)])
        cells.append(fmt(m / probe_base[part]) if probe_base.get(part) else "-")
    print(f"| {label} | " + " | ".join(cells) + " |")
