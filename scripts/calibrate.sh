#!/usr/bin/env bash
# Find the saturation throughput of a solution WITHOUT upgrades, to set the E2
# rate. Sweeps rates, 8 s each; a rate "passes" if there are no errors and the
# p99 latency stays below P99_LIMIT_MS. Writes 80% of the highest passing rate
# to results/calibration/<sol>.rate (used by E2). Linux/Docker only.
#   scripts/in_docker.sh scripts/calibrate.sh mock
set -euo pipefail
export LC_ALL=C   # awk/printf decimals (host locale may use commas)
ROOT="$(cd "$(dirname "$0")/.." && pwd)"; cd "$ROOT"
SOL="${1:?usage: calibrate.sh <solution>}"
RATES="${RATES:-1000 2000 4000 8000 12000 16000 24000 32000}"
: "${P99_LIMIT_MS:=50}"
mkdir -p results/calibration
out="results/calibration/$SOL.csv"; echo "rate,achieved_rps,errors,p99_ms" >"$out"
best=0
for r in $RATES; do
  RUN_TAG="cal$r" REPS=1 RATE=$r UPGRADES=0 UPGRADE_AT_S=0 UPGRADE_INTERVAL_S=0 TAIL_S=8 CONCURRENCY=4096 TIMEOUT=2s \
    bash scripts/run_experiment.sh "$SOL" adhoc >/dev/null
  d=$(ls -td results/raw/"$SOL"/adhoc-cal"$r"-* | head -1)
  line=$(python3 - "$d/loadgen.csv" <<'PY'
import sys, pandas as pd
d = pd.read_csv(sys.argv[1]); ok = d[d.err_class == "none"]
span = (d.end_ns.max() - d.intended_ns.min()) / 1e9
print(f"{len(ok)/span:.0f},{(d.err_class!='none').sum()},{ok.latency_ns.quantile(.99)/1e6:.2f}")
PY
)
  echo "$r,$line" | tee -a "$out"
  rm -rf "$d"
  IFS=, read -r ach err p99 <<<"$line"
  if [[ "$err" == 0 ]] && awk -v p="$p99" -v l="$P99_LIMIT_MS" 'BEGIN{exit !(p<l)}'; then best=$r; else break; fi
done
rate=$(( best * 8 / 10 )); echo "$rate" >"results/calibration/$SOL.rate"
echo "saturation ~${best} rps (p99<${P99_LIMIT_MS}ms, no errors); E2 rate set to $rate"
