#!/usr/bin/env bash
# Session A, runs ON THE GPU MACHINE (copied there by run.sh). Measures, for
# each power limit / clock lock: real throughput vs what GPUFit predicts.
# Everything goes to $OUT; the GPU is always reset on exit.
#
#   ./session.sh [OUT_DIR]
#
# Test hooks (local dry run): NVSMI=fake-nvidia-smi SUDO="" PYTHON=... QUICK=1
set -uo pipefail   # no -e: a failed step is recorded, the session continues

cd "$(dirname "$0")"
OUT="${1:-$HOME/gpufit-results}"
NVSMI="${NVSMI:-nvidia-smi}"
SUDO="${SUDO-sudo}"
AGENT="${AGENT:-./gpufit-agent}"
WARMUP=10 SECONDS_PER_RUN=20
[[ "${QUICK:-0}" == 1 ]] && WARMUP=0.5 SECONDS_PER_RUN=1
mkdir -p "$OUT"

log() { echo "[$(date +%H:%M:%S)] $*" | tee -a "$OUT/session.log"; }

# Pull a number out of the XML, e.g. xmlnum idle.xml max_power_limit
# (requires at least one digit, so "<default_power_limit>N/A" is skipped)
xmlnum() { grep -o "<$2>[0-9][0-9.]*" "$1" | head -1 | sed 's/.*>//'; }
calc() { awk "BEGIN { print $* }"; }   # bc isn't guaranteed on the image

# ---- find a Python that has torch (location differs between images) ----------
PYTHON="${PYTHON:-}"
if [[ -z "$PYTHON" ]]; then
  for p in python3 /opt/pytorch/bin/python3 /opt/conda/envs/pytorch/bin/python; do
    if "$p" -c 'import torch' 2>/dev/null; then PYTHON=$p; break; fi
  done
fi
[[ -n "$PYTHON" ]] || { log "FATAL: no python with torch found"; exit 1; }

# ---- always leave the GPU as we found it ---------------------------------------
DEFAULT_PL=""
reset_gpu() {
  $SUDO $NVSMI -rgc >/dev/null 2>&1
  [[ -n "$DEFAULT_PL" ]] && $SUDO $NVSMI -pl "$DEFAULT_PL" >/dev/null 2>&1
  log "GPU reset (clocks unlocked, power limit ${DEFAULT_PL:-unchanged})"
}
trap reset_gpu EXIT

# Run one benchmark; capture nvidia-smi XML halfway through (= under load) and
# GPUFit's explanation of it; append one JSON line to results.jsonl.
measure() {  # measure <label> <workload> <power_limit_w|""> <locked_clock_mhz|"">
  local label=$1 workload=$2 pl=$3 clk=$4
  local xml="$OUT/load-$label-$workload.xml"
  "$PYTHON" bench.py "$workload" --warmup "$WARMUP" --seconds "$SECONDS_PER_RUN" > "$OUT/bench.tmp" 2>>"$OUT/session.log" &
  local pid=$!
  sleep "$(calc "$WARMUP + $SECONDS_PER_RUN / 2")"
  $NVSMI -q -x > "$xml" 2>>"$OUT/session.log"
  wait $pid || { log "bench $label/$workload FAILED"; return; }
  "$AGENT" explain -xml "$xml" > "$xml.explain.txt" 2>&1
  local compute training
  compute=$(awk '$1 == "compute" {print $2; exit}' "$xml.explain.txt")
  training=$(sed -n 's/.*scores: *training \([0-9]*\).*/\1/p' "$xml.explain.txt" | head -1)
  "$PYTHON" - "$OUT/bench.tmp" "$label" "$pl" "$clk" "${compute:-}" "${training:-}" >> "$OUT/results.jsonl" <<'PY'
import json, sys
row = json.load(open(sys.argv[1]))
label, pl, clk, compute, training = sys.argv[2:7]
row.update(label=label, power_limit_w=float(pl) if pl else None, locked_clock_mhz=int(clk) if clk else None,
           gpufit_compute=float(compute) if compute else None, gpufit_training=int(training) if training else None)
print(json.dumps(row))
PY
  log "$label $workload: $(cat "$OUT/bench.tmp")  | GPUFit compute=${compute:-?} training=${training:-?}"
}

# ---- 1. environment + real captures -------------------------------------------
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
# nvidia-smi -pl wants whole watts
[[ -n "$DEFAULT_PL" ]] && DEFAULT_PL=$(printf '%.0f' "$DEFAULT_PL")
[[ -n "$MIN_PL" ]] && MIN_PL=$(printf '%.0f' "$MIN_PL")
MAX_PL=$(xmlnum "$OUT/idle.xml" max_power_limit)
MAX_CLK=$(sed -n '/<max_clocks>/,/<\/max_clocks>/p' "$OUT/idle.xml" | xmlnum /dev/stdin graphics_clock)
log "power limit: default=${DEFAULT_PL:-?} min=${MIN_PL:-?} max=${MAX_PL:-?} W; max graphics clock=${MAX_CLK:-?} MHz"

# ---- 2. are we allowed to degrade the GPU? ----------------------------------------
log "== 2. permission tests"
$SUDO $NVSMI -pm 1 >> "$OUT/session.log" 2>&1; log "persistence mode rc=$?"
PL_OK=0 LGC_OK=0
if [[ -n "$MIN_PL" && -n "$DEFAULT_PL" ]]; then
  if $SUDO $NVSMI -pl "$MIN_PL" >> "$OUT/session.log" 2>&1; then PL_OK=1; fi
  $SUDO $NVSMI -pl "$DEFAULT_PL" >> "$OUT/session.log" 2>&1
fi
if [[ -n "$MAX_CLK" ]]; then
  half=$((${MAX_CLK%.*} / 2))
  if $SUDO $NVSMI -lgc "$half,$half" >> "$OUT/session.log" 2>&1; then LGC_OK=1; fi
  $SUDO $NVSMI -rgc >> "$OUT/session.log" 2>&1
fi
log "power limit change allowed: $PL_OK   clock lock allowed: $LGC_OK"
echo "{\"power_limit_change\": $PL_OK, \"clock_lock\": $LGC_OK}" > "$OUT/permissions.json"

# ---- 3. baseline (3x gemm for noise, 1x train) --------------------------------------
log "== 3. baseline"
for i in 1 2 3; do measure "baseline$i" gemm "$DEFAULT_PL" ""; done
measure baseline1 train "$DEFAULT_PL" ""

# ---- 4. power sweep: default -> min in 4 steps ---------------------------------------
if [[ $PL_OK == 1 ]]; then
  log "== 4. power sweep"
  for f in 0.75 0.5 0.25 0; do
    pl=$(printf '%.0f' "$(calc "$MIN_PL + ($DEFAULT_PL - $MIN_PL) * $f")")
    $SUDO $NVSMI -pl "$pl" >> "$OUT/session.log" 2>&1
    measure "power${pl}W" gemm "$pl" ""
    measure "power${pl}W" train "$pl" ""
  done
  $SUDO $NVSMI -pl "$DEFAULT_PL" >> "$OUT/session.log" 2>&1
else
  log "== 4. power sweep SKIPPED (not allowed on this machine)"
fi

# ---- 5. clock sweep: 90% -> 50% of max graphics clock ------------------------------
if [[ $LGC_OK == 1 ]]; then
  log "== 5. clock sweep"
  for pct in 90 80 70 60 50; do
    clk=$(( ${MAX_CLK%.*} * pct / 100 ))
    $SUDO $NVSMI -lgc "$clk,$clk" >> "$OUT/session.log" 2>&1
    measure "clock${pct}pct" gemm "" "$clk"
    measure "clock${pct}pct" train "" "$clk"
  done
  $SUDO $NVSMI -rgc >> "$OUT/session.log" 2>&1
else
  log "== 5. clock sweep SKIPPED (not allowed on this machine)"
fi

# ---- 6. summary: measured vs predicted ------------------------------------------
$NVSMI -q -x > "$OUT/idle-after.xml"
"$PYTHON" - "$OUT/results.jsonl" > "$OUT/summary.md" <<'PY'
import json, statistics, sys
rows = [json.loads(l) for l in open(sys.argv[1])]
base = {w: statistics.median(r["value"] for r in rows if r["label"].startswith("baseline") and r["workload"] == w)
        for w in {r["workload"] for r in rows if r["label"].startswith("baseline")}}
print("| setting | workload | measured | vs baseline | GPUFit compute | GPUFit training |")
print("|---|---|---|---|---|---|")
for r in rows:
    rel = f"{r['value'] / base[r['workload']]:.2f}" if base.get(r["workload"]) else "?"
    print(f"| {r['label']} | {r['workload']} | {r['value']} {r['unit']} | {rel} | "
          f"{r['gpufit_compute']} | {r['gpufit_training']} |")
PY
cat "$OUT/summary.md"
log "== done. results in $OUT"
