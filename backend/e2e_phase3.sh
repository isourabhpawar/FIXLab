#!/bin/bash
# FixLab Phase 3 end-to-end acceptance test: trading simulation
# (spec §23-§29, §37, §50).
#
# Full lifecycle with the REAL external QuickFIX/Go initiator (fixclient
# in script mode):
#   NOS -> ack ER(0/0) -> PARTIAL_FILL -> FILL -> REJECT ->
#   cancel accept -> cancel reject -> replace accept -> replace reject
# plus WS event verification (ORDER_CREATED/UPDATED, EXECUTION_SENT)
# and the application-message quota enforcement check.
#
# Usage: ./backend/e2e_phase3.sh   (run from the repo root)
set -u
# A dead FIX client must not SIGPIPE-kill the harness mid-run; failed
# FIFO writes surface as ordinary errors instead.
trap '' PIPE
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
# Unique CompID per run so a stale client from an earlier run can never
# collide with this run's FIX session.
CID="E2E3_$$"
SERVER_LOG=/tmp/fixlab-e2e3-server.log
CLIENT_LOG=/tmp/fixlab-e2e3-client.log
WS_LOG=/tmp/fixlab-e2e3-ws.log
FIFO=/tmp/fixlab-e2e3-cmd
PASS=0; FAIL=0

# Kill leftover test processes from earlier runs (never ourselves: the
# bracket patterns can't match our own command line).
cleanup_stale() {
  for pat in '[b]in/fixlab-server' '[b]in/fixclient' '[b]in/wsprobe'; do
    for pid in $(ps -eo pid,args | grep "$pat" | awk '{print $1}'); do
      [ "$pid" != "$$" ] && kill "$pid" 2>/dev/null || true
    done
  done
  sleep 1
}
cleanup_stale

ok()   { PASS=$((PASS+1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL+1)); echo "  FAIL: $1"; }

need() { command -v "$1" >/dev/null || { echo "missing: $1"; exit 2; }; }
need curl; need python3

wait_for() { # pattern logfile [timeout_s]
  local pat=$1 log=$2 timeout=${3:-20} i=0
  while ! grep -q "$pat" "$log" 2>/dev/null; do
    sleep 0.5; i=$((i+1))
    if [ "$i" -ge $((timeout*2)) ]; then
      echo "    wait_for timeout after ${timeout}s for [$pat] in $log (size: $(wc -c <"$log" 2>/dev/null) bytes)"
      return 1
    fi
  done
}

# new_er_fields <lines-before> [timeout] -> prints newest ER_FIELDS line
new_er_fields() {
  local before=$1 timeout=${2:-20} i=0 line=""
  while [ -z "$line" ]; do
    line=$(tail -n +"$((before+1))" "$CLIENT_LOG" | grep 'ER_FIELDS' | tail -1)
    sleep 0.5; i=$((i+1))
    if [ "$i" -ge $((timeout*2)) ]; then
      echo "    new_er_fields timeout after ${timeout}s (client log lines: $(wc -l <"$CLIENT_LOG" 2>/dev/null))"
      return 1
    fi
  done
  echo "$line"
}

# check_fields <line> <field=value>...
check_fields() {
  local line=$1; shift; local fv
  for fv in "$@"; do
    echo "$line" | grep -Eq "(^| )$fv( |$)" || { echo "    missing [$fv] in: $line"; return 1; }
  done
}

order_json() { # clOrdId -> order json ({} if missing)
  curl -s "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/orders" | python3 -c "
import json,sys
ds=json.load(sys.stdin)['orders']
m={o['clOrdId']:o for o in ds}
print(json.dumps(m.get('$1', {})))"
}

wait_order_status() { # clOrdId want_status [timeout]
  local id=$1 want=$2 timeout=${3:-20} i=0 st=""
  while [ "$st" != "$want" ]; do
    st=$(order_json "$id" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status','MISSING'))")
    sleep 0.5; i=$((i+1))
    if [ "$i" -ge $((timeout*2)) ]; then
      echo "    wait_order_status timeout: $id last status [$st], want [$want]"
      return 1
    fi
  done
}

# http_code_body <curl args...> -> prints "code|body". Splits the status
# off the end instead of line-reading: Go's JSON bodies already end with
# a newline, so a naive read loop sees a blank line before the code.
http_code_body() {
  local resp code body
  resp=$(curl -s -w '\n%{http_code}' "$@")
  code="${resp##*$'\n'}"
  body="${resp%$'\n'*}"
  printf '%s|%s' "$code" "$body"
}

execute() { # clOrdId json-body -> prints "code|body"
  local id=$1 body=$2
  http_code_body -X POST \
    "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/orders/$id/execute" \
    -H 'Content-Type: application/json' -d "$body"
}

echo "== building =="
export PATH="$HOME/go/go/bin:$PATH"
(cd "$ROOT" && go build -o bin/fixlab-server ./backend/cmd/server \
              && go build -o bin/fixclient ./backend/cmd/fixclient \
              && go build -o bin/wsprobe ./backend/cmd/wsprobe) || exit 1

echo "== starting server =="
FIXLAB_HTTP_ADDR=127.0.0.1:8080 "$BIN/fixlab-server" >"$SERVER_LOG" 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null; rm -f $FIFO' EXIT
sleep 2
curl -sf http://127.0.0.1:8080/healthz >/dev/null || { echo "server did not start"; cat "$SERVER_LOG"; exit 1; }
ok "server healthy"

echo "== create session =="
CREATE=$(curl -sf -X POST http://127.0.0.1:8080/api/v1/sessions \
  -H 'Content-Type: application/json' -d '{"targetCompId":"'"$CID"'"}') || exit 1
TOKEN=$(python3 -c "import json,sys; print(json.load(sys.stdin)['sessionToken'])" <<<"$CREATE")
PORT=$(python3 -c "import json,sys; print(json.load(sys.stdin)['endpoint']['port'])" <<<"$CREATE")
echo "   port=$PORT"

echo "== ws probe =="
"$BIN/wsprobe" --url "ws://127.0.0.1:8080/ws/session/$TOKEN" --dur 150s >"$WS_LOG" 2>&1 &
WS=$!
sleep 1
grep -q 'WSPROBE: CONNECTED' "$WS_LOG" && ok "ws probe subscribed" || fail "ws probe"

echo "== external FIX client (script mode) =="
rm -f "$FIFO"; mkfifo "$FIFO"
(cd "$ROOT" && "$BIN/fixclient" --host 127.0.0.1 --port "$PORT" \
  --sender "$CID" --target FIXLAB --heartbt 30 --script \
  --dict backend/specs/FIX44.xml <"$FIFO" >"$CLIENT_LOG" 2>&1) &
CLI=$!
exec 3>"$FIFO"   # keep the writer open so the client sees EOF only at close
if wait_for 'FIXCLIENT: LOGON_OK' "$CLIENT_LOG" 20; then
  ok "client logon"
else
  fail "client logon"; echo "--- client log ---"; tail -10 "$CLIENT_LOG"
fi

echo "== 1. NewOrderSingle -> ORDER_CREATED + ack ER(0/0) =="
BEFORE=$(wc -l <"$CLIENT_LOG")
echo "NOS ORD1 MSFT 1 1000 310.50 2" >&3
wait_order_status ORD1 NEW 20 && ok "blotter: ORD1 NEW" || fail "blotter ORD1"
ER=$(new_er_fields "$BEFORE" 20) || fail "ack ER received"
check_fields "$ER" "150=0" "39=0" "11=ORD1" "14=0" "151=1000" "6=0" \
  && ok "client: ack ER 150=0/39=0" || fail "ack ER fields"
grep -q 'WSPROBE: ORDER_CREATED' "$WS_LOG" && ok "ws: ORDER_CREATED" || fail "ws ORDER_CREATED"

echo "== 2. PARTIAL_FILL 400 @ 310.50 =="
BEFORE=$(wc -l <"$CLIENT_LOG")
R=$(execute ORD1 '{"action":"PARTIAL_FILL","qty":400,"price":310.50}')
[[ "${R%%|*}" == "200" ]] && ok "execute -> 200" || fail "execute PARTIAL_FILL -> $R"
ER=$(new_er_fields "$BEFORE" 20) || fail "partial ER received"
check_fields "$ER" "150=D" "39=1" "11=ORD1" "14=400" "151=600" "32=400" "31=310.5" "6=310.5" \
  && ok "client: partial ER 150=D/39=1 cum=400 leaves=600" || fail "partial ER fields"
wait_order_status ORD1 PARTIALLY_FILLED 20 && ok "blotter: PARTIALLY_FILLED" || fail "status"

echo "== 3. FILL 600 @ 310.60 (weighted avg 310.56) =="
BEFORE=$(wc -l <"$CLIENT_LOG")
R=$(execute ORD1 '{"action":"FILL","qty":600,"price":310.60}')
[[ "${R%%|*}" == "200" ]] && ok "execute -> 200" || fail "execute FILL -> $R"
ER=$(new_er_fields "$BEFORE" 20) || fail "fill ER received"
check_fields "$ER" "150=F" "39=2" "11=ORD1" "14=1000" "151=0" "32=600" "31=310.6" "6=310.56" \
  && ok "client: fill ER 150=F/39=2 cum=1000 avg=310.56" || fail "fill ER fields"
wait_order_status ORD1 FILLED 20 && ok "blotter: FILLED" || fail "status"
grep -q 'WSPROBE: EXECUTION_SENT' "$WS_LOG" && ok "ws: EXECUTION_SENT" || fail "ws EXECUTION_SENT"
grep -q 'WSPROBE: ORDER_UPDATED' "$WS_LOG" && ok "ws: ORDER_UPDATED" || fail "ws ORDER_UPDATED"

echo "== 4. REJECT flow =="
echo "NOS ORD2 MSFT 1 500 300.00 2" >&3
wait_order_status ORD2 NEW 20 && ok "blotter: ORD2 NEW" || fail "blotter ORD2"
BEFORE=$(wc -l <"$CLIENT_LOG")
R=$(execute ORD2 '{"action":"REJECT","ordRejReason":"2","text":"Exchange closed"}')
[[ "${R%%|*}" == "200" ]] && ok "execute -> 200" || fail "execute REJECT -> $R"
ER=$(new_er_fields "$BEFORE" 20) || fail "reject ER received"
check_fields "$ER" "150=8" "39=8" "11=ORD2" "103=2" \
  && ok "client: reject ER 150=8/39=8 103=2" || fail "reject ER fields"
echo "$ER" | grep -q "58=Exchange closed" && ok "client: reject Text present" || fail "reject text"
wait_order_status ORD2 REJECTED 20 && ok "blotter: REJECTED" || fail "status"

echo "== 5. CANCEL accept flow =="
echo "NOS ORD3 MSFT 1 700 305.00 2" >&3
wait_order_status ORD3 NEW 20 || fail "blotter ORD3"
echo "CANCEL CX1 ORD3" >&3
wait_order_status ORD3 PENDING_CANCEL 20 && ok "blotter: PENDING_CANCEL" || fail "pending cancel"
BEFORE=$(wc -l <"$CLIENT_LOG")
R=$(execute ORD3 '{"action":"CANCEL_ACCEPT","text":"ok"}')
[[ "${R%%|*}" == "200" ]] && ok "execute -> 200" || fail "execute CANCEL_ACCEPT -> $R"
ER=$(new_er_fields "$BEFORE" 20) || fail "cancel ER received"
check_fields "$ER" "150=4" "39=4" "11=ORD3" "151=0" \
  && ok "client: cancel ER 150=4/39=4" || fail "cancel ER fields"
wait_order_status ORD3 CANCELED 20 && ok "blotter: CANCELED" || fail "status"

echo "== 6. CANCEL reject flow (35=9) =="
echo "NOS ORD4 MSFT 1 700 305.00 2" >&3
wait_order_status ORD4 NEW 20 || fail "blotter ORD4"
echo "CANCEL CX2 ORD4" >&3
wait_order_status ORD4 PENDING_CANCEL 20 || fail "pending cancel"
BEFORE=$(wc -l <"$CLIENT_LOG")
R=$(execute ORD4 '{"action":"CANCEL_REJECT","cxlRejReason":"1","text":"too late"}')
[[ "${R%%|*}" == "200" ]] && ok "execute -> 200" || fail "execute CANCEL_REJECT -> $R"
sleep 1
SEG=$(tail -n +"$((BEFORE+1))" "$CLIENT_LOG")
echo "$SEG" | grep -q "ER_RX.*35=9" && ok "client: 35=9 OrderCancelReject" || fail "35=9"
echo "$SEG" | grep "ER_FIELDS" | tail -1 | grep -Eq "(^| )434=1( |$)" && ok "client: 434=1 (cancel request)" || fail "434"
echo "$SEG" | grep "ER_FIELDS" | tail -1 | grep -Eq "(^| )102=1( |$)" && ok "client: 102=1 reason" || fail "102"
wait_order_status ORD4 NEW 20 && ok "blotter: back to NEW" || fail "status restore"

echo "== 7. REPLACE accept flow =="
echo "NOS ORD5 MSFT 1 1000 310.50 2" >&3
wait_order_status ORD5 NEW 20 || fail "blotter ORD5"
echo "REPLACE RP1 ORD5 1200 311.00" >&3
sleep 2
PR=$(order_json ORD5 | python3 -c "import json,sys; pr=json.load(sys.stdin).get('pendingReplace') or {}; print(pr.get('orderQty'), pr.get('price'))")
[[ "$PR" == "1200 311" ]] && ok "blotter: pending replace staged" || fail "pending replace: $PR"
BEFORE=$(wc -l <"$CLIENT_LOG")
R=$(execute ORD5 '{"action":"REPLACE_ACCEPT"}')
[[ "${R%%|*}" == "200" ]] && ok "execute -> 200" || fail "execute REPLACE_ACCEPT -> $R"
ER=$(new_er_fields "$BEFORE" 20) || fail "replace ER received"
check_fields "$ER" "150=5" "11=RP1" "41=ORD5" "38=1200" "44=311" \
  && ok "client: replace ER 150=5 new qty/price" || fail "replace ER fields"
sleep 1
OJ=$(order_json ORD5)
echo "$OJ" | python3 -c "
import json,sys; o=json.load(sys.stdin)
assert o['orderQty']==1200 and o['price']==311 and o['leavesQty']==1200 and o['status']=='REPLACED', o
print('ok')" && ok "blotter: REPLACED 1200 @ 311.00" || fail "replace blotter"

echo "== 8. REPLACE reject flow (35=9) =="
echo "NOS ORD6 MSFT 1 1000 310.50 2" >&3
wait_order_status ORD6 NEW 20 || fail "blotter ORD6"
echo "REPLACE RP2 ORD6 1500 312.00" >&3
sleep 2
BEFORE=$(wc -l <"$CLIENT_LOG")
R=$(execute ORD6 '{"action":"REPLACE_REJECT","cxlRejReason":"2","text":"no"}')
[[ "${R%%|*}" == "200" ]] && ok "execute -> 200" || fail "execute REPLACE_REJECT -> $R"
sleep 1
SEG=$(tail -n +"$((BEFORE+1))" "$CLIENT_LOG")
echo "$SEG" | grep -q "ER_RX.*35=9" && ok "client: 35=9 OrderCancelReject" || fail "35=9"
echo "$SEG" | grep "ER_FIELDS" | tail -1 | grep -Eq "(^| )434=2( |$)" && ok "client: 434=2 (replace request)" || fail "434"

echo "== 9. invalid transitions =="
R=$(execute ORD1 '{"action":"FILL","qty":1,"price":1}')
[[ "${R%%|*}" == "409" ]] && ok "fill on FILLED -> 409" || fail "fill on FILLED -> $R"
R=$(execute NOPE '{"action":"FILL","qty":1,"price":1}')
[[ "${R%%|*}" == "404" ]] && ok "unknown order -> 404" || fail "unknown -> $R"
R=$(execute ORD4 '{"action":"PARTIAL_FILL","qty":99999,"price":1}')
[[ "${R%%|*}" == "400" ]] && ok "partial > leaves -> 400" || fail "oversize partial -> $R"
R=$(execute ORD4 '{"action":"BOGUS"}')
[[ "${R%%|*}" == "400" ]] && ok "unknown action -> 400" || fail "bogus action -> $R"

echo "== client logout =="
echo "LOGOUT" >&3
exec 3>&-   # close the FIFO writer
wait $CLI 2>/dev/null
grep -q 'FIXCLIENT: LOGOUT_OK' "$CLIENT_LOG" && ok "client: clean logout" || fail "logout"
kill $WS 2>/dev/null

echo "== 10. quota enforcement (limit=6) =="
FIXLAB_HTTP_ADDR=127.0.0.1:8081 FIXLAB_APP_MSG_LIMIT=6 \
  "$BIN/fixlab-server" >/tmp/fixlab-e2e3-quota.log 2>&1 &
QSRV=$!
sleep 2
QC=$(curl -sf -X POST http://127.0.0.1:8081/api/v1/sessions \
  -H 'Content-Type: application/json' -d '{"targetCompId":"QUOTA"}') || { echo "quota server create failed"; exit 1; }
QTOKEN=$(python3 -c "import json,sys; print(json.load(sys.stdin)['sessionToken'])" <<<"$QC")
QPORT=$(python3 -c "import json,sys; print(json.load(sys.stdin)['endpoint']['port'])" <<<"$QC")
QEXE() { http_code_body -X POST \
  "http://127.0.0.1:8081/api/v1/sessions/$QTOKEN/orders/$1/execute" \
  -H 'Content-Type: application/json' -d "$2"; }
QCMD=/tmp/fixlab-e2e3-qcmd; rm -f "$QCMD"; mkfifo "$QCMD"
(cd "$ROOT" && timeout 60 "$BIN/fixclient" --host 127.0.0.1 --port "$QPORT" \
  --sender QUOTA --target FIXLAB --heartbt 30 --script \
  --dict backend/specs/FIX44.xml <"$QCMD" >/tmp/fixlab-e2e3-qclient.log 2>&1) &
QCLI=$!
exec 4>"$QCMD"
sleep 8  # logon + testreq
echo "NOS Q1 MSFT 1 100 10.00 2" >&4
sleep 2  # inbound NOS (1) + ack ER (2)
R=$(QEXE Q1 '{"action":"PARTIAL_FILL","qty":40,"price":10.00}'); [[ "${R%%|*}" == "200" ]] || fail "q partial -> $R"
sleep 1  # (3)
R=$(QEXE Q1 '{"action":"FILL","qty":60,"price":10.10}'); [[ "${R%%|*}" == "200" ]] || fail "q fill -> $R"
sleep 1  # (4)
echo "NOS Q2 MSFT 1 50 9.00 2" >&4
sleep 2  # inbound (5) + ack (6) -> quota exhausted
R=$(QEXE Q2 '{"action":"REJECT","text":"no room"}')
CODE="${R%%|*}"
[[ "$CODE" == "429" ]] && ok "execution past quota -> 429" || fail "quota -> $R"
echo "$R" | grep -qi "quota exceeded" && ok "429 message names the quota" || fail "quota message"
exec 4>&-; kill $QCLI 2>/dev/null; kill $QSRV 2>/dev/null; rm -f "$QCMD"

kill $SRV 2>/dev/null; trap - EXIT; rm -f "$FIFO"
echo
echo "RESULT: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
