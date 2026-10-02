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
- Gotcha found in a demo run: `scores.json` had come back (claim-generator demo still used the laptop CLI).
  Once the node key exists, the driver IGNORES scores.json, so the ranking/tiers demos (hand-set scores via
  scores.json) would silently use the agent's scores. Fix: demos no longer write scores.json; ranking/tiers
  pause the agent (DaemonSet nodeSelector nobody matches), write the node key by hand, and resume the agent
  at the end (guided-lib: pause_agent/resume_agent/set_scores). One writer per key.

## 2026-09-30 — AWS account + session A scripts (nothing applied yet)
- A dedicated AWS account with MFA and budget alerts; the CLI logs in through IAM Identity Center (SSO).
- GPU quota "Running On-Demand G and VT instances" (L-DB2E81BA): 0, request for 8 is a support case (CASE_OPENED).
- `terraform plan` (deploy/aws): 10 resources, account lock works, Base DLAMI Ubuntu 24.04 found.
  Session A overrides the AMI to "Deep Learning OSS Nvidia Driver AMI GPU PyTorch 2.12 (Ubuntu 24.04)".
- Prices checked 2026-09-30 (us-east-1 on-demand): g4dn.xlarge $0.526/h (T4), g6.xlarge $0.805 (L4), g5.xlarge $1.006 (A10G).
- experiments/real-gpu/session-a: bench.py (fp16 GEMM TFLOPS + small CNN train steps/s), session.sh (runs on the VM:
  captures, -pl/-lgc permission test, power + clock sweeps, measured vs GPUFit-predicted), run.sh (Mac: apply ->
  run -> download -> destroy -> verify). Dry-run on the Mac against test/fake-nvidia-smi: both "allowed" and
  "refused" paths work; GPU always reset.
- **Gap found in the dry run:** locking clocks with -lgc leaves GPUFit compute at 1.0, because the compute ratio
  only reads *applications* clocks. The real XML capture should show where locked clocks appear; fix after.

## 2026-09-30 — Node-level pools (gpufit-controller) on kind
Design: docs/design/node-pools.md. Guided demo: experiments/node-pools/run.sh (~2 min, demo timers 10s/30s).
- Node -> inference pool if any GPU's training score < 80 (DeviceTaintRule gpufit.io/unfit-training on the whole
  pool = node); GPU -> needs-repair if inference < 50 (per-device rule). Node label/annotation + Events.
- Test run (fake metrics): healthy node -> training pool; 4-GPU training job runs. gpu-5 thermal 0.7 ->
  inference pool after ~17s (agent 5s + degrade-after 10s); running job NOT evicted; new 2-GPU training job
  Pending; inference job Running on the same node (tolerates unfit-training). gpu-7 ECC -> only gpu-7 gets
  needs-repair. Repair all -> back to training after ~34s (recover-after 30s), Pending training job started
  by itself. Events: GPUFitPoolChanged / GPUFitNeedsRepair visible via kubectl get events.
- With 1 GPU node, "training moves to another node" can't be shown; that needs several GPU nodes (shown later on real GPUs, sessions B and C).
- Controller isn't left running between demos: with the default fake metrics (gpu-3/gpu-5 degraded) it would
  keep the only node in the inference pool and block the other demos' training jobs.

## 2026-09-30 — `kubectl gpufit why`: explain Pending GPU pods
agent/cmd/kubectl-gpufit + internal/why. Per claim option, every device is classified (in order):
wrong class -> selector false (shows the attribute value, e.g. trainingScore 70) -> tainted, not tolerated
(adds the node's pool reason for gpufit.io/unfit-training) -> in use (by which pod) -> available.
Selectors are evaluated with k8s.io/dynamic-resource-allocation/cel (the scheduler's own CEL code).
Out of scope: node-level constraints (node selectors, CPU/memory); the scheduler event still shows those.
Demo (experiments/why/run.sh), both pods got the SAME scheduler message
("0/2 nodes are available: 1 cannot allocate all claims, ..."):
- train-busy (4 GPUs):  "closest is gpus/min95 with 3 of 4 usable; blocked by 5 busy" (lists who holds them)
- train-unfit (2 GPUs): "0 of 2 usable; blocked by 7 tainted, 1 failing the selector" + "node is in the
  inference pool (gpu-5 training 70 < 80)", while 3 GPUs were actually free.
Works with any DRA driver; only the gpufit.io notes need GPUFit. Candidate for upstream discussion.

## 2026-09-30 — SESSION A: first real GPU (T4) — details in experiments/real-gpu/results/session-a-20260930-1055/README.md
- AWS g4dn.xlarge, Tesla T4, driver 595.91.07. ~33 min, ~$0.25-0.30. Destroyed + verified nothing left.
- Gotcha: first apply failed: AWS rejects "'" in security-group rule descriptions ("operator's IP").
  The VM was created anyway (billing, unreachable); fixed the description, re-ran run.sh -> "1 to add".
- `nvidia-smi -pl` and `-lgc` both ALLOWED on this VM. T4 power range is only 60-70 W.
- Power limit: throughput ~ power_ratio^a, a = 1.11 (GEMM), 1.03 (CNN train); GPUFit default a=1.0 within
  noise (baseline spread 1.7%). Predicted vs measured within ~3%.
- Clock lock: GEMM unaffected (T4 already power-capped at 660 MHz under GEMM); CNN train 0.86 at 50% lock
  while GPUFit said 1.00 -> blind spot confirmed. Same degradation, different impact per workload.
- nvidia-smi on this driver: applications clocks "deprecated" (collector clock ratio has no data);
  a lock only shows as the current clock (no flag/event); throttle-reason COUNTERS (cumulative us) exist
  and should replace the instantaneous Active flags in the window.
- Next measurement improvements: repeat runs, continuous clock/power sampling, wider-power-range GPU, 2+ GPUs.

## 2026-09-30 — Rule B: "unexplained clock deficit" (passive clock-lock detection)
A healthy busy GPU is either at max clock or power-capped. If it's busy (util >= 90%), below 95% of max clock,
below 95% of its power limit, and no throttle reason is active -> something (e.g. a clock lock) holds it back.
Estimate compute = power draw / limit (unused power headroom; consistent with throughput ~ power, a~1).
On all 22 session-A captures: flags exactly the 2 runs slowed by a clock lock (50%: est 0.87 vs measured 0.86;
60%: est 0.89 vs 0.94), no false alarms (power-capped and idle captures explained/skipped).
Max error vs measured: 0.14 -> 0.05. CAVEAT: rule designed on this same data (in-sample) -> validate in
session A2 with continuous sampling, other GPUs (L4/A10G), repeated runs. Probe design now secondary:
use it mainly as the gate back into the training pool (option D).

## 2026-09-30 — Session A2 prepared (L4), not run yet
- experiments/real-gpu/run.sh <session> is now generic (session.env: instance type, price, AMI); session-a/run.sh wraps it.
- session-a2: 3 reps/setting; 200 ms --query-gpu sampling incl. throttle flags + counters (field names verified from the
  T4's --help-query-gpu); XML snapshot every 5 s judged by the real Go collector (validates rule B on new data);
  probe prototype = bench.py probe (gemm + CNN + copy).
- Dry-run bug found: `explain | grep -q UNEXPLAINED` under `set -o pipefail` reports a MATCH as failure (grep exits
  early -> SIGPIPE upstream). Flags went 2/5 instead of 5/5. Fixed by capturing output once.
- terraform plan for g6.xlarge: us-east-1a, PyTorch 2.12 AMI, 10 to add (read-only, nothing created).

## 2026-10-01 — SESSION A2 (A10G) + refocus on cost
Details: experiments/real-gpu/results/session-a2-20261001-0929/README.md. ~$1.12. Total real-GPU spend so far ~$1.45.
- L4 (g6.xlarge) had no capacity in us-east-1a/1b (InsufficientInstanceCapacity; the provider retried silently
  for 20+ min). Added: availability_zone variable, 4m create timeout, INSTANCE_TYPE override. Ran on A10G (1c).
- Same degradation, very different cost per workload: 150 W -> heavy GEMM 0.68, CNN 0.94.
- v1 power model and rule B's size estimate are T4-specific (too pessimistic on the A10G); rule B DETECTION held
  (all lock snapshots flagged, no false alarms). Probe matches per-workload impact.
- Decision: passive signals = trigger, probe = per-workload score. STOP measurement deep-dives.
- Refocus: the goal is cost. Next = session B on 2 GPUs: real Kubernetes + NVIDIA DRA driver + GPUFit,
  same training job with vs without GPUFit -> measured job time, GPU-hours, $. Then a cost calculator.

## 2026-10-01 — Session B built and rehearsed (not run)
Plan: one session, Kubernetes + cost (docs/design/session-b-cost.md). Built: 2-VM Terraform (VM-to-VM traffic
allowed, internet still SSH-only), multi-VM runner with orchestrate.sh hook, DDP training + decode-style inference,
deploy/real/gpufit.yaml (agent reads host nvidia-smi via NVIDIA runtime; controller for gpu.nvidia.com), render.sh
(Indexed Job + headless Service rendezvous), analyze.py ($ per useful GPU-hour), fallback.sh (same measurement
without Kubernetes).
Rehearsed on kind (CPU image, example driver): 2-pod training rendezvous works (world_size 2), results printed,
controller taint -> training Pending + `kubectl gpufit why` explains it, inference still runs; manifests pass
server-side validation. NOT testable locally: k3s install, NVIDIA DRA driver + CDI, nvidia-smi in the agent pod,
NCCL between VMs, 3 GB image pulls -> covered by the fallback.
Versions: k3s v1.37.1+k3s1, dra-driver-nvidia-gpu chart 0.5.0 (DeviceClass gpu.nvidia.com, pool = node name),
pytorch/pytorch:2.12.1-cuda13.0-cudnn9-runtime (same torch as sessions A/A2).

## 2026-10-01 — SESSION B: the project's claim, measured (Kubernetes + real GPUs + cost)
Details: experiments/real-gpu/results/session-b-20261001-1128/README.md. 2x g5.xlarge, 54 min, ~$1.9.
Total real-GPU spend so far ~$3.3.
- k3s v1.37.1 + NVIDIA DRA driver 0.5.0 + GPUFit (real nvidia-smi agent, controller for gpu.nvidia.com) WORKED.
- Without GPUFit: 2-GPU training with B at 150 W -> 0.84 of healthy (straggler). Efficiency 74%, $1.35/useful GPU-h.
- With GPUFit: B -> inference pool (DeviceTaintRule), 2-GPU training Pending + `kubectl gpufit why` explains it;
  training on A at 1.00, inference on degraded B at 0.98. Efficiency 99%, $1.02/useful GPU-h -> -25%.
- Caveat: no spare GPU -> training job slower (0.67x). Fleet case -> cost calculator.
- Fixed on the run: k3s pod CIDR clashed with the VPC (10.42/16); NVIDIA DRA plugin needs nvidia.com/gpu.present
  label; never edit a running bash script (resume.sh rescued the session); don't print the join token.

## 2026-10-01 — Cost calculator (docs/calculator/index.html)
Model per hour, in GPU-hours: stragglers = fleet x degraded% x training share; lost without GPUFit =
stragglers x job GPUs x (1 - training speed); lost with GPUFit = stragglers x (1 - inference speed) (degraded GPU
swaps with a healthy one from the inference side). Scenarios from sessions A2/B, each value tagged measured or
estimate; degraded% is the user's own number (no public data). Default example (1024 GPUs, 2%, 60%, 8-GPU jobs,
session B speeds, $2/h, 85% busy): ~15.5 GPU-h/h recovered, ~$231k/yr, ~1.5% of fleet time. Example inputs, not a claim.

## 2026-10-02 — v0.1 step 3: rescue running training jobs (experiments/rescue/, kind, CPU)
Controller `--rescue --rescue-grace`: an opted-in job (pod label gpufit.io/rescue=true) holding a GPU on a node
that left the training pool for longer than the grace period gets its pods evicted (Eviction API). The Job's
podFailurePolicy ignores DisruptionTarget, so the pods are recreated; train_ddp.py job mode resumes from the
last checkpoint (ckpt_store.py, a tiny HTTP store standing in for S3/NFS).
Rehearsal (2-pod DDP, 600 steps, checkpoint every 20): degraded -> inference pool ~17s, evicted 25s later
(20s grace + 5s interval), pods Pending (one GPU node), repaired -> resumed_from 120, finished 600 steps.
Confusing moments:
- Replacement pods appear only ~20s after the eviction: with a podFailurePolicy the Job's podReplacementPolicy
  defaults to Failed (waits until the old pod is fully gone). A script that checked after 5s saw no pods.
- Evicted pods are Terminating for a while and still hold their claims; the controller must skip terminating
  pods or it counts the job again (double eviction, double-counted report).
- The guided `run` helper uses eval: an argument like '.event=="checkpoint"' loses its quotes. Pass the whole
  command as one quoted string.
- Pools are per node, so "moved off gpu-0" in the log meant the job's first GPU on the node, not the bad GPU (gpu-5).

## 2026-10-02 — v0.1 step 4: Helm chart (charts/gpufit) + release workflow
`helm install gpufit charts/gpufit -n gpufit --create-namespace -f charts/gpufit/ci/kind-values.yaml` on kind:
observe -> status says "inference (would apply)", no taints; `helm upgrade --reuse-values --set mode=enforce,rescue.enabled=true`
-> taint + label, pods/eviction RBAC added, metrics gpufit_mode_enforce 1.
Confusing moments:
- Helm merges maps: `nodeSelector: {}` in a values file does NOT remove the default nvidia.com/gpu.present
  selector (the agent would never schedule on kind). Set the key to null.
- `helm uninstall` left the taint and pool label behind: training would avoid those GPUs forever. Added a
  pre-delete hook Job running `gpufit-controller --cleanup`. First try still failed: the controller was still
  running and re-applied the taint 1s after cleanup. The hook now scales the controller to 0 and waits first.
- `--set-string` can't carry JSON with commas; fake metrics go in a values file.
