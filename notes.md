# GPUFit notes — confusing moments & design notes

## 2026-09-29 — "Too many GPUs" experiment
Manifests: `experiments/too-many-gpus/`. Cluster: kind, 1 worker with 8 simulated GPUs (gpu-0..gpu-7).

**A. Impossible request (9 GPUs, only 8 exist)**
- Pod stays Pending forever; the auto-generated ResourceClaim stays `pending`, `status.allocation` is empty.
- Event: `0/2 nodes are available: 1 cannot allocate all claims, 1 node(s) had untolerated taint(s).
  preemption: ... Preemption is not helpful for scheduling.`
- "cannot allocate all claims" is the worker; the "taint" is just the control-plane.

**B. Contention (pod1 takes 8, pod2 wants 1)**
- pod2 Pending with the *identical* event message as case A.
- After deleting pod1, pod2 scheduled on its own (no manual retry) and got gpu-0.

**Confusing / design-relevant:**
- The scheduler message doesn't say *why* a claim can't be allocated. It's the same for
  "can never fit" (9 > 8) and "busy right now" (8/8 in use). To tell them apart you have to
  compare the claim with the ResourceSlices and the existing allocations yourself.
  -> GPUFit idea: explain Pending claims (impossible vs contended vs excluded because the
  GPUs aren't fit enough).
- All GPUs look interchangeable to the scheduler; it just picks the first free one (gpu-0).
  There's no preference or quality signal, which is the gap the fitness score fills.
- In DRA the pod doesn't request "N GPUs" directly; it references a ResourceClaimTemplate ->
  a per-pod ResourceClaim is generated (`<template>-<random>`) and deleted with the pod.
- Example driver passes the allocated devices to the container as env vars (GPU_DEVICE_0=gpu-0 ...).

## 2026-09-29 — Device taint experiment
Manifests: `experiments/device-taint/`. `DeviceTaintRule` is served at resource.k8s.io/v1 on
k8s 1.37, no feature gate needed in this kind cluster.

- Tainted gpu-3 (`gpufit.io/degraded=training:NoSchedule`) via a DeviceTaintRule selecting
  driver + pool + device.
- 8-GPU claim without a toleration -> Pending (only 7 untainted). Same generic
  "cannot allocate all claims" message; it doesn't mention the taint.
- Same 8-GPU claim with a toleration (`key: gpufit.io/degraded, operator: Exists`) -> Running on
  gpu-0..gpu-7, including gpu-3. = GPUFit's core idea done by hand.
- The toleration goes on the *claim* (`devices.requests[].exactly.tolerations`), not on the pod.
- Rule status: `EvictionInProgress=False, reason OtherEffect` -> NoSchedule never evicts;
  only NoExecute does.
- Deleting the pods: their generated claims lingered a few seconds before GC cleaned them up.

**Design implications:**
- Device taint effects are only None / NoSchedule / NoExecute. There is **no PreferNoSchedule**,
  so taints are binary: they block a GPU but can't rank it. To grade GPUs per workload, scores
  need to be device attributes that claims select with CEL. Taints are the "hard floor"
  (e.g. GPU unfit for anything).
- Pending-reason explainability gap again: the scheduler message doesn't distinguish a taint from capacity.

## 2026-09-29 — Fitness score v0 (agent/internal/fitness)
- Input per GPU: 5 health ratios in [0,1] (measured/expected): compute, memory, nvlink, network, thermal.
- Score per workload = 100 * weighted geometric mean of the ratios, plus hard floors (below floor -> 0).
  Geometric so one bad component the workload relies on isn't averaged away.
- Profiles (weights/floors) are STARTING GUESSES, not measurements: training has nvlink/network floors
  of 0.90; inference ignores nvlink. Tune in weeks 5-6 from real data.
- Fake metrics (agent/testdata/metrics.json): gpu-3 nvlink 0.60 -> training 0, finetune 93, inference 100.
  gpu-5 compute 0.80 + thermal 0.70 -> 91/88/85. (Outputs of the formula on made-up inputs, not hardware numbers.)

## 2026-09-29 — Scores as DRA device attributes (end-to-end on simulated GPUs)
Driver fork: `dra-example-driver` branch `gpufit` (image `v0.5.0-gpufit`, Helm value
`kubeletPlugin.gpufitScoresConfigMap=gpufit-scores`). Code: `cmd/dra-example-kubeletplugin/gpufit_scores.go`.
- gpufit-agent `-o configmap` -> ConfigMap `dra-example-driver/gpufit-scores` (scores.json).
- Driver polls it every 5s; on change it republishes the ResourceSlice with
  `gpufit.io/{training,finetune,inference}Score` int attributes. Calling `helper.PublishResources`
  again just works: the helper diffs and updates the slice.
- Claims select with CEL: `device.attributes["gpufit.io"].trainingScore >= 80`.
- RBAC: kubelet plugin gets `get` on exactly that one ConfigMap (resourceNames).

Results (experiments/fitness-scheduling, scores from the fake metrics, not real hardware):
- training-7 (trainingScore>=80) -> gpu 0-2,4-7; skipped gpu-3 (training 0). No taint needed.
- inference-1 (inferenceScore>=80) -> gpu-3. The degraded GPU is still used. = the GPUFit idea.
- training-8 -> Pending with 8 GPUs free (only 7 qualify). Same generic "cannot allocate all claims".
- Healed gpu-3 in the metrics, re-applied the ConfigMap -> Pending training-8 scheduled by itself
  within seconds; nothing on the pod had to change.

Open questions / next:
- Scores only affect NEW allocations. A running training job on a GPU that degrades isn't moved
  (that would be NoExecute taint territory, or a controller that drains/restarts).
- CEL is a hard threshold, not a ranking. Untested: when more GPUs qualify than requested, does the
  scheduler prefer higher scores? (training-7 needed all 7 qualifying GPUs, so this run can't tell.)
  Expectation: no, it picks any qualifying device. Test next with e.g. training-2.
- Polling a ConfigMap is fine for a demo; a watch/informer would be faster and cheaper.
