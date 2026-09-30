#!/usr/bin/env bash
# Session A2, runs ON THE GPU MACHINE (copied there by ../run.sh with
# common/bench.py and gpufit-agent). Goals (see docs/design/active-probe.md):
#   1. repeat every setting 3x, so we know the noise
#   2. sample clock/power/utilization/throttle reasons+counters every 200 ms
#   3. validate the "unexplained clock deficit" rule on NEW data: every 5 s an
#      nvidia-smi XML snapshot is judged by the real Go collector
#   4. run the probe prototype (gemm + CNN + copy) at every setting
# The GPU is always reset on exit.
#
#   ./session.sh [OUT_DIR]
# Test hooks (local dry run): NVSMI=fake SUDO="" PYTHON=... QUICK=1 REPS=1
set -uo pipefail

cd "$(dirname "$0")"
OUT="${1:-$HOME/gpufit-results}"
NVSMI="${NVSMI:-nvidia-smi}"
SUDO="${SUDO-sudo}"
AGENT="${AGENT:-./gpufit-agent}"
REPS="${REPS:-3}"
WARMUP=10 SECONDS_PER_RUN=20 PROBE_SECONDS=3 SNAPSHOT_EVERY=5 SAMPLE_MS=200
[[ "${QUICK:-0}" == 1 ]] && WARMUP=0.5 SECONDS_PER_RUN=1.5 PROBE_SECONDS=0.3 SNAPSHOT_EVERY=0.5
mkdir -p "$OUT"/{samples,snapshots}

FIELDS=timestamp,clocks.gr,clocks.max.gr,power.draw.instant,enforced.power.limit,utilization.gpu,temperature.gpu
FIELDS+=,clocks_event_reasons.sw_power_cap,clocks_event_reasons.hw_slowdown,clocks_event_reasons.hw_thermal_slowdown
FIELDS+=,clocks_event_reasons.sw_thermal_slowdown,clocks_event_reasons_counters.sw_power_cap
FIELDS+=,clocks_event_reasons_counters.hw_thermal_slowdown,clocks_event_reasons_counters.sw_thermal_slowdown

log() { echo "[$(date +%H:%M:%S)] $*" | tee -a "$OUT/session.log"; }
xmlnum() { grep -o "<$2>[0-9][0-9.]*" "$1" | head -1 | sed 's/.*>//'; }
calc() { awk "BEGIN { print $* }"; }

PYTHON="${PYTHON:-}"
if [[ -z "$PYTHON" ]]; then
  for p in python3 /opt/pytorch/bin/python3 /opt/conda/envs/pytorch/bin/python; do
    if "$p" -c 'import torch' 2>/dev/null; then PYTHON=$p; break; fi
  done
fi
[[ -n "$PYTHON" ]] || { log "FATAL: no python with torch found"; exit 1; }

DEFAULT_PL=""
reset_gpu() {
  $SUDO $NVSMI -rgc >/dev/null 2>&1
  [[ -n "$DEFAULT_PL" ]] && $SUDO $NVSMI -pl "$DEFAULT_PL" >/dev/null 2>&1
  jobs -p | xargs -r kill 2>/dev/null
  log "GPU reset (clocks unlocked, power limit ${DEFAULT_PL:-unchanged})"
}
trap reset_gpu EXIT

# measure <label> <rep> <workload> <power_limit|""> <locked_clock|"">
# Runs the benchmark with the 200 ms sampler and periodic XML snapshots in the
# background; appends one JSON line to results.jsonl.
measure() {
  local label=$1 rep=$2 workload=$3 pl=$4 clk=$5
  local id="$label-$workload-r$rep" args=(--warmup "$WARMUP" --seconds "$SECONDS_PER_RUN")
  [[ $workload == probe ]] && args=(--warmup "$PROBE_SECONDS" --seconds "$PROBE_SECONDS")

  $NVSMI --query-gpu="$FIELDS" --format=csv,noheader,nounits -lms "$SAMPLE_MS" > "$OUT/samples/$id.csv" 2>/dev/null &
  local sampler=$!
  ( i=0; while true; do sleep "$SNAPSHOT_EVERY"; $NVSMI -q -x > "$OUT/snapshots/$id-$i.xml" 2>/dev/null; i=$((i+1)); done ) &
  local snapper=$!
  "$PYTHON" bench.py "$workload" "${args[@]}" > "$OUT/bench.tmp" 2>>"$OUT/session.log"
  local rc=$?
  kill "$sampler" "$snapper" 2>/dev/null; wait "$sampler" "$snapper" 2>/dev/null
  [[ $rc == 0 ]] || { log "bench $id FAILED"; return; }

  # The real Go collector's verdict on every snapshot of this run.
  local verdicts=""
  for x in "$OUT"/snapshots/"$id"-*.xml; do
    [[ -s "$x" ]] || continue
    # Run explain once and inspect the text (piping into "grep -q" would make
    # pipefail report a match as a failure).
    local e c d=0; e=$("$AGENT" explain -xml "$x" 2>/dev/null)
    c=$(awk '$1 == "compute" {print $2; exit}' <<< "$e")
    [[ "$e" == *UNEXPLAINED* ]] && d=1
    [[ -n "$c" ]] && verdicts+="$c:$d "
  done

  "$PYTHON" - "$OUT/bench.tmp" "$label" "$rep" "$pl" "$clk" "$verdicts" >> "$OUT/results.jsonl" <<'PY'
import json, statistics, sys
row = json.load(open(sys.argv[1]))
label, rep, pl, clk, verdicts = sys.argv[2:7]
v = [tuple(x.split(":")) for x in verdicts.split()]
row.update(label=label, rep=int(rep), power_limit_w=float(pl) if pl else None,
           locked_clock_mhz=int(clk) if clk else None,
           gpufit_compute=statistics.median(float(c) for c, _ in v) if v else None,
           deficit_snapshots=sum(int(d) for _, d in v), snapshots=len(v))
print(json.dumps(row))
PY
  log "$id: $(head -c 160 "$OUT/bench.tmp")  | GPUFit compute(median)=$(tail -1 "$OUT/results.jsonl" | "$PYTHON" -c 'import json,sys; r=json.load(sys.stdin); print(r["gpufit_compute"], "deficit", r["deficit_snapshots"], "/", r["snapshots"])')"
}

setting() {  # setting <label> <pl|""> <clk|"">
  for rep in $(seq 1 "$REPS"); do
    for w in gemm train probe; do measure "$1" "$rep" "$w" "$2" "$3"; done
  done
}

# ---- 1. environment + captures ---------------------------------------------
log "== 1. environment"
uname -a > "$OUT/uname.txt"
$NVSMI > "$OUT/nvidia-smi.txt" 2>&1
$NVSMI --help-query-gpu > "$OUT/help-query-gpu.txt" 2>&1
$NVSMI -q -d POWER,CLOCK,SUPPORTED_CLOCKS,TEMPERATURE,PERFORMANCE > "$OUT/q-details.txt" 2>&1
"$PYTHON" -c 'import torch; print("torch", torch.__version__, "cuda", torch.version.cuda)' | tee -a "$OUT/session.log"
$NVSMI -q -x > "$OUT/idle.xml"
"$AGENT" explain -xml "$OUT/idle.xml" | tee "$OUT/idle.xml.explain.txt"
DEFAULT_PL=$(xmlnum "$OUT/idle.xml" default_power_limit)
MIN_PL=$(xmlnum "$OUT/idle.xml" min_power_limit)
MAX_CLK=$(sed -n '/<max_clocks>/,/<\/max_clocks>/p' "$OUT/idle.xml" | xmlnum /dev/stdin graphics_clock)
[[ -n "$DEFAULT_PL" ]] && DEFAULT_PL=$(printf '%.0f' "$DEFAULT_PL")
[[ -n "$MIN_PL" ]] && MIN_PL=$(printf '%.0f' "$MIN_PL")
log "power limit: default=${DEFAULT_PL:-?} min=${MIN_PL:-?} W; max graphics clock=${MAX_CLK:-?} MHz; reps=$REPS"

# ---- 2. permissions ---------------------------------------------------------
log "== 2. permission tests"
$SUDO $NVSMI -pm 1 >> "$OUT/session.log" 2>&1
PL_OK=0 LGC_OK=0
if [[ -n "$MIN_PL" && -n "$DEFAULT_PL" ]]; then
  $SUDO $NVSMI -pl "$MIN_PL" >> "$OUT/session.log" 2>&1 && PL_OK=1
  $SUDO $NVSMI -pl "$DEFAULT_PL" >> "$OUT/session.log" 2>&1
fi
if [[ -n "$MAX_CLK" ]]; then
  half=$((${MAX_CLK%.*} / 2))
  $SUDO $NVSMI -lgc "$half,$half" >> "$OUT/session.log" 2>&1 && LGC_OK=1
  $SUDO $NVSMI -rgc >> "$OUT/session.log" 2>&1
fi
log "power limit change allowed: $PL_OK   clock lock allowed: $LGC_OK"
echo "{\"power_limit_change\": $PL_OK, \"clock_lock\": $LGC_OK}" > "$OUT/permissions.json"

# ---- 3. baseline ---------------------------------------------------------------
log "== 3. baseline"
setting baseline "$DEFAULT_PL" ""

# ---- 4. power sweep: 4 steps from default down to min -----------------------------
if [[ $PL_OK == 1 ]]; then
  log "== 4. power sweep"
  for f in 0.75 0.5 0.25 0; do
    pl=$(printf '%.0f' "$(calc "$MIN_PL + ($DEFAULT_PL - $MIN_PL) * $f")")
    $SUDO $NVSMI -pl "$pl" >> "$OUT/session.log" 2>&1
    setting "power${pl}W" "$pl" ""
  done
  $SUDO $NVSMI -pl "$DEFAULT_PL" >> "$OUT/session.log" 2>&1
else
  log "== 4. power sweep SKIPPED"
fi

# ---- 5. clock sweep: 90 / 70 / 50 % of max ------------------------------------------
if [[ $LGC_OK == 1 ]]; then
  log "== 5. clock sweep"
  for pct in 90 70 50; do
    clk=$(( ${MAX_CLK%.*} * pct / 100 ))
    $SUDO $NVSMI -lgc "$clk,$clk" >> "$OUT/session.log" 2>&1
    setting "clock${pct}pct" "" "$clk"
  done
  $SUDO $NVSMI -rgc >> "$OUT/session.log" 2>&1
else
  log "== 5. clock sweep SKIPPED"
fi

# ---- 6. summary -------------------------------------------------------------------
$NVSMI -q -x > "$OUT/idle-after.xml"
"$PYTHON" analyze.py "$OUT" > "$OUT/summary.md"
cat "$OUT/summary.md"
log "== done. results in $OUT"
