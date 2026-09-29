# Design: nvidia-smi collector

Status: **draft, not yet validated on real hardware.** Every threshold and
formula below is a starting assumption. The point of the real-GPU session
(weeks 5-6) is to measure them.

## Goal

Turn what `nvidia-smi` reports about a GPU into the five health ratios GPUFit
scores (`compute`, `memory`, `nvlink`, `network`, `thermal`), each in [0, 1]
where 1.0 = "performs like a healthy GPU of this model".

## Decision 1: health, not capability

A score says **how far this GPU is from its own spec**, not how good the model
is. An L4 at full health is 1.0 for compute, the same as a healthy H100.

- Why: capability is already published by the DRA driver (`model`, memory
  size). Mixing the two makes the score meaningless across models ("0.6
  because it's an L4, or because it's broken?").
- Consequence: a claim that needs a fast GPU selects on `model` **and**
  `trainingScore`. GPUFit only answers "is this GPU healthy enough for X".

## Decision 2: passive signals first, measured over a window

v1 only reads counters and state. It doesn't run benchmarks on the GPU.

- Why: an active probe (e.g. a 10 s matrix multiply) needs the GPU idle,
  steals time from jobs, and needs scheduling of its own. Passive reads are free.
- Why a window: many signals only show under load. An idle GPU is cool and
  never throttles, so a single reading says little. The collector samples
  every interval and keeps the **worst value over the last N minutes**
  (N = 10 to start).
- Later: an optional active probe, run only when the GPU is idle, to measure
  real throughput and cross-check the passive estimate.

## Data source

One call per interval: `nvidia-smi -q -x` (full XML report). It includes
clocks, clock event (throttle) reasons, power limits, temperatures and
their thresholds, ECC counts, retired pages / remapped rows, all in one
structured document. NVLink needs a second call: `nvidia-smi nvlink -s`
(link state and speed) and `nvidia-smi nvlink -e` (error counters).

To verify on the real GPU before trusting the parser: some XML element names
changed across driver versions (e.g. `clocks_throttle_reasons` became
`clocks_event_reasons`). The parser must accept both, and the first task in the
GPU session is to save real `nvidia-smi -q -x` output as test data.

Not chosen for v1: DCGM (`dcgmi`, dcgm-exporter). It has richer data
(profiling counters, health checks, Xid tracking), but it's an extra install.
It is the obvious second collector behind the same interface.

## Mapping

| Ratio | Signals | v1 formula (assumption) |
|---|---|---|
| **compute** | enforced power limit vs default limit; max SM clock vs default max; active clock-event reasons: HW slowdown, HW power brake, SW power cap while under load | `power_ratio^a × clock_cap_ratio`, then × 0.5 if a hardware slowdown event was active in the window. **a is unknown: measure it** (see below) |
| **memory** | uncorrectable ECC errors (volatile); retired pages / remapped rows pending; row-remap failure; max memory clock vs default | rule-based: remap failure or new uncorrectable errors → 0.0; pending retirement/remap (needs reset) → 0.5; otherwise the memory-clock cap ratio |
| **nvlink** | links active vs links expected; link speed vs max; replay/recovery/CRC error counters (rate over the window) | `active_links / expected_links × min(speed / max_speed)`, × 0.8 if error counters are climbing. **No NVLink on this GPU → not applicable (see below)** |
| **network** | not visible to nvidia-smi | **not measured in v1** (see below) |
| **thermal** | GPU temperature vs its slowdown threshold (from the XML); thermal slowdown event reasons | `clamp((slowdown - temp_max_in_window) / 15 °C, 0, 1)`, capped at 0.5 if a thermal slowdown event was active. The 15 °C band is a guess |

The 0.5 / 0.8 penalties and the memory categories are **policy choices**, not
measurements. They decide how harshly a symptom is punished and should be
tuned from what the real GPU shows.

### The compute experiment (what we pay GPU time for)

The key unknown: when the power limit is lowered to X% of default, how much
throughput is lost? Throughput is probably not linear in power (clocks and
voltage scale differently), so `a` in `power_ratio^a` must be measured:

1. Record `nvidia-smi -q -x` and a fixed workload's throughput at 100% power.
2. Repeat at, for example, 90%, 80%, 70%, 60%, 50% (`nvidia-smi -pl`).
3. Fit `throughput_ratio ≈ power_ratio^a`. Same for locked clocks (`nvidia-smi -lgc`).

This turns the compute formula from a guess into a measurement.

## Missing data: "not applicable" vs "not measured"

- **Not applicable**: the hardware doesn't exist (a PCIe GPU has no NVLink).
  Report 1.0 with reason "n/a". It's not a fault of this GPU (Decision 1).
- **Not measured**: the signal exists but v1 can't see it (network). Also
  1.0, but it must be visible: the agent logs it, and the attribute should
  make it explicit (e.g. `gpufit.io/networkMeasured=false`) so nobody assumes
  training scores include network health.

Network options for later: NIC/RDMA link state and rate from
`/sys/class/infiniband/*/ports/*/` or `ethtool`, error counters, or an active
NCCL all-reduce probe between nodes.

## Open question (decide before renting GPUs): which DRA driver?

The scores reach the scheduler as device attributes that our
**dra-example-driver fork** adds. On a real GPU node the real driver is NVIDIA's
DRA driver, and a third party can't add attributes to another driver's
devices. Options:

1. **Device taints per workload** (driver-independent): the agent creates
   `DeviceTaintRule`s like `gpufit.io/unfit-training:NoSchedule` for GPUs below
   a threshold. Training claims don't tolerate it; inference claims tolerate
   all `gpufit.io/unfit-*` taints. Works with any driver today. Loses the
   tiers (only pass/fail per workload).
2. **Run the example-driver fork on the real node** only for the scheduling
   demo, with its `gpu-N` mapped to real GPU index N. Proves placement, but
   pods don't actually get the GPU through DRA.
3. **Patch NVIDIA's DRA driver** the same way as the example driver: most
   realistic, largest effort.

Leaning: **1 for the real-GPU demo** (small, honest, works with the real stack),
with attributes + tiers staying the story on kind. Revisit after the collector.

## Device identity

The driver names devices `gpu-0`, `gpu-1`, ... The collector keys health by
the same names. On real hardware, map by **GPU UUID** (stable) rather than
index (can change across reboots), since both the XML and DRA drivers expose UUIDs.

## Test plan

- Unit tests: parse saved `nvidia-smi -q -x` samples (healthy, power-capped,
  thermal-throttled, ECC error) into ratios, both XML naming variants.
- Samples come from the real GPU session. Until then, parser tests only use
  hand-written XML that follows the documented structure, and are marked as
  such.
- On the GPU: apply each degradation, check that the published scores move in
  the expected direction, and check that placement changes.
