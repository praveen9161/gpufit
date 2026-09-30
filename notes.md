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

## 2026-09-29 — Does the scheduler rank by score? No.
Guided script: `experiments/ranking/run.sh` (AUTO=1 to run straight through).
Training scores set by hand, best on the highest numbers: gpu-0=80, 83, 86, 89, 92, 95, 98, gpu-7=100.
- 2 GPUs with trainingScore >= 80 (all 8 qualify) -> gpu-0 + gpu-1 (80, 83), the two WORST passing
  GPUs. Same result in 4 out of 4 runs (deterministic first-fit in slice order).
- 2 GPUs with >= 95 -> gpu-5 + gpu-6 (95, 98), again not the best pair (98, 100).
- Confirmed by the API docs (`kubectl explain ...requests.firstAvailable`): "DRA does not yet implement
  scoring, so the scheduler will select the first set of devices that satisfies all the requests ...
  Scoring will be implemented later."

**Design implications:**
- CEL thresholds = filter only. Among qualifying GPUs, placement is arbitrary (effectively device order).
- Workaround available today: `firstAvailable` subrequests tried in order, e.g. >=95, then >=90, then >=80
  -> "best tier that fits". Coarse (tiers, not true ranking) but pure upstream API. Could be generated
  for users (webhook / claim template helper) so they only say "training, min 80".
- Don't spend time on a custom scheduler plugin now; upstream says scoring is coming. Revisit later.

## 2026-09-29 — firstAvailable tiers: "best tier still free" works today
Guided script: `experiments/tiers/run.sh`. Same ascending scores (gpu-0=80 ... gpu-7=100).
One template, options tried in order: min95 -> min90 -> min80 (2 GPUs each; a 4-GPU variant for job-d).
- job-a (all free)          -> gpu-5 (95), gpu-6 (98)  via min95
- job-b (only gpu-7 >= 95)  -> gpu-4 (92), gpu-7 (100) via min90
- job-c (only 80-89 left)   -> gpu-0 (80), gpu-1 (83)  via min80
- job-d (4 GPUs, 2 free)    -> Pending, same generic "cannot allocate all claims" (no hint about options)
- free job-a's GPUs         -> job-d starts: gpu-2, 3, 5, 6 via min80
- Which option was used is recorded in the claim: `status.allocation.devices.results[].request` =
  `gpus/min95`. Dashboard shows it as TIER.
- Several pods can share one ResourceClaimTemplate; each gets its own claim.

**Limits:**
- Coarse: inside a tier it's still first-fit (job-a got 95+98, not 98+100; job-b's min90 took gpu-7=100).
- Greedy per job, no global view: an early small job can take the top tier a later big job needed.
- Each option repeats the whole request (class, count, selector) -> verbose YAML. Users shouldn't
  hand-write this; GPUFit should generate it from "workload=training, min=80".

## 2026-09-29 — gpufit-claim: users ask for "workload + min score", not YAML
`agent/cmd/gpufit-claim` (logic in `agent/internal/claims`, tested): prints a ResourceClaimTemplate with
firstAvailable tiers (default prefer 95, 90, then -min). Workload must be a fitness profile name.
Guided demo `experiments/claim-generator/run.sh`, with the agent's normal scores (gpu-3 NVLink broken,
gpu-5 slower/hot; fake metrics):
- training x6, min 80  -> gpu-0,1,2,4,6,7 via min95 (skips gpu-3=0 and gpu-5=91)
- inference x1, min 80 -> gpu-3 via min95 (inference 100). Useless for training, best for inference.
- inference x1, min 80 -> gpu-5 via min80 (inference 85). All 8 GPUs busy, none wasted.
- MutatingAdmissionPolicy is served at admissionregistration.k8s.io/v1 on this 1.37 cluster
  -> the in-cluster version (user only adds labels, the API server rewrites the claim) needs no webhook server.

## 2026-09-29 — Agent runs in-cluster (DaemonSet, per-node scores)
- `gpufit-agent run`: every 5s collector.Collect() -> ScoreAll -> server-side apply of key
  `<node>.json` in `gpufit-scores` (field manager `gpufit-agent-<node>`; each node owns its key,
  managedFields shows it). Re-applying unchanged data is a no-op, so it applies every tick and self-heals.
- Collector interface (`internal/collect`); `fake` reads ConfigMap `gpufit-fake-metrics`
  (`<node>.json` overrides `metrics.json`). nvidia-smi collector next.
- Driver (patch 0002, image v0.5.0-gpufit-2) reads `<node>.json`, falls back to `scores.json`.
- Image: host cross-compile + distroless static nonroot. RBAC: get fake metrics, get+patch scores,
  by resourceName only. gpufit-scores is created by the manifest because RBAC can't limit "create"
  to a name.
- Gotcha: `kubectl apply` of the empty gpufit-scores removed the old `scores.json` key (it was in the
  last-applied annotation from the CLI). Fine here, but CLI and agent shouldn't both manage that ConfigMap.
- Gotcha: `kubectl get -o json` hides managedFields unless `--show-managed-fields`.

Run with the guided demo (`experiments/in-cluster-agent/run.sh`), fake metrics:
- gpu-6 nvlink 1.0 -> 0.5: published training score 100 -> 0 after ~5s; agent log:
  `gpu-6: training 100 -> 0 (nvlink 0.50 < floor 0.90), finetune 100 -> 90 ...`
- training x6 min80 then got gpu-0,1,2,4,5,7 via min90 (5 GPUs >= 95 wasn't enough).
- gpu-6 repaired -> back to 100 after ~7s; the running job kept its GPUs (only new allocations change).

## 2026-09-30 — nvidia-smi collector v1 (code + hand-written samples)
Design: docs/design/nvidia-smi-collector.md. Code: agent/internal/collect/{nvsmi,window}.go.
- Parses `nvidia-smi -q -x` as a generic XML tree so both element-name generations work
  (gpu_power_readings/power_readings, clocks_event_reasons/clocks_throttle_reasons, dram_/sram_uncorrectable
  vs double_bit/total). "N/A" -> unknown -> ratio 1 with reason "n/a".
- `Windowed` wraps any collector: worst ratio per component over the window (10m default for nvidia-smi,
  off for fake so demos react in seconds).
- `gpufit-agent explain -xml f.xml`: readings -> ratios (with reasons) -> scores. First command on a real GPU.
- Test XMLs in agent/testdata/nvidia-smi/ are HAND-WRITTEN from the documented layout. Replace with real captures.
- nvlink/network: 1.0 in v1, logged as not collected/measured.

**Design flaw found (from the hand-written samples, so logic only, not measured):**
- Overheating GPU (93 C, hw_slowdown): training 69 > finetune 58 > inference 51. Backwards:
  synchronous training runs at the speed of its slowest GPU (straggler), so a throttling GPU should be
  worst for training. Power cap 70% -> training still 91.
- Cause: training profile only has floors on nvlink/network; compute/thermal get averaged away.
- Proposal: training = weakest component (min), others keep weighted mean.
  Testable on real hardware: cap one GPU in a multi-GPU job, measure whole-job step time.

## 2026-09-30 — Training score = weakest link (straggler fix)
`Profile.WeakestLink` (training only): score = 100 x lowest relevant component;
finetune/inference keep the weighted geometric mean. Floors unchanged.
- Throttling sample GPU: training 69 -> 13; 70% power cap: training 91 -> 70; fake gpu-5: training 91 -> 70
  (weakest link: thermal 0.70). Finetune/inference unchanged. Still a hypothesis until measured on a
  multi-GPU job with one capped GPU.
- Re-baselined demos: fitness-scheduling now 01-training-6 (gets the 6 qualifying GPUs) and 03-training-7
  (Pending). in-cluster-agent: 6-GPU training job now Pending after gpu-6 breaks and STARTS BY ITSELF
  when gpu-6 is repaired. Earlier notes above reflect the old scoring.
