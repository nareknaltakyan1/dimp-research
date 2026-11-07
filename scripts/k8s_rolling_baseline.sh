#!/usr/bin/env bash
# "Standard cloud-native" baseline: Kubernetes Deployment rolling update v1->v2
# of the contract mock server, under open-loop load from an in-cluster Job,
# measured with the same loadgen CSV + events format as scripts/run_experiment.sh.
# Runs on the HOST (needs docker); creates a kind cluster "dimp".
#
#   scripts/k8s_rolling_baseline.sh                 # E1-like, VARIANT=naive, REPS=3
#   VARIANT=prestop KEEPALIVE=0 REPS=5 scripts/k8s_rolling_baseline.sh
#   K8S_DELETE_CLUSTER=1 scripts/k8s_rolling_baseline.sh   # tear down afterwards
#
# VARIANT=naive   : readinessProbe only (common default).
# VARIANT=prestop : + preStop sleep 5s so endpoints are removed before SIGTERM
#                   (the documented best practice for graceful rolling updates).
# Results: results/raw/k8s-<variant>[-noka]/<run-id>/ (analyse with bench/analysis/analyze.py).
# Notes: RSS is not sampled (no procmon in pods); pid is 1 in every pod.
set -euo pipefail
export LC_ALL=C   # awk/printf decimals (host locale may use commas)
ROOT="$(cd "$(dirname "$0")/.." && pwd)"; cd "$ROOT"
: "${VARIANT:=naive}" "${REPS:=3}" "${RATE:=200}" "${WARMUP_S:=3}" "${UPGRADE_AT_S:=10}" "${TAIL_S:=40}"
: "${CONCURRENCY:=512}" "${WORK_MIX:=0:1}" "${KEEPALIVE:=1}" "${TIMEOUT:=5s}" "${SEED:=1}"
: "${CLUSTER:=dimp}" "${KIND_CPUSET:=0-5}"
EXP="${EXP:-E1}"
LABEL="k8s-${VARIANT}$( ((KEEPALIVE)) || echo -noka)"
NODE="${CLUSTER}-control-plane"
export PATH="$ROOT/bin:$PATH"

if ! command -v kind >/dev/null; then
  echo "[kind] installing into $ROOT/bin"; GOBIN="$ROOT/bin" go install sigs.k8s.io/kind@latest
fi
KUBECTL="kubectl --context kind-$CLUSTER"
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --wait 120s
fi
docker update --cpuset-cpus "$KIND_CPUSET" "$NODE" >/dev/null   # same CPU budget as the Docker runs
echo "[image] building dimp-mock"
docker build -q -f deploy/Dockerfile.mockserver -t dimp-mock:latest . >/dev/null
kind load docker-image dimp-mock:latest --name "$CLUSTER" >/dev/null

$KUBECTL apply -f deploy/k8s/mock.yaml >/dev/null
if [[ "$VARIANT" == prestop ]]; then
  $KUBECTL -n dimp-bench patch deploy mock --type=json -p \
    '[{"op":"add","path":"/spec/template/spec/containers/0/lifecycle","value":{"preStop":{"sleep":{"seconds":5}}}}]' >/dev/null
else
  $KUBECTL -n dimp-bench patch deploy mock --type=json -p \
    '[{"op":"remove","path":"/spec/template/spec/containers/0/lifecycle"}]' >/dev/null 2>&1 || true
fi
$KUBECTL -n dimp-bench set env deploy/mock VERSION=v1 >/dev/null
$KUBECTL -n dimp-bench rollout restart deploy/mock >/dev/null   # fresh pods (image may have changed)
$KUBECTL -n dimp-bench rollout status deploy/mock --timeout 180s

node_ns() { docker exec "$NODE" date +%s%N; }   # all timestamps on the node clock (= loadgen pod clock)
DURATION_S=$((UPGRADE_AT_S + TAIL_S))
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
GIT_SHA=$(git rev-parse HEAD 2>/dev/null || echo unknown)

for ((rep = 1; rep <= REPS; rep++)); do
  RUN_ID="${EXP}-${STAMP}-r${rep}"
  RUN="$ROOT/results/raw/$LABEL/$RUN_ID"; mkdir -p "$RUN"
  JOB_RUN=$(echo "$LABEL-$rep-$STAMP" | tr 'A-Z' 'a-z')
  echo "[run] $LABEL rep $rep/$REPS -> ${RUN#$ROOT/}"
  # reset to v1
  if [[ "$($KUBECTL -n dimp-bench get deploy mock -o jsonpath='{.spec.template.spec.containers[0].env[0].value}')" != v1 ]]; then
    $KUBECTL -n dimp-bench set env deploy/mock VERSION=v1 >/dev/null
    $KUBECTL -n dimp-bench rollout status deploy/mock --timeout 180s >/dev/null
  fi
  sleep 5   # let terminating pods disappear
  sed -e "s|__RUN__|$JOB_RUN|g" -e "s|__RATE__|$RATE|" -e "s|__DURATION__|$DURATION_S|" -e "s|__CONC__|$CONCURRENCY|" \
      -e "s|__MIX__|$WORK_MIX|" -e "s|__KA__|$( ((KEEPALIVE)) && echo true || echo false)|" -e "s|__TIMEOUT__|$TIMEOUT|" \
      -e "s|__SEED__|$((SEED + rep))|" deploy/k8s/loadgen-job.yaml.tmpl | $KUBECTL apply -f - >/dev/null
  for _ in $(seq 600); do docker exec "$NODE" test -s "/tmp/dimp-out/$JOB_RUN/t0" 2>/dev/null && break; sleep 0.1; done
  T0=$(docker exec "$NODE" cat "/tmp/dimp-out/$JOB_RUN/t0")
  # sleep until T0 + UPGRADE_AT on the node clock
  rem=$(( T0 + UPGRADE_AT_S * 1000000000 - $(node_ns) ))
  (( rem > 0 )) && sleep "$(awk -v d="$rem" 'BEGIN{printf "%.3f", d/1e9}')"
  TS=$(node_ns)
  $KUBECTL -n dimp-bench set env deploy/mock VERSION=v2 >/dev/null
  echo "idx,ts_ns,target,method,http_code" >"$RUN/triggers.csv"
  echo "0,$TS,v2,k8s-rolling-update,0" >>"$RUN/triggers.csv"
  $KUBECTL -n dimp-bench rollout status deploy/mock --timeout 180s >/dev/null
  DONE=$(node_ns)
  # rollout status returns when the new ReplicaSet is fully available; old pods may still be terminating.
  for _ in $(seq 600); do
    n=$($KUBECTL -n dimp-bench get pods -l app=mock --no-headers 2>/dev/null | wc -l | tr -d ' ')
    [[ "$n" == 3 ]] && break; sleep 0.2
  done
  DRAINED=$(node_ns)
  printf '{"ev":"upgrade_start","ts_ns":%s,"from":"v1","to":"v2","pid":0}\n{"ev":"upgrade_done","ts_ns":%s,"pid":0}\n{"ev":"old_version_drained","ts_ns":%s,"version":"v1","pid":0}\n' \
    "$TS" "$DONE" "$DRAINED" >"$RUN/events.jsonl"
  $KUBECTL -n dimp-bench wait --for=condition=complete "job/loadgen-$JOB_RUN" --timeout "$((DURATION_S + 120))s" >/dev/null
  $KUBECTL -n dimp-bench logs "job/loadgen-$JOB_RUN" >"$RUN/loadgen.log" 2>&1 || true
  # (docker cp cannot read the node's /tmp tmpfs, so stream it out)
  docker exec "$NODE" cat "/tmp/dimp-out/$JOB_RUN/loadgen.csv" >"$RUN/loadgen.csv"
  echo "$T0" >"$RUN/t0"
  tail -1 "$RUN/loadgen.log"
  $KUBECTL -n dimp-bench delete job "loadgen-$JOB_RUN" --wait=false >/dev/null
  docker exec "$NODE" rm -rf "/tmp/dimp-out/$JOB_RUN"
  K8S_VERSION=$($KUBECTL version -o json 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["serverVersion"]["gitVersion"])' || echo unknown)
  python3 - "$RUN/metadata.json" <<PY
import json, sys
m = dict(sol="$LABEL", build_sol="bench/mockserver (k8s Deployment)", exp="$EXP", rep=$rep, run_id="$RUN_ID",
         git_sha="$GIT_SHA", t0=$T0, rate=float($RATE), warmup_s=float($WARMUP_S), upgrade_at_s=float($UPGRADE_AT_S),
         upgrades=1, upgrade_interval_s=0.0, tail_s=float($TAIL_S), duration_s=float($DURATION_S),
         concurrency=$CONCURRENCY, work_mix="$WORK_MIX", keepalive=$KEEPALIVE, timeout="$TIMEOUT",
         upgrade_targets="v2", trigger="kubectl set env (rolling update)", expect_fail=0, reps=$REPS, seed=$((SEED + rep)),
         k8s=dict(variant="$VARIANT", replicas=3, max_surge=1, max_unavailable=0, readiness_period_s=1,
                  server_version="$K8S_VERSION", kind_node_cpuset="$KIND_CPUSET", cluster="$CLUSTER",
                  client="in-cluster Job via ClusterIP Service (kube-proxy)"),
         host=dict(docker_host_info="$(docker info --format '{{.OperatingSystem}}; {{.NCPU}} cpus; kernel {{.KernelVersion}}')"))
json.dump(m, open(sys.argv[1], "w"), indent=2)
PY
done
if [[ -n "${K8S_DELETE_CLUSTER:-}" ]]; then kind delete cluster --name "$CLUSTER"; fi
echo "[done] results/raw/$LABEL/"
