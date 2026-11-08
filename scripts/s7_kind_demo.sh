#!/usr/bin/env bash
# S7 end-to-end demo on a kind cluster named "dimp-s7":
#   3 replicas of s7-inplace v1 under continuous in-cluster load, then
#   1. HotPatch v2 (good)      -> in-place, zero failed requests, no pod restarts
#   2. HotPatch v3 (buggy)     -> caught in shadow, fleet rolled back, zero client errors
#   3. HotPatch v4 (tampered)  -> rejected by every pod's signature check
#   4. spec drift              -> a deleted pod comes back with image code v1,
#                                 is held NotReady by the readiness gate and re-patched
# Output: results/s7-demo/.  Only deletes the cluster if it created it AND KEEP=0.
#   scripts/s7_kind_demo.sh            # KEEP=1 by default (cluster kept)
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$(pwd)
CLUSTER=dimp-s7
OUT=${OUT:-$ROOT/results/s7-demo}
KEEP=${KEEP:-1}
KIND=${KIND:-$(command -v kind || echo "$HOME/go/bin/kind")}
WORK=$(mktemp -d /tmp/dimp-s7-demo.XXXX)
mkdir -p "$OUT"
rm -f "$OUT"/*.log "$OUT"/*.json "$OUT"/*.yaml "$OUT"/*.txt 2>/dev/null || true
exec > >(tee "$OUT/demo.log") 2>&1
K="kubectl --context kind-$CLUSTER"
NS=dimp-s7
ts() { date +%H:%M:%S.%N | cut -c1-12; }
say() { echo; echo "=== [$(ts)] $*"; }
now_ms() { python3 -c 'import time; print(int(time.time()*1000))'; }

say "kind cluster $CLUSTER"
CREATED=0
if ! "$KIND" get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  "$KIND" create cluster --name "$CLUSTER" --wait 120s
  CREATED=1
fi
$K get nodes -o wide

say "build images"
docker build -q -f deploy/s7/Dockerfile -t dimp/s7-inplace:dev .
docker build -q -f deploy/s7/Dockerfile.operator -t dimp/hotpatch-operator:dev operator/
"$KIND" load docker-image --name "$CLUSTER" dimp/s7-inplace:dev dimp/hotpatch-operator:dev

say "build guest modules + release signing key + signed bundles (local)"
OUT_BIN=$WORK/bin
OUT=$OUT_BIN cmd/s7-inplace/build.sh >/dev/null
PC=$OUT_BIN/s7-patchctl
$PC keygen -out "$WORK/release"
cp "$WORK/release.pub" "$OUT/release.pub"
$PC sign -key "$WORK/release.key" -module "$OUT_BIN/modules/v2.wasm" -version v2 -base v1 -out "$WORK/b/v2"
$PC sign -key "$WORK/release.key" -module "$OUT_BIN/modules/v3.wasm" -version v3 -base v2 -out "$WORK/b/v3"
$PC sign -key "$WORK/release.key" -module "$OUT_BIN/modules/v2.wasm" -version v4 -base v2 -out "$WORK/b/v4"
$PC tamper -bundle "$WORK/b/v4" -module "$OUT_BIN/modules/v3.wasm" -out "$WORK/b/v4-tampered"
$PC verify -keys "$WORK/release.pub" -bundle "$WORK/b/v2"
$PC verify -keys "$WORK/release.pub" -bundle "$WORK/b/v4-tampered" || echo "(local verify of tampered bundle failed as expected)"

say "install CRD, RBAC, secrets, operator, app"
$K apply -f deploy/s7/crd.yaml -f deploy/s7/rbac.yaml
$K create namespace $NS --dry-run=client -o yaml | $K apply -f -
TOKEN=$(head -c 24 /dev/urandom | base64 | tr -d '/+=')
for n in dimp-system $NS; do
  $K -n $n create secret generic dimp-admin-token --from-literal=token="$TOKEN" --dry-run=client -o yaml | $K apply -f -
done
$K -n $NS create configmap dimp-trusted-keys --from-file=release.pub="$WORK/release.pub" --dry-run=client -o yaml | $K apply -f -
$K apply -f deploy/s7/operator.yaml
$K -n dimp-system rollout restart deploy/hotpatch-operator >/dev/null 2>&1 || true
$K -n dimp-system rollout status deploy/hotpatch-operator --timeout=180s
# Clean previous demo state (idempotent re-runs).
$K -n $NS delete hotpatch --all --ignore-not-found
$K -n $NS delete pod s7-load --ignore-not-found --wait=true
$K -n $NS delete deploy s7-inplace --ignore-not-found --wait=true
$K -n $NS delete configmap patch-v2 patch-v3 patch-v4-tampered --ignore-not-found
$K -n $NS delete events --all >/dev/null 2>&1 || true
$K apply -f deploy/s7/app.yaml
$K -n $NS rollout status deploy/s7-inplace --timeout=180s
$K -n $NS get pods -o wide

podtable() { $K -n $NS get pods -l app=s7-inplace -o custom-columns='NAME:.metadata.name,UID:.metadata.uid,RESTARTS:.status.containerStatuses[0].restartCount,STARTED:.status.startTime,PATCH:.metadata.annotations.dimp\.research\.io/patch-version,READY:.status.conditions[?(@.type=="Ready")].status,GATE:.status.conditions[?(@.type=="dimp.research.io/patched")].status' --no-headers | sort; }
say "pods BEFORE (v1 from image)"
podtable | tee "$OUT/pods-before.txt"

say "start in-cluster load (150 rps, verifies results + per-pod counter continuity)"
$K apply -f deploy/s7/load.yaml
$K -n $NS wait --for=condition=Ready pod/s7-load --timeout=120s
sleep 10
api() { $K -n $NS exec s7-load -- wget -qO- "http://s7-inplace:8080/api?x=$1" ; }
api 13; api 13

for v in v2 v3 v4-tampered; do
  $K -n $NS create configmap patch-$v --from-file=manifest.json="$WORK/b/$v/manifest.json" \
    --from-file=manifest.sig="$WORK/b/$v/manifest.sig" --from-file=module.wasm.gz="$WORK/b/$v/module.wasm.gz"
done
$K -n $NS get configmap patch-v2 -o jsonpath='{.metadata.name}: binaryData keys={.binaryData}' | cut -c1-80; echo

wait_hp() { # name -> waits for a terminal phase, prints elapsed
  local name=$1 t0 ph
  t0=$(now_ms)
  for _ in $(seq 1 600); do
    ph=$($K -n $NS get hotpatch "$name" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    case "$ph" in Succeeded|RolledBack|Failed) break;; esac
    sleep 0.5
  done
  echo "HotPatch $name -> ${ph:-<none>} after $(( $(now_ms) - t0 )) ms (wall clock, from apply)"
}
show_hp() {
  local name=$1
  $K -n $NS get hotpatch "$name" -o wide
  $K -n $NS get hotpatch "$name" -o yaml > "$OUT/hotpatch-$name.yaml"
  $K -n $NS get hotpatch "$name" -o json | python3 -c '
import json,sys
h=json.load(sys.stdin); s=h["status"]
print("phase:", s.get("phase"), "| message:", s.get("message"))
print("started:", s.get("startedAt"), "completed:", s.get("completedAt"))
for p in s.get("pods",[]):
    m=p.get("metrics") or {}
    print("  %-34s %-9s %s->%s stage=%s samples=%s mismatches=%s candP99=%sms stableP99=%sms decision=%sms verify=%sms compile=%sms | %s" % (
      p["name"], p["phase"], p.get("fromVersion",""), p.get("version",""), m.get("stage"), m.get("candidateSamples"),
      m.get("mismatches"), m.get("candidateP99Ms"), m.get("stableP99Ms"), m.get("decisionMs"), m.get("verifyMs"), m.get("compileMs"), (p.get("message") or "")[:140]))'
}

say "1) apply HotPatch v2 (good patch)"
$K apply -f deploy/s7/hotpatch-v2.yaml
wait_hp s7-v2 | tee "$OUT/timing-v2.txt"
show_hp s7-v2
podtable | tee "$OUT/pods-after-v2.txt"
api 13; api 13

say "2) apply HotPatch v3 (buggy: wrong result for x%10==3, +4ms latency)"
$K apply -f deploy/s7/hotpatch-v3.yaml
wait_hp s7-v3 | tee "$OUT/timing-v3.txt"
show_hp s7-v3
podtable | tee "$OUT/pods-after-v3.txt"
api 13

say "3) apply HotPatch v4 (TAMPERED: signed manifest, substituted module)"
$K apply -f deploy/s7/hotpatch-tampered.yaml
wait_hp s7-v4-tampered | tee "$OUT/timing-tampered.txt"
show_hp s7-v4-tampered
podtable | tee "$OUT/pods-after-tampered.txt"

say "NetworkPolicy check: load pod -> pod admin port 9090 (should be blocked if the CNI enforces policies)"
P1=$($K -n $NS get pods -l app=s7-inplace -o jsonpath='{.items[0].status.podIP}')
if $K -n $NS exec s7-load -- wget -qO- -T 3 "http://$P1:9090/admin/status" >/dev/null 2>&1; then
  echo "admin port REACHABLE from a non-operator pod (CNI does not enforce NetworkPolicy); bearer token still required:"
  $K -n $NS exec s7-load -- wget -qO- -T 3 "http://$P1:9090/admin/rollout" 2>&1 | head -2 || true
else
  echo "admin port BLOCKED for non-operator pods (NetworkPolicy enforced)"
fi

say "stop load, collect summary (covers v2, v3, tampered phases)"
$K -n $NS exec s7-load -- /app/s7-load -stop http://localhost:8081 > "$OUT/load-summary.json"
python3 - "$OUT/load-summary.json" <<'EOF'
import json,sys
d=json.load(open(sys.argv[1]))
tl=d.pop("timeline")
print(json.dumps(d, indent=1))
print("per-10s version mix:")
for i in range(0,len(tl),10):
    agg={}; err=0; wrong=0
    for s in tl[i:i+10]:
        err+=s["err"]; wrong+=s["wrong"]
        for k,v in s["v"].items(): agg[k]=agg.get(k,0)+v
    print("  t=%3ds-%3ds versions=%s err=%d wrong=%d" % (i, i+10, agg, err, wrong))
EOF
$K -n $NS logs s7-load > "$OUT/load.log" || true
$K -n $NS delete pod s7-load --wait=false

say "4) spec drift: delete one pod; the replacement runs IMAGE code v1"
VICTIM=$($K -n $NS get pods -l app=s7-inplace -o jsonpath='{.items[0].metadata.name}')
T0=$(now_ms)
$K -n $NS delete pod "$VICTIM" --wait=false
NEWPOD=""
for _ in $(seq 1 240); do
  NEWPOD=$($K -n $NS get pods -l app=s7-inplace --no-headers -o custom-columns=N:.metadata.name,T:.metadata.deletionTimestamp | awk '$2=="<none>"{print $1}' | grep -vxF -f <(cut -d' ' -f1 "$OUT/pods-after-tampered.txt") || true)
  [ -n "$NEWPOD" ] && break; sleep 0.25
done
echo "replacement pod: $NEWPOD"
GATE_FALSE_SEEN=0
for _ in $(seq 1 240); do
  g=$($K -n $NS get pod "$NEWPOD" -o jsonpath='{.status.conditions[?(@.type=="dimp.research.io/patched")].status}' 2>/dev/null || true)
  a=$($K -n $NS get pod "$NEWPOD" -o jsonpath='{.metadata.annotations.dimp\.research\.io/patch-version}' 2>/dev/null || true)
  r=$($K -n $NS get pod "$NEWPOD" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
  [ "$g" = "False" ] && GATE_FALSE_SEEN=1
  echo "  t+$(( $(now_ms) - T0 ))ms gate=${g:-?} patch-version=${a:-<none>} ready=${r:-?}"
  [ "$r" = "True" ] && [ -n "$a" ] && break
  sleep 0.5
done | tee "$OUT/spec-drift.txt" | tail -25
$K -n $NS get pod "$NEWPOD" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason} {.message}{"\n"}{end}' | tee -a "$OUT/spec-drift.txt"
$K -n $NS get hotpatch s7-v2 -o json | python3 -c 'import json,sys; [print(" ", p["name"], p["phase"], p.get("fromVersion"), "->", p.get("version"), p.get("message")) for p in json.load(sys.stdin)["status"]["pods"]]' | tee -a "$OUT/spec-drift.txt"
podtable | tee "$OUT/pods-final.txt"

say "events, operator log, pod event logs"
$K -n $NS get events --sort-by=.lastTimestamp -o custom-columns='TIME:.lastTimestamp,OBJ:.involvedObject.name,REASON:.reason,MSG:.message' | grep -E 'hotpatch|HotPatch|s7-v' > "$OUT/k8s-events.txt" || true
grep -E 'Promoted|Rollback|RolledBack|Rejected|Succeeded|Repatched|AnalysisFailed' "$OUT/k8s-events.txt" | head -40 || true
$K -n dimp-system logs deploy/hotpatch-operator > "$OUT/operator.log" 2>&1 || true
for p in $($K -n $NS get pods -l app=s7-inplace -o name); do
  $K -n $NS logs "$p" > "$OUT/$(basename "$p").events.log" 2>&1 || true
done
grep -h -E '"ev":"(upgrade_start|rollout_rollback|new_version_serving|upgrade_done|upgrade_failed|patch_rejected|shadow_mismatch)"' "$OUT"/*.events.log | cut -c1-300 | head -40

say "done (cluster $CLUSTER kept=$KEEP, created_by_this_run=$CREATED); results in $OUT"
if [ "$KEEP" = 0 ] && [ "$CREATED" = 1 ]; then "$KIND" delete cluster --name "$CLUSTER"; fi
rm -rf "$WORK"
