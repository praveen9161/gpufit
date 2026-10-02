#!/usr/bin/env python3
"""Session C analysis: straggler (observe) vs rescue (enforce), from measured times only.

  python3 analyze.py <results dir>

Reads runs.jsonl, timeline.jsonl, logs/<pod>.log (JSON lines from train_ddp.py),
status-after-R1.json and setup.json; prints a Markdown summary. Values derived from
measurements (break-even, production timers) are labelled as estimates.
"""
import glob
import json
import os
import statistics
import sys

d = sys.argv[1]
GPUS = 2


def jsonl(path):
    if not os.path.exists(path):
        return []
    out = []
    for line in open(path):
        line = line.strip()
        if line.startswith("{"):
            try:
                out.append(json.loads(line))
            except json.JSONDecodeError:
                pass
    return out


setup = json.load(open(os.path.join(d, "setup.json")))
price = setup.get("price_per_gpu_hour", 0)
runs = {r["run"]: r for r in jsonl(os.path.join(d, "runs.jsonl"))}
timeline = jsonl(os.path.join(d, "timeline.jsonl"))


def t(run, event):
    for e in timeline:
        if e["run"] == run and e["event"] == event:
            return e["time"]
    return None


def rank0(job):
    """Events of every incarnation of rank 0, oldest pod first."""
    ev = []
    for f in sorted(glob.glob(os.path.join(d, "logs", f"{job}-0-*.log")), key=os.path.getmtime):
        ev += jsonl(f)
    return ev


def step_times(events, start=None, end=None):
    """Seconds per step between consecutive checkpoints whose wall time is in [start, end]."""
    ck = [e for e in events if e.get("event") == "checkpoint" and "time" in e]
    out = []
    for a, b in zip(ck, ck[1:]):
        if b["step"] <= a["step"] or b["time"] < a["time"]:
            continue  # a new incarnation started
        if (start is None or a["time"] >= start) and (end is None or b["time"] <= end):
            out.append((b["time"] - a["time"]) / (b["step"] - a["step"]))
    return out


def gh(seconds):
    return GPUS * seconds / 3600


def money(h):
    return f" (${h * price:.2f})" if price else ""


def helm_durations(path):
    """degradeAfter and grace (seconds) from `helm get values` output, without a YAML library."""
    import re
    out = {}
    if not os.path.exists(path):
        return out
    for key in ("degradeAfter", "grace"):
        m = re.search(rf"^\s*{key}:\s*\"?(\d+)(s|m)\"?\s*$", open(path).read(), re.M)
        if m:
            out[key] = int(m.group(1)) * (60 if m.group(2) == "m" else 1)
    return out


print("# Session C: straggler vs rescue, measured\n")
print(f"Job: {GPUS}-GPU data-parallel training, `{setup['train_args']}`; one of its GPUs degrades at step "
      f"{setup['degrade_at_step']}.\n")

r0, r1, r2 = runs.get("R0"), runs.get("R1"), runs.get("R2")
if not r0:
    sys.exit("no R0 reference run")
T0 = r0["wall_s"]
ev0 = rank0(r0["job"])
st0 = step_times(ev0)
step_ok = statistics.median(st0) if st0 else None
saves = [e["save_s"] for e in ev0 if e.get("event") == "checkpoint" and "save_s" in e]

print("| Run | What GPUFit did | Job time | Extra vs R0 | Extra GPU-hours |")
print("|---|---|---|---|---|")
print(f"| R0 reference, healthy | - | {T0:.0f} s | - | - |")
if r1:
    print(f"| R1 one GPU degraded, **observe** | reported only | {r1['wall_s']:.0f} s | {r1['wall_s'] - T0:+.0f} s "
          f"| {gh(r1['wall_s'] - T0):.3f}{money(gh(r1['wall_s'] - T0))} |")
if r2:
    print(f"| R2 one GPU degraded, **enforce + rescue** | moved the job | {r2['wall_s']:.0f} s | {r2['wall_s'] - T0:+.0f} s "
          f"| {gh(r2['wall_s'] - T0):.3f}{money(gh(r2['wall_s'] - T0))} |")
print()
if step_ok:
    print(f"- Healthy step time: {step_ok:.2f} s (median between checkpoints, R0).")
if saves:
    print(f"- Checkpoint overhead: {statistics.median(saves):.1f} s per checkpoint (median, all ranks wait), "
          "paid equally in every run.")

speed = None
if r1:
    td = t("R1", "degrade")
    ev1 = rank0(r1["job"])
    after = step_times(ev1, start=td)
    if step_ok and after:
        speed = step_ok / statistics.median(after)
        print(f"- **Straggler speed (R1): {speed:.2f}** of healthy after the degradation "
              f"({statistics.median(after):.2f} s/step vs {step_ok:.2f}).")
    try:
        st = json.load(open(os.path.join(d, "status-after-R1.json")))
        inc = st["report"]["incidents"].get(f"gpufit-lab/Job/{r1['job']}")
    except (OSError, KeyError, json.JSONDecodeError):
        inc = None
    measured = gh(r1["wall_s"] - T0)
    if inc:
        est = inc["wastedGpuHours"]
        ratio = f" ({est / measured:.1f}x the measured waste)" if measured > 0 else ""
        print(f"- `kubectl gpufit report` estimated **{est:.3f}** wasted GPU-hours for R1 (slowest score "
              f"{inc['slowestScore']}); measured: **{measured:.3f}**{ratio}.")

if r2:
    t_deg, t_pool, t_ev, t_run = (t("R2", e) for e in ("degrade", "pool_inference", "evicted", "running_again"))
    ev2 = rank0(r2["job"])
    starts = [e for e in ev2 if e.get("event") == "start"]
    resumed = next((e for e in starts if e.get("resumed_from", 0) > 0), None)
    print("\nRescue timeline (R2):\n")
    if t_deg and t_pool:
        print(f"- degraded -> node out of the training pool: {t_pool - t_deg:.0f} s (agent window + controller degrade-after)")
    if t_pool and t_ev:
        print(f"- out of the pool -> job evicted: {t_ev - t_pool:.0f} s (rescue grace)")
    if t_ev and t_run:
        print(f"- evicted -> both pods running again: {t_run - t_ev:.0f} s (pod replacement, scheduling)")
    if resumed:
        lost = None
        ck = [e for e in ev2 if e.get("event") == "checkpoint" and t_ev and e["time"] <= t_ev]
        if ck and step_ok and t_ev:
            last = ck[-1]
            lost = last["step"] - resumed["resumed_from"] + (t_ev - last["time"]) / step_ok
        print(f"- resumed from step {resumed['resumed_from']} (checkpoint load {resumed.get('load_s', '?')} s)"
              + (f"; about {lost:.0f} steps redone" if lost is not None else ""))
    if r2.get("placement_start") and r2.get("placement_end"):
        print(f"- placement: {', '.join(r2['placement_start'])} -> {', '.join(r2['placement_end'])}")

if r1 and r2:
    s1, s2 = r1["wall_s"] - T0, r2["wall_s"] - T0
    print("\n## Straggler vs rescue\n")
    if s2 < s1:
        print(f"Rescue finished the job **{s1 - s2:.0f} s sooner** than leaving it on the degraded GPU "
              f"({gh(s1 - s2):.3f} GPU-hours{money(gh(s1 - s2))} saved, this job).")
    else:
        print(f"Rescue cost **{s2 - s1:.0f} s more** than leaving the job on the degraded GPU: for this "
              "job length and degradation, moving it didn't pay off.")
    if speed and speed < 1:
        rate = 1 / speed - 1  # extra seconds per healthy second while straggling
        print(f"\n*Estimate:* a straggler at {speed:.2f} loses {rate:.2f} s per healthy second of remaining work; the "
              f"rescue cost a fixed ~{s2:.0f} s here, so rescue pays off when the job has more than "
              f"~{s2 / rate / 60:.0f} min of (healthy) work left after the degradation.")
        demo = helm_durations(os.path.join(d, "helm-values.yaml"))
        if demo:
            # Production defaults: degrade-after 5m, rescue grace 2m. The job keeps running (degraded) while
            # GPUFit waits, so a longer wait costs `rate` per second of it, not the whole second.
            extra = (300 - demo.get("degradeAfter", 300)) + (120 - demo.get("grace", 120))
            fixed = s2 + rate * extra
            print(f"\n*Estimate, production timers* (degrade-after 5m, rescue grace 2m instead of "
                  f"{demo.get('degradeAfter', 300):.0f} s / {demo.get('grace', 120):.0f} s): GPUFit waits {extra:.0f} s longer, "
                  f"during which the job runs degraded; the fixed cost becomes ~{fixed:.0f} s and the break-even "
                  f"~{fixed / rate / 60:.0f} min of remaining work. Work redone after the move is bounded by the "
                  "checkpoint interval either way.")

