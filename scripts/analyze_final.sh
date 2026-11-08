#!/usr/bin/env bash
# Final analysis of the campaign (inside the bench container):
#   scripts/in_docker.sh scripts/analyze_final.sh
# 1. per-run metrics + per-experiment aggregates (bench/analysis/analyze.py)
# 2. cross-solution tables and figures (bench/analysis/report.py)
# 3. results/summary.md = cross-solution tables followed by the per-experiment tables.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"; cd "$ROOT"
python3 bench/analysis/analyze.py --raw results/raw --out results --plots none
PYTHONPATH=bench/analysis python3 bench/analysis/report.py --out results
tmp=$(mktemp)
{ cat results/report_tables.md; echo; echo "---"; echo; sed '1s/^# DIMP benchmark summary/# Per-experiment summary (all metrics)/' results/summary.md; } >"$tmp"
mv "$tmp" results/summary.md
# analyze.py references per-experiment CDFs that are not generated with --plots none
sed -i 's#^!\[CDF \(E[0-9a-z-]*\)\](plots/cdf_\(.*\)\.png)$#![upgrade-window CDF \1](plots/cdf_upgrade_\2.png)#' results/summary.md
sed -i "/cdf_upgrade_E[56]\.png/d" results/summary.md   # no CDF figure for E5/E6
echo "summary: results/summary.md, results/summary.csv, results/summary_runs.csv, results/plots/"
