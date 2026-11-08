#!/usr/bin/env bash
# End-to-end smoke test for S6 (run inside Docker, see build.sh):
#   docker run --rm -v "$PWD":/src -w /src golang:1.25 cmd/s6-hotpatch/smoke.sh
# Runs the scripted upgrade/rollback/failure sequence under load twice:
#   1. -consistency=request (the design): expects 0 mixed requests
#   2. -consistency=call    (ablation):   mixed requests are expected
set -euo pipefail
cd "$(dirname "$0")/../.."
OUT=${OUT:-/tmp/s6}
OUT="$OUT" cmd/s6-hotpatch/build.sh >/dev/null
go build -o "$OUT/smoke" ./cmd/s6-hotpatch/smoke
PORT=${PORT:-18086}

run() {
  local mode=$1
  local log="$OUT/server-$mode.log"
  "$OUT/s6-hotpatch" -addr "127.0.0.1:$PORT" -consistency "$mode" 2>"$log" &
  local pid=$!
  for _ in $(seq 50); do curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
  echo "=== consistency=$mode (pid $pid) ==="
  set +e
  "$OUT/smoke" -url "http://127.0.0.1:$PORT" "${@:2}"
  local rc=$?
  set -e
  echo "--- SIGHUP (v2 applied -> rollback)"
  kill -HUP "$pid"; sleep 0.5
  curl -s "http://127.0.0.1:$PORT/admin/status" | head -c 300; echo
  kill "$pid"; wait "$pid" 2>/dev/null || true
  echo "--- event log ($log)"
  grep -o '"ev":"[a-z_]*"' "$log" | sort | uniq -c
  grep '"upgrade_failed"' "$log" | sed 's/"load":{[^}]*}//' | cut -c1-260
  grep '"new_version_serving"' "$log" | head -2
  grep '"old_version_drained"' "$log" | cut -c1-260 | head -3
  cp "$log" "${SMOKE_LOGDIR:-$OUT}/" 2>/dev/null || true
  echo "smoke exit code: $rc"
  return 0
}
run request
run call
