#!/usr/bin/env bash
# Final measurement campaign (host side). Runs every (solution, experiment)
# cell in its own bench container via scripts/in_docker.sh, in a randomised
# but reproducible order (experiments in sequence, solutions shuffled per
# experiment with a fixed seed) so slow drift of the host is not confounded
# with one solution. Resumable: a finished cell leaves
# results/raw/.campaign/<label>__<exp>.done and is skipped on re-run.
#
#   E2_RATE=... scripts/campaign.sh               # whole matrix
#   CELLS="s1-epoch-swap:E1 s4-reuseport+migrate:E4-noka" scripts/campaign.sh
#
# Labels: <cmd dir>[+variant]. Variants:
#   s4-reuseport+migrate : container started with --sysctl net.ipv4.tcp_migrate_req=1
#   s4-reuseport+crash, s5-fdpass+crash : E6 with a successor that crashes after the state handoff
#   s7-inplace+tampered  : E6 with a tampered (signed manifest, substituted module) bundle
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"; cd "$ROOT"
: "${E2_RATE:?set E2_RATE (common E2 rate, see results/calibration/)}"
: "${REPS_MAIN:=5}" "${REPS_E5:=2}" "${SHUFFLE_SEED:=20261108}"
DONE="$ROOT/results/raw/.campaign"; mkdir -p "$DONE"
LOG="$ROOT/results/raw/campaign.log"

BASE="b0-naive-restart s1-epoch-swap s2-plugin s3-wasm s4-reuseport s4-reuseport+migrate s5-fdpass s6-hotpatch s7-inplace"
default_cells() {
  local e s
  for e in E1 E2 E3 E4-ka E4-noka E6 E5; do
    local sols="$BASE"
    [[ $e == E5 ]] && sols="${BASE/s4-reuseport+migrate /}"            # migrate only matters for queued connections
    [[ $e == E6 ]] && sols="$sols s4-reuseport+crash s5-fdpass+crash s7-inplace+tampered"
    # shellcheck disable=SC2086
    for s in $(python3 -c 'import random,sys; l=sys.argv[2:]; random.Random(sys.argv[1]).shuffle(l); print(" ".join(l))' \
               "$SHUFFLE_SEED$e" $sols); do echo "$s:$e"; done
  done
}
CELLS="${CELLS:-$(default_cells)}"

run_cell() {
  local label=$1 exp=$2 sol=${1%%+*} var="" extra="" reps=$REPS_MAIN
  [[ $label == *+* ]] && var=${label#*+}
  [[ $exp == E5 ]] && reps=$REPS_E5
  case $var in
    migrate) extra="--sysctl net.ipv4.tcp_migrate_req=1"; var="" ;;
  esac
  local envs=(REPS="$reps" SOL_LABEL="$label" RUN_TAG=final SKIP_ANALYSIS=1 DOCKER_EXTRA_ARGS="$extra")
  [[ -n $var ]] && envs+=(BENCH_VARIANT="$var")
  [[ $exp == E2 ]] && envs+=(RATE="$E2_RATE")
  env "${envs[@]}" scripts/in_docker.sh scripts/run_experiment.sh "$sol" "$exp"
}

t_start=$(date +%s)
for cell in $CELLS; do
  label=${cell%%:*}; exp=${cell#*:}
  marker="$DONE/${label}__${exp}.done"
  [[ -f $marker ]] && { echo "[skip] $cell (done)"; continue; }
  t0=$(date +%s)
  echo "[$(date -u +%H:%M:%S)] >>> $cell" | tee -a "$LOG"
  if run_cell "$label" "$exp" >>"$LOG" 2>&1; then
    touch "$marker"
    echo "[$(date -u +%H:%M:%S)] <<< $cell ok ($(( $(date +%s) - t0 )) s, total $(( ($(date +%s) - t_start) / 60 )) min)" | tee -a "$LOG"
  else
    echo "[$(date -u +%H:%M:%S)] !!! $cell FAILED (see $LOG)" | tee -a "$LOG"
  fi
done
echo "campaign finished in $(( ($(date +%s) - t_start) / 60 )) min"
