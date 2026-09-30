#!/usr/bin/env bash
# Run session A2 on this Mac against the fake nvidia-smi (1 repeat, tiny runs),
# with power/clock changes allowed. Needs a Python with torch:
#   PYTHON=/path/to/venv/bin/python ./session-a2/test/dry-run.sh
set -euo pipefail
cd "$(dirname "$0")/.."
: "${PYTHON:?set PYTHON to a python that has torch}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
(cd ../../../agent && go build -o "$work/gpufit-agent" ./cmd/gpufit-agent)
cp session.sh analyze.py ../common/bench.py "$work/"
FAKE_STATE="$work/state" NVSMI="$PWD/../common/test/fake-nvidia-smi" SUDO="" PYTHON="$PYTHON" \
  AGENT="$work/gpufit-agent" QUICK=1 REPS="${REPS:-1}" \
  "$work/session.sh" "$work/out" > "$work/stdout.txt" 2>&1 || { tail -30 "$work/stdout.txt"; exit 1; }
grep -E '== |allowed|SKIPPED|FAILED|FATAL|GPU reset' "$work/out/session.log"
cat "$work/out/summary.md"
echo "samples: $(ls "$work/out/samples" | wc -l | tr -d ' ') files, e.g.:"; head -2 "$(ls -d "$work"/out/samples/* | head -1)"
echo "snapshots: $(ls "$work/out/snapshots" | wc -l | tr -d ' ') XML files"
