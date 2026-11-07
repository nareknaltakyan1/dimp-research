# shellcheck shell=bash
# Experiment matrix. Sourced by scripts/run_experiment.sh / run_matrix.sh.
# Every parameter can be overridden from the environment (env wins), e.g.
#   RATE=500 REPS=10 scripts/run_experiment.sh s1-epoch-swap E1
# See docs/evaluation-methodology.md for the rationale of each experiment.

ALL_EXPERIMENTS="E1 E2 E3 E4-ka E4-noka E5 E6"

# Defaults shared by all experiments.
exp_defaults() {
  : "${RATE:=200}"                 # req/s, open loop
  : "${WARMUP_S:=3}"               # excluded from steady-state statistics
  : "${UPGRADE_AT_S:=10}"          # first upgrade trigger, seconds after load start
  : "${UPGRADES:=1}"               # number of upgrade triggers
  : "${UPGRADE_INTERVAL_S:=2}"     # spacing of triggers when UPGRADES>1
  : "${TAIL_S:=10}"                # load continues this long after last trigger
  : "${CONCURRENCY:=512}"          # loadgen in-flight cap
  : "${WORK_MIX:=0:1}"             # work_ms:weight list
  : "${KEEPALIVE:=1}"
  : "${TIMEOUT:=5s}"
  : "${UPGRADE_TARGETS:=v2}"       # cycled over triggers
  : "${TRIGGER:=http}"             # http | sighup
  : "${REPS:=5}"
  : "${SEED:=1}"
}

load_experiment() {
  case "$1" in
    E1) # baseline correctness at moderate load
      : "${RATE:=200}" ;;
    E2) # high load near saturation (calibrate first: scripts/calibrate.sh <sol>)
      local cal="results/calibration/${SOL:-mock}.rate"
      if [[ -z "${RATE:-}" && -f "$cal" ]]; then RATE=$(cat "$cal"); fi
      : "${RATE:=4000}"; : "${CONCURRENCY:=2048}"; : "${TIMEOUT:=2s}" ;;
    E3) # long-request draining: 10% of requests take 500 ms
      : "${WORK_MIX:=0:0.9,500:0.1}"; : "${CONCURRENCY:=1024}" ;;
    E4-ka) # keep-alive connections across the handoff
      : "${RATE:=500}"; : "${WORK_MIX:=0:0.95,100:0.05}"; KEEPALIVE=1 ;;
    E4-noka) # a fresh TCP connection per request
      : "${RATE:=500}"; : "${WORK_MIX:=0:0.95,100:0.05}"; KEEPALIVE=0 ;;
    E5) # repeated upgrades: memory growth / leaks, cumulative errors
      : "${RATE:=100}"; : "${UPGRADE_AT_S:=5}"; : "${UPGRADES:=50}"
      : "${UPGRADE_INTERVAL_S:=2}"; : "${TAIL_S:=5}"; : "${UPGRADE_TARGETS:=v2 v1}"; : "${REPS:=3}" ;;
    E6) # failed upgrade must roll back with no client-visible impact.
      # The contract does not yet define how to induce a failure; solutions
      # declare it via FAIL_TARGET in cmd/<sol>/bench.env (default: v2-broken).
      : "${UPGRADE_TARGETS:=${FAIL_TARGET:-v2-broken}}"; EXPECT_FAIL=1 ;;
    adhoc) ;;
    *) echo "unknown experiment '$1' (known: $ALL_EXPERIMENTS adhoc)" >&2; return 1 ;;
  esac
  exp_defaults
}
