#!/usr/bin/env bash
# Run the experiment matrix for one or more solutions, then analyse.
#   scripts/run_matrix.sh <sol> [<sol> ...]          (all experiments)
#   EXPERIMENTS="E1 E3" REPS=3 scripts/run_matrix.sh mock s1-epoch-swap
# Inside Docker: scripts/in_docker.sh scripts/run_matrix.sh <sol>...
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"; cd "$ROOT"
source scripts/experiments.sh
EXPS="${EXPERIMENTS:-$ALL_EXPERIMENTS}"
for sol in "$@"; do
  for e in $EXPS; do
    # each experiment in a fresh shell so per-experiment defaults don't leak
    bash scripts/run_experiment.sh "$sol" "$e"
  done
done
[[ -n "${SKIP_ANALYSIS:-}" ]] || python3 bench/analysis/analyze.py --raw results/raw --out results
