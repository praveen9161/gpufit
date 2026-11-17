# Design: learned fitness (GPUFit v0.2)

Status: **proposed for v0.2** (2026-10-09).

## Problem

v0.1 *estimates* each GPU's speed from hardware signals (power, clocks, temperature). On real
A10Gs at 150 W it scored training 50, but training actually ran at 0.84-0.87: the report's waste
estimate was 3.8x too high (sessions B, C). Hardware signals also miss the most common slowdown:
in FALCON's clusters, network slowdowns were 7.2x more common than GPU compute slowdowns.

## Idea

Let the jobs measure. Training jobs already know how long each GPU takes; GPUFit just never asks.

One catch decides the design: in synchronous training **every GPU's step time is the same**,
because all wait for the slowest at the gradient sync. So each GPU reports two numbers per step:

- **compute time**: forward + backward on its own data, before it waits for anyone
  (CUDA events around the local work; with gradient accumulation, the `no_sync()` micro-steps
  are pure local compute);
- **sync time**: time in the gradient all-reduce.

The slow GPU has the longest *compute* time. A slow network link shows up as long *sync* time on
the ranks behind it, while their compute time stays normal.

## Pieces

1. **`gpufit-probe` (Python, ~100 lines, only PyTorch)**: `gpufit.watch(model)` or two calls around
   the step. Every N steps each rank sends `{job, rank, node, gpu uuid, workload, compute_ms,
   sync_ms}` to the GPUFit agent on its own node. If the agent isn't there, it does nothing:
   it never slows or breaks training.
2. **Agent**: a small HTTP endpoint on the node (hostPort, or a node-local Service), maps the GPU
   UUID to the DRA device name, and adds the observations to the scores ConfigMap it already writes.
3. **Controller**: for each job, compares its GPUs with each other (peer-relative, like FALCON and
   Guard): `speed = median(compute) / compute` per GPU, same for sync time per node. Keeps a smoothed
   per-GPU, per-workload **learned speed**. When a GPU has recent learned data, it replaces the
   hardware estimate in the score; otherwise the estimate stays.
4. **Report**: waste from the learned speed instead of score/100, so the estimate matches reality.
   `kubectl gpufit status` shows where each score came from (`learned` or `estimated`).

## Open decisions

1. **A Python package in this repo** (`probe/`), installable with `pip install` from git. Alternative:
   a PyTorch profiler plugin, heavier. *Proposed: the small package.*
2. **How ranks reach the agent:** hostPort on each node (simple, works on k3s and kind) vs a
   node-local Service (cleaner, needs `internalTrafficPolicy: Local`). *Proposed: node-local Service.*
3. **Single-GPU jobs** have no peers: compare with the same GPU model's median across recent jobs.
   *Proposed: yes, as a second step.*
4. **Inference:** later (needs request-level timing in the serving framework).

## Validation

- kind (CPU): two ranks, one slowed artificially (a sleep in its compute, or CPU limits); the
  controller must learn ~the injected slowdown, and the report's waste must match the measured one.
- Session D (real A10Gs, from the $300 budget): power sweep 100/150/200 W and a network slowdown
  (`tc netem` on one VM): learned speed vs measured job slowdown, and network detection.
