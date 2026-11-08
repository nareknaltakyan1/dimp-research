#!/usr/bin/env bash
# Run one experiment (N repetitions) against one solution. Linux only: run it
# inside the bench container via scripts/in_docker.sh.
#
#   scripts/run_experiment.sh <solution> [experiment]      (default E1)
#
# <solution> is a directory name under cmd/ (e.g. s1-epoch-swap), or "mock"
# (bench/mockserver; extra args via SERVER_ARGS, e.g. SERVER_ARGS="-mode restart").
#
# Build hook: if cmd/<sol>/build.sh exists it is run as
#   cmd/<sol>/build.sh <out_dir>
# with cwd = repo root and must leave an executable <out_dir>/server (plus any
# artifacts it needs, e.g. v2 plugins). Otherwise: go build -o <out_dir>/server ./cmd/<sol>.
# Optional cmd/<sol>/bench.env is sourced before the experiment defaults and may
# set SERVER_ARGS, TRIGGER, FAIL_TARGET, UPGRADE_TARGETS, ... It sees $EXP and
# $BENCH_VARIANT, and may define a shell function `bench_build <out_dir>` that
# replaces the build step (used to build per-generation binaries/plugins/bundles).
# UPGRADE_EXTRA_QUERY (printf template, %s = target) is appended to the HTTP
# trigger, e.g. 'bin=./server-%s' -> POST /admin/upgrade?to=v2&bin=./server-v2.
# The server runs with cwd = <out_dir>.
set -euo pipefail
export LC_ALL=C   # awk/printf decimals (host locale may use commas)
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
SOL="${1:?usage: run_experiment.sh <solution> [experiment]}"
EXP="${2:-E1}"
[[ "$(uname -s)" == Linux ]] || { echo "must run on Linux (use scripts/in_docker.sh)" >&2; exit 1; }

if [[ -f "cmd/$SOL/bench.env" ]]; then set -a; source "cmd/$SOL/bench.env"; set +a; fi
source scripts/experiments.sh
load_experiment "$EXP"
: "${PORT:=18080}"
: "${SERVER_ARGS:=}"
: "${SERVER_CPUS:=0-1}"
: "${LOADGEN_CPUS:=2-5}"
: "${RSS_INTERVAL:=100ms}"
: "${EXPECT_FAIL:=0}"
: "${RUN_TAG:=}"
: "${UPGRADE_EXTRA_QUERY:=}"
: "${BENCH_VARIANT:=}"
LABEL="${SOL_LABEL:-$SOL}"   # results/raw/<LABEL>/..., lets one solution run under several configs
DURATION_S=$(awk -v a="$UPGRADE_AT_S" -v n="$UPGRADES" -v i="$UPGRADE_INTERVAL_S" -v t="$TAIL_S" 'BEGIN{print a+(n-1)*i+t}')

# ---------- build ----------
BUILD_DIR="$ROOT/bin/bench/$SOL"
rm -rf "$BUILD_DIR"; mkdir -p "$BUILD_DIR"
echo "[build] $SOL"
if declare -F bench_build >/dev/null; then
  bench_build "$BUILD_DIR"
elif [[ "$SOL" == mock ]]; then
  go build -o "$BUILD_DIR/server" ./bench/mockserver
elif [[ -x "cmd/$SOL/build.sh" ]]; then
  "cmd/$SOL/build.sh" "$BUILD_DIR"
elif [[ -d "cmd/$SOL" ]]; then
  go build -o "$BUILD_DIR/server" "./cmd/$SOL"
else
  echo "no such solution: cmd/$SOL" >&2; exit 1
fi
[[ -x "$BUILD_DIR/server" ]] || { echo "build did not produce $BUILD_DIR/server" >&2; exit 1; }
TOOLS="$ROOT/bin/bench/tools"; mkdir -p "$TOOLS"
go build -o "$TOOLS/loadgen" ./bench/loadgen
go build -o "$TOOLS/procmon" ./bench/procmon

now_ns() { date +%s%N; }
sleep_until_ns() { local d; d=$(( $1 - $(now_ns) )); (( d > 0 )) && sleep "$(awk -v d="$d" 'BEGIN{printf "%.6f", d/1e9}')"; return 0; }
# run "$@" pinned to CPU list $1; exec so that $! is the real PID (use only with &)
pin_exec() { local c="$1"; shift; if command -v taskset >/dev/null && [[ -n "$c" ]]; then exec taskset -c "$c" "$@"; else exec "$@"; fi; }

GIT_SHA=$(git rev-parse HEAD 2>/dev/null || echo unknown)
GIT_DIRTY=$( [[ -n "$(git status --porcelain 2>/dev/null)" ]] && echo true || echo false)
STAMP=$(date -u +%Y%m%dT%H%M%SZ)

for ((rep = 1; rep <= REPS; rep++)); do
  RUN_ID="${EXP}${RUN_TAG:+-$RUN_TAG}-${STAMP}-r${rep}"
  RUN="$ROOT/results/raw/$LABEL/$RUN_ID"
  mkdir -p "$RUN"
  echo "[run] $LABEL $EXP rep $rep/$REPS -> ${RUN#$ROOT/}"
  # make sure the port is free (previous run's process could linger)
  for _ in $(seq 50); do (exec 3<>/dev/tcp/127.0.0.1/$PORT) 2>/dev/null || break; sleep 0.1; done

  # ---------- start server in its own session ----------
  # shellcheck disable=SC2086
  (cd "$BUILD_DIR" && exec setsid taskset -c "$SERVER_CPUS" ./server -addr "127.0.0.1:$PORT" $SERVER_ARGS \
      >"$RUN/server.stdout" 2>"$RUN/server.stderr") &
  SPID=$!
  ok=0
  for _ in $(seq 300); do
    if curl -fsS -o /dev/null "http://127.0.0.1:$PORT/healthz" 2>/dev/null; then ok=1; break; fi
    kill -0 "$SPID" 2>/dev/null || break
    sleep 0.1
  done
  if (( ! ok )); then echo "server failed to become healthy; stderr:" >&2; cat "$RUN/server.stderr" >&2; kill -9 -- -"$SPID" 2>/dev/null || true; exit 1; fi
  SERVER_START_NS=$(now_ns)

  pin_exec "$LOADGEN_CPUS" "$TOOLS/procmon" -sid "$SPID" -interval "$RSS_INTERVAL" -out "$RUN/rss.csv" &
  MPID=$!

  rm -f "$RUN/t0"
  pin_exec "$LOADGEN_CPUS" "$TOOLS/loadgen" -url "http://127.0.0.1:$PORT/api" -rate "$RATE" -duration "${DURATION_S}s" \
      -concurrency "$CONCURRENCY" -work-mix "$WORK_MIX" -keepalive="$( ((KEEPALIVE)) && echo true || echo false)" \
      -timeout "$TIMEOUT" -seed "$((SEED + rep))" -out "$RUN/loadgen.csv" -start-file "$RUN/t0" 2>"$RUN/loadgen.log" &
  LPID=$!
  for _ in $(seq 100); do [[ -s "$RUN/t0" ]] && break; sleep 0.02; done
  T0=$(cat "$RUN/t0")

  # ---------- upgrade triggers (absolute schedule, no drift) ----------
  echo "idx,ts_ns,target,method,http_code" >"$RUN/triggers.csv"
  read -r -a TARGETS <<<"$UPGRADE_TARGETS"
  for ((u = 0; u < UPGRADES; u++)); do
    at=$(awk -v t0="$T0" -v a="$UPGRADE_AT_S" -v u="$u" -v i="$UPGRADE_INTERVAL_S" 'BEGIN{printf "%d", t0 + (a + u*i)*1e9}')
    sleep_until_ns "$at"
    tgt="${TARGETS[$((u % ${#TARGETS[@]}))]}"
    ts=$(now_ns)
    if [[ "$TRIGGER" == sighup ]]; then
      kill -HUP "$SPID" && code=sig || code=sigfail
    else
      extra=""; [[ -n "$UPGRADE_EXTRA_QUERY" ]] && extra="&$(printf "$UPGRADE_EXTRA_QUERY" "$tgt")"
      code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST "http://127.0.0.1:$PORT/admin/upgrade?to=$tgt$extra" || true)
    fi
    echo "$u,$ts,$tgt,$TRIGGER,$code" >>"$RUN/triggers.csv"
  done

  wait "$LPID" || echo "loadgen exited non-zero" >&2
  END_NS=$(now_ns)
  kill -TERM -- -"$SPID" 2>/dev/null || true
  for _ in $(seq 50); do kill -0 -- -"$SPID" 2>/dev/null || break; sleep 0.1; done
  kill -9 -- -"$SPID" 2>/dev/null || true
  # also stop processes that left the process group but kept the session
  for p in $(ps -eo pid=,sid= | awk -v s="$SPID" '$2==s{print $1}'); do kill -9 "$p" 2>/dev/null || true; done
  kill -TERM "$MPID" 2>/dev/null || true; wait "$MPID" 2>/dev/null || true
  wait "$SPID" 2>/dev/null || true
  grep -h '^{' "$RUN/server.stderr" | grep '"ev"' >"$RUN/events.jsonl" || true
  tail -1 "$RUN/loadgen.log"

  # ---------- metadata ----------
  BUILD_SOL="$SOL" SOL="$LABEL" EXP="$EXP" REP="$rep" RUN_ID="$RUN_ID" GIT_SHA="$GIT_SHA" GIT_DIRTY="$GIT_DIRTY" T0="$T0" \
  SERVER_START_NS="$SERVER_START_NS" END_NS="$END_NS" DURATION_S="$DURATION_S" \
  RATE="$RATE" WARMUP_S="$WARMUP_S" UPGRADE_AT_S="$UPGRADE_AT_S" UPGRADES="$UPGRADES" \
  UPGRADE_INTERVAL_S="$UPGRADE_INTERVAL_S" TAIL_S="$TAIL_S" CONCURRENCY="$CONCURRENCY" WORK_MIX="$WORK_MIX" \
  KEEPALIVE="$KEEPALIVE" TIMEOUT="$TIMEOUT" UPGRADE_TARGETS="$UPGRADE_TARGETS" TRIGGER="$TRIGGER" \
  SERVER_ARGS="$SERVER_ARGS" SERVER_CPUS="$SERVER_CPUS" LOADGEN_CPUS="$LOADGEN_CPUS" EXPECT_FAIL="$EXPECT_FAIL" \
  REPS="$REPS" SEED="$((SEED + rep))" RSS_INTERVAL="$RSS_INTERVAL" \
  UPGRADE_EXTRA_QUERY="$UPGRADE_EXTRA_QUERY" BENCH_VARIANT="$BENCH_VARIANT" \
  python3 - "$RUN/metadata.json" <<'PY'
import json, os, platform, subprocess, sys
e = os.environ
def sh(c):
    try: return subprocess.run(c, shell=True, capture_output=True, text=True).stdout.strip()
    except Exception: return ""
def rd(p):
    try: return open(p).read().strip()
    except Exception: return None
keys = ["SOL","BUILD_SOL","EXP","REP","RUN_ID","GIT_SHA","T0","SERVER_START_NS","END_NS","DURATION_S","RATE","WARMUP_S",
        "UPGRADE_AT_S","UPGRADES","UPGRADE_INTERVAL_S","TAIL_S","CONCURRENCY","WORK_MIX","KEEPALIVE","TIMEOUT",
        "UPGRADE_TARGETS","TRIGGER","SERVER_ARGS","SERVER_CPUS","LOADGEN_CPUS","EXPECT_FAIL","REPS","SEED","RSS_INTERVAL",
        "UPGRADE_EXTRA_QUERY","BENCH_VARIANT"]
m = {k.lower(): e.get(k) for k in keys}
for k in ["t0","server_start_ns","end_ns","rep","upgrades","concurrency","keepalive","expect_fail","reps","seed"]:
    m[k] = int(m[k])
for k in ["rate","warmup_s","upgrade_at_s","upgrade_interval_s","tail_s","duration_s"]:
    m[k] = float(m[k])
m["git_dirty"] = e.get("GIT_DIRTY") == "true"
m["host"] = {
    "hostname": platform.node(), "kernel": platform.release(), "machine": platform.machine(),
    "os": rd("/etc/os-release").splitlines()[0] if rd("/etc/os-release") else None,
    "go_version": sh("go version"), "python": platform.python_version(),
    "nproc_visible": int(sh("nproc") or 0), "cpus_allowed": sh("grep Cpus_allowed_list /proc/self/status | cut -f2"),
    "cgroup_cpu_max": rd("/sys/fs/cgroup/cpu.max"), "cgroup_mem_max": rd("/sys/fs/cgroup/memory.max"),
    "docker_cpus": e.get("DOCKER_CPUS"), "docker_cpuset": e.get("DOCKER_CPUSET"), "docker_memory": e.get("DOCKER_MEMORY"),
    "docker_host_info": e.get("DOCKER_HOST_INFO"),
    "docker_other_containers_at_start": e.get("DOCKER_OTHER_CONTAINERS"),
    "loadavg_at_end": rd("/proc/loadavg"),
    "cpu_model": sh("grep -m1 -i 'model name' /proc/cpuinfo | cut -d: -f2") or sh("lscpu | grep -m1 'Model name' | cut -d: -f2"),
    "sysctl": {k: rd("/proc/sys/" + k.replace(".", "/")) for k in
               ["net.ipv4.ip_local_port_range","net.ipv4.tcp_tw_reuse","net.core.somaxconn",
                "net.ipv4.tcp_migrate_req"]},
    "docker_extra_args": e.get("DOCKER_EXTRA_ARGS"),
}
json.dump(m, open(sys.argv[1], "w"), indent=2)
PY
  sleep 1
done
echo "[done] results in results/raw/$LABEL/"
