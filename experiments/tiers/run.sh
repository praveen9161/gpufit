#!/usr/bin/env bash
# Guided experiment: can firstAvailable give jobs the BEST tier of GPUs that is
# still free, even though DRA itself doesn't rank by score?
#
#   ./experiments/tiers/run.sh          # step by step, press Enter between steps
#   AUTO=1 ./experiments/tiers/run.sh   # run straight through
#
# Keep ./scripts/gpu-dash.sh open in a second terminal (watch the TIER column).
set -euo pipefail
cd "$(dirname "$0")"
source ../../scripts/guided-lib.sh

kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS" >/dev/null

step 1 "Same scores as the ranking test: every GPU passes, best on the highest numbers." \
       "  gpu-0=80  gpu-1=83  gpu-2=86  gpu-3=89  gpu-4=92  gpu-5=95  gpu-6=98  gpu-7=100" \
       "Last time, a plain '>= 80' claim got gpu-0 + gpu-1: the two WORST passing GPUs."
pause
note "The agent would overwrite hand-written scores every 5s, so pause it for this experiment:"
run pause_agent
run "set_scores \"\$(jq -r '.data[\"scores.json\"]' 00-scores-ascending.json)\""
want=$(jq -r '.data["scores.json"]' 00-scores-ascending.json | jq -c 'map_values(.training)')
run wait_for_scores "'$want'"
pause

step 2 "One claim template with three options, tried IN ORDER:" \
       "  min95: 2 GPUs with trainingScore >= 95" \
       "  min90: 2 GPUs with trainingScore >= 90   (only if min95 can't be satisfied)" \
       "  min80: 2 GPUs with trainingScore >= 80   (only if min90 can't be satisfied)" \
       "Several pods can share one template; each pod still gets its OWN claim."
run "grep -E 'name: min|expression' 01-template-2gpus.yaml"
run kubectl apply -f 01-template-2gpus.yaml -f 02-template-4gpus.yaml
pause

step 3 "job-a: all 8 GPUs free. Expect option min95 -> gpu-5 (95) + gpu-6 (98)."
run kubectl apply -f 11-job-a.yaml
run kubectl wait -n "$NS" --for=condition=Ready pod/job-a --timeout=90s
note "job-a got: $(show_got job-a)"
pause

step 4 "job-b: only gpu-7 is still >= 95. That's 1 GPU, job needs 2 -> min95 fails." \
       "Expect fallback to min90: free GPUs >= 90 are gpu-4 (92) and gpu-7 (100)."
run kubectl apply -f 12-job-b.yaml
run kubectl wait -n "$NS" --for=condition=Ready pod/job-b --timeout=90s
note "job-b got: $(show_got job-b)"
pause

step 5 "job-c: only gpu-0..gpu-3 (80-89) left. min95 and min90 both fail." \
       "Expect fallback to min80 -> gpu-0 + gpu-1."
run kubectl apply -f 13-job-c.yaml
run kubectl wait -n "$NS" --for=condition=Ready pod/job-c --timeout=90s
note "job-c got: $(show_got job-c)"
pause

step 6 "job-d asks for 4 GPUs (same three options). Only gpu-2 + gpu-3 are free." \
       "No option can be satisfied -> expect Pending. Does the message say anything about tiers?"
run kubectl apply -f 14-job-d.yaml
sleep 6
run kubectl get pod -n "$NS" job-d
note "why job-d is Pending: $(why_pending job-d)"
pause

step 7 "Free job-a's GPUs (gpu-5, gpu-6). Now 4 are free: gpu-2, gpu-3, gpu-5, gpu-6." \
       "min95 needs 4 GPUs >= 95 (only 2 free), min90 same, min80 fits -> job-d should start."
run kubectl delete -f 11-job-a.yaml --wait
run kubectl wait -n "$NS" --for=condition=Ready pod/job-d --timeout=90s
note "job-d got: $(show_got job-d)"
pause

step 8 "Clean up and resume the agent; it re-publishes the real scores (gpu-3 degraded)."
run kubectl delete -f 12-job-b.yaml -f 13-job-c.yaml -f 14-job-d.yaml --wait
run kubectl delete -f 01-template-2gpus.yaml -f 02-template-4gpus.yaml
run resume_agent
want='{"gpu-0":100,"gpu-1":100,"gpu-2":100,"gpu-3":0,"gpu-4":100,"gpu-5":70,"gpu-6":100,"gpu-7":100}'
run wait_for_scores "'$want'"

printf '\n\033[1;36m━━ DONE ━━\033[0m\n'
echo "Check each job against its 'Expect' line. Compare with the ranking test, where plain '>= 80' got the worst GPUs."
