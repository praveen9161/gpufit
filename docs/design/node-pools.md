# Design: node-level pools (gpufit-controller)

Status: **implemented on kind, thresholds are starting guesses.**

## Why

Large synchronous training jobs take whole nodes (all GPUs of a node, joined
by NVLink). If one GPU of a node is degraded, the question isn't "which GPUs
should the job get", it's "**should this node run training at all?**". The
answer operators want: automatically move the node to an inference pool until
it's healthy again, instead of retiring all of its GPUs.

## What it does

`gpufit-controller` (one per cluster) reads the per-node scores the agents
publish in ConfigMap `gpufit-scores` and decides, per node:

| State | Condition (default thresholds) | Effect |
|---|---|---|
| node pool **training** | every GPU has training score >= 80 | nothing blocked |
| node pool **inference** | any GPU has training score < 80 | all GPUs of the node get taint `gpufit.io/unfit-training` |
| GPU **needs repair** | this GPU's inference score < 50 (unfit even for the most forgiving workload) | that GPU gets taint `gpufit.io/needs-repair` |

- Taints are **DRA device taints** (`DeviceTaintRule`), effect `NoSchedule`:
  new claims avoid the devices, **running jobs are never evicted**.
  Selecting by driver + pool (= node) + device works with any DRA driver.
- Training claims tolerate nothing. Inference and fine-tune claims made by
  `gpufit-claim` tolerate `gpufit.io/unfit-training`, so the degraded node's
  GPUs keep serving inference. Nobody tolerates `gpufit.io/needs-repair`.
- Visible to humans: node label `gpufit.io/pool=training|inference`, node
  annotation `gpufit.io/pool-reason`, and a Kubernetes Event on every change
  (`kubectl describe node`, `kubectl get events`).

## Hysteresis (no flapping)

A noisy GPU shouldn't bounce a node between pools. Changes are **asymmetric**:

- to a worse state (training -> inference, GPU -> needs repair): only after
  the condition held for `--degrade-after` (default 5m);
- back to a better state: only after being healthy for `--recover-after`
  (default 30m, longer on purpose: a GPU that just recovered is suspect).

On controller start, the first observation is applied immediately.
The kind demo uses short timers (seconds) so changes are visible.

## Not in v1

- Evicting running training jobs from a degraded node (would be `NoExecute`;
  needs a policy for checkpointing first).
- Draining a node for repair when *all* its GPUs need repair.
- Kueue integration (fitness pools as ResourceFlavors).
