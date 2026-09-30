#!/usr/bin/env bash
# Run session.sh on this Mac against the fake nvidia-smi, twice:
# once where power/clock changes are allowed, once where they're refused
# (the case we might hit on a cloud VM). Needs a Python with torch:
#   PYTHON=/path/to/venv/bin/python ./test/dry-run.sh
set -euo pipefail
cd "$(dirname "$0")/.."
: "${PYTHON:?set PYTHON to a python that has torch}"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
(cd ../../../agent && go build -o "$work/gpufit-agent" ./cmd/gpufit-agent)
cp session.sh ../common/bench.py "$work/"

for allowed in 1 0; do
  echo "######## permissions allowed=$allowed"
  rm -rf "$work/state" "$work/out"
  FAKE_STATE="$work/state" FAKE_PL_OK=$allowed FAKE_LGC_OK=$allowed \
  NVSMI="$PWD/../common/test/fake-nvidia-smi" SUDO="" PYTHON="$PYTHON" AGENT="$work/gpufit-agent" QUICK=1 \
    "$work/session.sh" "$work/out" > "$work/stdout.txt" 2>&1 || { cat "$work/stdout.txt"; exit 1; }
  grep -E '== |allowed|SKIPPED|FAILED|FATAL|GPU reset' "$work/out/session.log"
  cat "$work/out/summary.md"
  echo "files: $(ls "$work/out" | wc -l | tr -d ' ')"
done
