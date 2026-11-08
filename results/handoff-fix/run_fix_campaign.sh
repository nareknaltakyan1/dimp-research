#!/usr/bin/env bash
# Re-run S4/S5 cells after the handoff fix, same parameters as scripts/campaign.sh.
set -uo pipefail
cd /Users/nareknaltakyan/research/dimp-research
LOG=results/handoff-fix/runs.log
cell() { # label exp
  local label=$1 exp=$2 sol=${1%%+*} var="" extra=""
  [[ $label == *+* ]] && var=${label#*+}
  case $var in migrate) extra="--sysctl net.ipv4.tcp_migrate_req=1"; var="" ;; esac
  local envs=(REPS=5 SOL_LABEL="fix-$label" RUN_TAG=fix SKIP_ANALYSIS=1 DOCKER_EXTRA_ARGS="$extra")
  [[ -n $var ]] && envs+=(BENCH_VARIANT="$var")
  [[ $exp == E2 ]] && envs+=(RATE=21000)
  echo "[$(date -u +%H:%M:%S)] >>> $label $exp (containers running: $(docker ps -q | wc -l | tr -d ' '))" | tee -a $LOG
  env "${envs[@]}" scripts/in_docker.sh scripts/run_experiment.sh "$sol" "$exp" >>$LOG 2>&1 || echo "!!! $label $exp failed" | tee -a $LOG
}
for exp in E1 E4-ka E2 E6; do
  for l in s4-reuseport s4-reuseport+migrate s5-fdpass; do cell $l $exp; done
done
cell s4-reuseport+crash E6
cell s5-fdpass+crash E6
mkdir -p results/handoff-fix/raw
for d in results/raw/fix-*; do mv "$d" results/handoff-fix/raw/; done
echo "ALL DONE" | tee -a $LOG
