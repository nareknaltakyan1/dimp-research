#!/usr/bin/env bash
# End-to-end smoke test for any contract server (S1/S2/S3).
# usage: smoke.sh <server-binary> [port] [extra server args...]
# Starts the server, runs 4 curl loops against /api (alternating work_ms=50
# and 0), POSTs /admin/upgrade?to=v2, then sends SIGHUP (back to v1), and
# prints HTTP status histogram, per-version counts, count duplicates and the
# stderr event log. Needs bash, curl, python3.
set -u
BIN=$1; PORT=${2:-18080}; shift 2 || true
LOG=${TMPDIR:-/tmp}/dimp-smoke-$PORT
$BIN -addr 127.0.0.1:$PORT "$@" 2>$LOG.events &
PID=$!
for i in $(seq 1 100); do curl -sf 127.0.0.1:$PORT/healthz >/dev/null && break; sleep 0.05; done
: > $LOG.out
load() { while [ ! -f $LOG.stop ]; do curl -s -o /dev/null -w '%{http_code} ' 127.0.0.1:$PORT/api?work_ms=50 >>$LOG.codes; curl -s 127.0.0.1:$PORT/api >>$LOG.out; done; }
rm -f $LOG.stop $LOG.codes
for w in 1 2 3 4; do load & done
sleep 1
echo "status before: $(curl -s 127.0.0.1:$PORT/admin/status)"
echo "upgrade: $(curl -s -o /dev/null -w '%{http_code}' -XPOST "127.0.0.1:$PORT/admin/upgrade?to=v2")"
sleep 0.03; echo "status during: $(curl -s 127.0.0.1:$PORT/admin/status)"
sleep 2
touch $LOG.stop; wait $(jobs -p | grep -v "^$PID$") 2>/dev/null
echo "status after: $(curl -s 127.0.0.1:$PORT/admin/status)"
kill -HUP $PID; sleep 1.5
echo "status after SIGHUP: $(curl -s 127.0.0.1:$PORT/admin/status)"
kill $PID; wait $PID 2>/dev/null
echo "http codes: $(tr ' ' '\n' < $LOG.codes | grep -v '^$' | sort | uniq -c | tr '\n' ' ')"
echo "versions: $(grep -o '"version":"[^"]*"' $LOG.out | sort | uniq -c | tr '\n' ' ')"
python3 - $LOG.out <<'PY'
import json,sys
rows=[json.loads(l) for l in open(sys.argv[1]) if l.strip()]
v1=[r["count"] for r in rows if r["version"]=="v1"]; v2=[r["count"] for r in rows if r["version"]!="v1"]
cs=[r["count"] for r in rows]
print("responses",len(rows),"dups",len(cs)-len(set(cs)),"max v1 count",max(v1) if v1 else None,"min v2 count",min(v2) if v2 else None, "first count",cs[0] if cs else None)
PY
echo "--- events"; cat $LOG.events
