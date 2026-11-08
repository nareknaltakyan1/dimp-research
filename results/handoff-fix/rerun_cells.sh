#!/usr/bin/env bash
# Re-run cells whose first fix runs were invalidated by host sleep (wall duration >> 20 s).
# Same parameters as scripts/campaign.sh. Run as: caffeinate -dimsu results/handoff-fix/rerun_cells.sh
set -uo pipefail
cd "$(dirname "$0")/../.."
LOG=results/handoff-fix/runs.log
cell() {
  local label=$1 exp=$2 sol=${1%%+*} var="" extra=""
  [[ $label == *+* ]] && var=${label#*+}
  case $var in migrate) extra="--sysctl net.ipv4.tcp_migrate_req=1"; var="" ;; esac
  local envs=(REPS=5 SOL_LABEL="fix-$label" RUN_TAG=fix2 SKIP_ANALYSIS=1 DOCKER_EXTRA_ARGS="$extra")
  [[ -n $var ]] && envs+=(BENCH_VARIANT="$var")
  [[ $exp == E2 ]] && envs+=(RATE=21000)
  echo "[$(date -u +%H:%M:%S)] >>> RERUN $label $exp (containers running: $(docker ps -q | wc -l | tr -d ' '))" | tee -a $LOG
  env "${envs[@]}" scripts/in_docker.sh scripts/run_experiment.sh "$sol" "$exp" >>$LOG 2>&1 || echo "!!! $label $exp failed" | tee -a $LOG
}
for c in ${CELLS:-s4-reuseport+migrate:E2 s5-fdpass:E2 s4-reuseport:E6 s4-reuseport+migrate:E6 s5-fdpass:E6 s4-reuseport+crash:E6 s5-fdpass+crash:E6}; do
  cell "${c%%:*}" "${c#*:}"
done
for d in results/raw/fix-*; do
  mkdir -p results/handoff-fix/raw/$(basename "$d")
  mv "$d"/* results/handoff-fix/raw/$(basename "$d")/ && rmdir "$d"
done
echo "RERUN DONE" | tee -a $LOG
