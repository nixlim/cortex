#!/usr/bin/env bash
# Deep eval data collector — wrapper around eval/deep/runner (Go).
#
# This script does NOT score results. It captures verbatim recall
# output for every question (and every rephrasing) into a timestamped
# dump under eval/deep/runs/. A subsequent scoring pass (by an agent
# reading eval/deep/README.md) produces <timestamp>.scored.json.
#
# Usage:
#   bash eval/deep/run_deep_eval.sh                   # defaults
#   K=10 bash eval/deep/run_deep_eval.sh              # override recall --limit
#   NEG_THRESHOLD=0.03 bash eval/deep/run_deep_eval.sh
#   CORTEX=./cortex bash eval/deep/run_deep_eval.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

CORTEX="${CORTEX:-$REPO_ROOT/cortex}"
K="${K:-5}"
NEG_THRESHOLD="${NEG_THRESHOLD:-0.05}"
TIMEOUT="${TIMEOUT:-60}"
QUESTIONS="${QUESTIONS:-$SCRIPT_DIR/questions_deep.json}"
OUT_DIR="${OUT_DIR:-$SCRIPT_DIR/runs}"

if [[ ! -x "$CORTEX" ]]; then
  echo "ERROR: $CORTEX not found. Build first: go build -o cortex ./cmd/cortex" >&2
  exit 1
fi

cd "$REPO_ROOT"
exec go run ./eval/deep/runner \
  -cortex "$CORTEX" \
  -questions "$QUESTIONS" \
  -out "$OUT_DIR" \
  -k "$K" \
  -neg-threshold "$NEG_THRESHOLD" \
  -timeout "$TIMEOUT"
