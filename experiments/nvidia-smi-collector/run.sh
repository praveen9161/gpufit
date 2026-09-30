#!/usr/bin/env bash
# Guided walkthrough: what the nvidia-smi collector reads and how it scores it.
# Uses HAND-WRITTEN sample reports (agent/testdata/nvidia-smi/), not real GPUs.
# On a real GPU the same command runs on live output:
#   nvidia-smi -q -x > gpu.xml && gpufit-agent explain -xml gpu.xml
#
#   ./experiments/nvidia-smi-collector/run.sh          # step by step
#   AUTO=1 ./experiments/nvidia-smi-collector/run.sh   # run straight through
set -euo pipefail
cd "$(dirname "$0")"
source ../../scripts/guided-lib.sh

BIN_DIR=$(mktemp -d)
trap 'rm -rf "$BIN_DIR"' EXIT
(cd ../../agent && go build -o "$BIN_DIR" ./cmd/gpufit-agent)
gpufit-agent() { "$BIN_DIR/gpufit-agent" "$@"; }
XML=../../agent/testdata/nvidia-smi

step 1 "What the collector reads: one 'nvidia-smi -q -x' report per interval (XML)." \
       "The parts it uses for one GPU (power, temperature, clock event reasons):"
run "grep -E 'current_power_limit|default_power_limit|gpu_temp>|gpu_temp_slow|hw_slowdown|sw_power_cap' $XML/healthy-2gpu.xml | head -8"
pause

step 2 "Two healthy GPUs. gpu-1 is under load: 78 C and at its power cap (sw_power_cap active)." \
       "Expect: all ratios 1.00. Running at the power limit under load is NORMAL, not a fault."
run gpufit-agent explain -xml "$XML/healthy-2gpu.xml"
pause

step 3 "Power limit lowered to 70% (like 'nvidia-smi -pl 210'). Older driver: different XML names," \
       "ECC and clocks 'N/A'. Expect: compute 0.70, missing values shown as n/a (not errors)."
run gpufit-agent explain -xml "$XML/power-capped-old-names.xml"
pause

step 4 "Two broken GPUs: gpu-0 overheating (93 C, hardware throttling), gpu-1 with ECC memory errors." \
       "Expect: gpu-1 scores 0 everywhere (corrupt memory). Look at gpu-0's training vs inference score:" \
       "a throttling GPU slows a whole synchronous training job, so training should be LOWEST."
run gpufit-agent explain -xml "$XML/degraded-2gpu.xml"
note "gpu-0: training 69 > inference 51. That's the design flaw (training averages away a straggler)."
pause

step 5 "The compute formula has one unknown: how throughput falls with the power limit." \
       "power-exponent bends the curve; the real-GPU session measures it. Same 70% file, exponent 0.5:"
run "gpufit-agent explain -xml $XML/power-capped-old-names.xml -power-exponent 0.5 | grep -E 'compute|scores'"

printf '\n\033[1;36m━━ DONE ━━\033[0m\n'
