#!/usr/bin/env bash
# End-to-end smoke tests for B0/S4/S5. Run INSIDE a Linux container, e.g.:
#   docker run --rm -v $PWD:/src -w /src golang:1.25 bash internal/handoff/smoke/smoke.sh
#   docker run --rm --sysctl net.ipv4.tcp_migrate_req=1 -v $PWD:/src -w /src golang:1.25 \
#       bash internal/handoff/smoke/smoke.sh s4-stall
# Optional args: scenario names to run (default: all except none).
set -u
B=/tmp/dimpbin; mkdir -p $B
C=${C:-32}; D=${D:-6s}; AT=${AT:-2s}; WORK=${WORK:-5}
build() { go build -o "$B/$1" -ldflags "-X main.version=$2 ${3:-}" "./cmd/$4" || exit 1; }
for s in b0-naive-restart s4-reuseport s5-fdpass; do
  short=${s%%-*}
  build $short-v1 v1 "" $s; build $short-v2 v2 "" $s
done
build s5-v2hang v2 "-X main.simulateFail=hang" s5-fdpass
build s5-v2crash v2 "-X main.simulateFail=crash-after-state" s5-fdpass
build s4-v2crash v2 "-X main.simulateFail=crash-after-state" s4-reuseport
go build -o $B/hammer ./internal/handoff/smoke/hammer || exit 1
echo "kernel: $(uname -r)  tcp_migrate_req=$(cat /proc/sys/net/ipv4/tcp_migrate_req 2>/dev/null || echo n/a)  somaxconn=$(cat /proc/sys/net/core/somaxconn)"

waitready() { for i in $(seq 1 200); do curl -sf -o /dev/null http://127.0.0.1:8080/healthz && return 0; sleep 0.05; done; echo "server not ready"; return 1; }
cleanup() { pkill -f "$B/" 2>/dev/null; sleep 0.5; pkill -9 -f "$B/" 2>/dev/null; rm -f /tmp/dimp-*.sock /tmp/dimp-*.pid /tmp/b0.state; sleep 0.3; }

# run NAME TRIGGER CMD...   (TRIGGER = http | sighup | none)
run() {
  name=$1; trig=$2; shift 2
  echo; echo "=================== $name ==================="
  echo "+ $*"
  cleanup
  "$@" 2>/tmp/ev-$name.log &
  spid=$!
  waitready || { cat /tmp/ev-$name.log; return; }
  echo "before: $(curl -s http://127.0.0.1:8080/admin/status)"
  t=http; [ "$trig" = sighup ] && t="sighup:$spid"
  at=$AT; [ "$trig" = none ] && at=0
  $B/hammer -c $C -d $D -upgrade-at $at -trigger $t -work-ms $WORK ${HAMMER_ARGS:-}
  sleep 1
  echo "after:  $(curl -s -m 2 http://127.0.0.1:8080/admin/status || echo DOWN)"
  echo "events:"; grep '"ev"' /tmp/ev-$name.log | sed 's/^/  /'
  grep -v '"ev"' /tmp/ev-$name.log | head -5 | sed 's/^/  [stderr] /'
}

all="b0-graceful b0-hard-state s4 s4-stall s4-rollback s5 s5-stall s5-sighup s5-rollback-hang s5-rollback-crash s5-twice s5-supervised"
for sc in ${@:-$all}; do case $sc in
  b0-graceful)      run $sc http  $B/b0-v1 -mode=graceful -next-binary=$B/b0-v2 ;;
  b0-hard-state)    run $sc http  $B/b0-v1 -mode=hard -state-file=/tmp/b0.state -next-binary=$B/b0-v2 ;;
  s4)               run $sc http  $B/s4-v1 -next-binary=$B/s4-v2 ;;
  s4-stall)         run $sc http  $B/s4-v1 -next-binary=$B/s4-v2 -stall-accept=300ms ;;
  s4-rollback)      run $sc http  $B/s4-v1 -next-binary=$B/s4-v2crash -ready-timeout=3s ;;
  s5)               run $sc http  $B/s5-v1 -next-binary=$B/s5-v2 ;;
  s5-stall)         run $sc http  $B/s5-v1 -next-binary=$B/s5-v2 -stall-accept=300ms ;;
  s5-sighup)        run $sc sighup $B/s5-v1 -next-binary=$B/s5-v2 ;;
  s5-rollback-hang) run $sc http  $B/s5-v1 -next-binary=$B/s5-v2hang -ready-timeout=2s ;;
  s5-rollback-crash) run $sc http $B/s5-v1 -next-binary=$B/s5-v2crash -ready-timeout=3s ;;
  s5-twice)
    run $sc http $B/s5-v1 -next-binary=$B/s5-v2
    echo "--- second upgrade v2 -> v3 (re-exec self with -version=v3) ---"
    HAMMER_ARGS="-to v3" $B/hammer -c $C -d $D -upgrade-at $AT -work-ms $WORK -to v3
    sleep 1; echo "after2: $(curl -s http://127.0.0.1:8080/admin/status)"
    grep '"ev"' /tmp/ev-$sc.log | tail -4 | sed 's/^/  /' ;;
  s5-supervised)
    run $sc sighup $B/s5-v1 -supervise -next-binary=$B/s5-v2
    echo "process tree after upgrade (supervisor stays, new worker reparented to it):"
    ps -o pid,ppid,args -C s5-v1,s5-v2 2>/dev/null || ps -eo pid,ppid,args | grep dimpbin | grep -v grep ;;
  *) echo "unknown scenario $sc" ;;
esac; done
cleanup
