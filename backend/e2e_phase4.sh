#!/bin/bash
# FixLab Phase 4 end-to-end acceptance test: deterministic rule engine +
# kill switch (spec §31, §32, §33).
#
# With the REAL external QuickFIX/Go initiator (fixclient in script mode):
#   R1: IF Symbol(55) eq MSFT THEN FULL_FILL
#     NOS MSFT -> client receives ER(150=F/39=2) with NO execute API call
#   R2 (higher priority): IF OrderQty(38) gt 10000 THEN REJECT
#     NOS MSFT qty 20000 -> rejected by R2, not filled by R1 (priority proof)
#   R3: IF Symbol eq DELAYSYM THEN DELAY 500ms -> FULL_FILL
#     measured delay between NOS and fill ER >= 500ms
#   kill switch on -> NOS AAPL -> immediate 150=8/39=8 "VENUE HALTED",
#     rule_matches metric unchanged (rules not evaluated)
#   kill switch off -> NOS AAPL -> stays NEW, only the plain ack ER
# plus rule CRUD (create/get/update/disable/delete, 400 on invalid) and
# the manual-execution conflict (rule-filled order -> 409).
#
# Usage: ./backend/e2e_phase4.sh   (run from the repo root)
set -u
trap '' PIPE
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
CID="E2E4_$$"
SERVER_LOG=/tmp/fixlab-e2e4-server.log
CLIENT_LOG=/tmp/fixlab-e2e4-client.log
WS_LOG=/tmp/fixlab-e2e4-ws.log
FIFO=/tmp/fixlab-e2e4-cmd
PASS=0; FAIL=0

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

http_code_body() {
  local resp code body
  resp=$(curl -s -w '\n%{http_code}' "$@")
  code="${resp##*$'\n'}"
  body="${resp%$'\n'*}"
  printf '%s|%s' "$code" "$body"
}

rule_matches() {
  curl -s http://127.0.0.1:8080/api/v1/metrics | python3 -c "import json,sys; print(json.load(sys.stdin)['rule_matches'])"
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
"$BIN/wsprobe" --url "ws://127.0.0.1:8080/ws/session/$TOKEN" --dur 300s >"$WS_LOG" 2>&1 &
WS=$!
sleep 1
grep -q 'WSPROBE: CONNECTED' "$WS_LOG" && ok "ws probe subscribed" || fail "ws probe"

echo "== rule R1: IF Symbol eq MSFT THEN FULL_FILL =="
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules" \
  -H 'Content-Type: application/json' -d '{
    "name":"msft-autofill","priority":10,"enabled":true,
    "predicates":[{"tag":55,"op":"eq","value":"MSFT"}],
    "actions":[{"type":"FULL_FILL"}]}')
[[ "${R%%|*}" == "200" ]] && ok "rule R1 created -> 200" || fail "R1 create -> $R"
R1ID=$(python3 -c "import json,sys; print(json.load(sys.stdin)['id'])" <<<"${R#*|}")
echo "   R1 id=$R1ID"

echo "== external FIX client (script mode) =="
rm -f "$FIFO"; mkfifo "$FIFO"
(cd "$ROOT" && "$BIN/fixclient" --host 127.0.0.1 --port "$PORT" \
  --sender "$CID" --target FIXLAB --heartbt 30 --script \
  --dict backend/specs/FIX44.xml <"$FIFO" >"$CLIENT_LOG" 2>&1) &
CLI=$!
exec 3>"$FIFO"
if wait_for 'FIXCLIENT: LOGON_OK' "$CLIENT_LOG" 20; then
  ok "client logon"
else
  fail "client logon"; echo "--- client log ---"; tail -10 "$CLIENT_LOG"
fi

echo "== 1. rule-fired FULL_FILL with NO execute API call =="
BEFORE=$(wc -l <"$CLIENT_LOG")
echo "NOS R4A MSFT 1 100 300.00 2" >&3
ER=$(new_er_fields "$BEFORE" 20) || fail "fill ER received"
check_fields "$ER" "150=F" "39=2" "11=R4A" "14=100" "151=0" "32=100" "31=300" "6=300" \
  && ok "client: rule fill ER 150=F/39=2 cum=100 leaves=0" || fail "fill ER fields"
wait_order_status R4A FILLED 20 && ok "blotter: R4A FILLED" || fail "blotter R4A"
grep -q 'WSPROBE: ORDER_CREATED' "$WS_LOG" && ok "ws: ORDER_CREATED" || fail "ws ORDER_CREATED"
grep -q 'WSPROBE: ORDER_UPDATED' "$WS_LOG" && ok "ws: ORDER_UPDATED" || fail "ws ORDER_UPDATED"
grep -q 'WSPROBE: EXECUTION_SENT' "$WS_LOG" && ok "ws: EXECUTION_SENT" || fail "ws EXECUTION_SENT"
[[ "$(rule_matches)" == "1" ]] && ok "metric: rule_matches=1" || fail "rule_matches=$(rule_matches)"

echo "== rule R2 (priority 5): IF OrderQty gt 10000 THEN REJECT =="
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules" \
  -H 'Content-Type: application/json' -d '{
    "name":"size-limit","priority":5,"enabled":true,
    "predicates":[{"tag":38,"op":"gt","value":"10000"}],
    "actions":[{"type":"REJECT","ordRejReason":"99","text":"size limit"}]}')
[[ "${R%%|*}" == "200" ]] && ok "rule R2 created -> 200" || fail "R2 create -> $R"
R2ID=$(python3 -c "import json,sys; print(json.load(sys.stdin)['id'])" <<<"${R#*|}")

echo "== 2. priority: MSFT qty 20000 matches both R1 and R2 -> R2 REJECT wins =="
BEFORE=$(wc -l <"$CLIENT_LOG")
echo "NOS R4B MSFT 1 20000 300.00 2" >&3
ER=$(new_er_fields "$BEFORE" 20) || fail "reject ER received"
check_fields "$ER" "150=8" "39=8" "11=R4B" "103=99" \
  && ok "client: reject ER 150=8/39=8 103=99 (R2, not R1 fill)" || fail "reject ER fields"
echo "$ER" | grep -q "58=size limit" && ok "client: reject text 'size limit'" || fail "reject text"
wait_order_status R4B REJECTED 20 && ok "blotter: R4B REJECTED" || fail "blotter R4B"
[[ "$(rule_matches)" == "2" ]] && ok "metric: rule_matches=2" || fail "rule_matches=$(rule_matches)"
MC=$(curl -s "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules" | python3 -c "
import json,sys
rs={r['id']:r for r in json.load(sys.stdin)['rules']}
print(rs['$R1ID']['matchedCount'], rs['$R2ID']['matchedCount'])")
[[ "$MC" == "1 1" ]] && ok "per-rule matchedCount: R1=1 R2=1" || fail "matchedCount: $MC"

echo "== rule R3 (priority 7): IF Symbol eq DELAYSYM THEN DELAY 500ms -> FULL_FILL =="
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules" \
  -H 'Content-Type: application/json' -d '{
    "name":"delayed-fill","priority":7,"enabled":true,
    "predicates":[{"tag":55,"op":"eq","value":"DELAYSYM"}],
    "actions":[{"type":"DELAY","delayMs":500},{"type":"FULL_FILL"}]}')
[[ "${R%%|*}" == "200" ]] && ok "rule R3 created -> 200" || fail "R3 create -> $R"

echo "== 3. DELAY 500ms before the fill ER =="
BEFORE=$(wc -l <"$CLIENT_LOG")
START_MS=$(date +%s%3N)
echo "NOS R4C DELAYSYM 1 100 10.00 2" >&3
ER=$(new_er_fields "$BEFORE" 20) || fail "delayed fill ER received"
END_MS=$(date +%s%3N)
ELAPSED=$((END_MS - START_MS))
check_fields "$ER" "150=F" "39=2" "11=R4C" "14=100" "151=0" \
  && ok "client: delayed fill ER correct" || fail "delayed fill ER fields"
if [ "$ELAPSED" -ge 500 ]; then
  ok "delay measured ${ELAPSED}ms >= 500ms"
else
  fail "delay measured ${ELAPSED}ms < 500ms"
fi
wait_order_status R4C FILLED 20 && ok "blotter: R4C FILLED" || fail "blotter R4C"

echo "== 4. kill switch ON -> immediate reject, rules not evaluated =="
M_BEFORE=$(rule_matches)
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/killswitch" \
  -H 'Content-Type: application/json' -d '{"enabled":true}')
[[ "${R%%|*}" == "200" ]] && ok "killswitch on -> 200" || fail "killswitch -> $R"
grep -q 'WSPROBE: KILL_SWITCH' "$WS_LOG" && ok "ws: KILL_SWITCH event" || fail "ws KILL_SWITCH"
KS=$(curl -s "http://127.0.0.1:8080/api/v1/sessions/$TOKEN" | python3 -c "import json,sys; print(json.load(sys.stdin)['killSwitch'])")
[[ "$KS" == "True" ]] && ok "session GET shows killSwitch=true" || fail "killSwitch=$KS"
BEFORE=$(wc -l <"$CLIENT_LOG")
echo "NOS R4D AAPL 1 50 150.00 2" >&3
ER=$(new_er_fields "$BEFORE" 20) || fail "halt reject ER received"
check_fields "$ER" "150=8" "39=8" "11=R4D" \
  && ok "client: halt reject ER 150=8/39=8" || fail "halt reject ER fields"
echo "$ER" | grep -q "58=VENUE HALTED" && ok "client: halt text 'VENUE HALTED'" || fail "halt text"
wait_order_status R4D REJECTED 20 && ok "blotter: R4D REJECTED" || fail "blotter R4D"
M_AFTER=$(rule_matches)
[[ "$M_BEFORE" == "$M_AFTER" ]] && ok "rule_matches unchanged ($M_AFTER): rules skipped" || fail "rule_matches $M_BEFORE -> $M_AFTER"

echo "== 5. kill switch OFF -> unmatched order stays NEW, no rule ER =="
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/killswitch" \
  -H 'Content-Type: application/json' -d '{"enabled":false}')
[[ "${R%%|*}" == "200" ]] && ok "killswitch off -> 200" || fail "killswitch off -> $R"
BEFORE=$(wc -l <"$CLIENT_LOG")
echo "NOS R4E AAPL 1 50 150.00 2" >&3
wait_order_status R4E NEW 20 && ok "blotter: R4E NEW (awaiting manual execution)" || fail "blotter R4E"
sleep 2  # the plain New ack (150=0) arrives; no rule execution must follow
SEG=$(tail -n +"$((BEFORE+1))" "$CLIENT_LOG")
if echo "$SEG" | grep 'ER_FIELDS' | grep -q "11=R4E"; then
  # only the ack may mention R4E; any execution ER (F/D/8/4/5) is a failure
  echo "$SEG" | grep 'ER_FIELDS' | grep "11=R4E" | grep -Eq "150=(F|D|8|4|5)" \
    && fail "unexpected rule execution ER for R4E" \
    || ok "no rule execution for unmatched order (only the New ack)"
else
  ok "no execution ER for unmatched order"
fi

echo "== 6. rule CRUD =="
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules" \
  -H 'Content-Type: application/json' -d '{
    "name":"crud","priority":99,"enabled":true,
    "predicates":[{"tag":55,"op":"contains","value":"CRUD"}],
    "actions":[{"type":"ACK_NEW"}]}')
[[ "${R%%|*}" == "200" ]] && ok "create -> 200" || fail "create -> $R"
RID=$(python3 -c "import json,sys; print(json.load(sys.stdin)['id'])" <<<"${R#*|}")
curl -s "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules" | grep -q "$RID" \
  && ok "list shows the rule" || fail "list missing rule"
R=$(http_code_body -X PUT "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules/$RID" \
  -H 'Content-Type: application/json' -d '{
    "name":"crud","priority":1,"enabled":false,
    "predicates":[{"tag":55,"op":"contains","value":"CRUD"}],
    "actions":[{"type":"ACK_NEW"}]}')
[[ "${R%%|*}" == "200" ]] && ok "update -> 200" || fail "update -> $R"
EN=$(curl -s "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules" | python3 -c "
import json,sys
r=[x for x in json.load(sys.stdin)['rules'] if x['id']=='$RID'][0]
print(r['priority'], r['enabled'])")
[[ "$EN" == "1 False" ]] && ok "update persisted (priority 1, disabled)" || fail "update not persisted: $EN"
R=$(http_code_body -X DELETE "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules/$RID")
[[ "${R%%|*}" == "200" ]] && ok "delete -> 200" || fail "delete -> $R"
R=$(http_code_body -X DELETE "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules/$RID")
[[ "${R%%|*}" == "404" ]] && ok "re-delete -> 404" || fail "re-delete -> $R"
R=$(http_code_body -X PUT "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules/NOPE" \
  -H 'Content-Type: application/json' -d '{"name":"x","priority":1,"actions":[{"type":"ACK_NEW"}]}')
[[ "${R%%|*}" == "404" ]] && ok "update unknown -> 404" || fail "update unknown -> $R"

echo "== 7. invalid rules -> 400 =="
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules" \
  -H 'Content-Type: application/json' -d '{
    "name":"bad-tag","priority":1,"enabled":true,
    "predicates":[{"tag":999,"op":"eq","value":"x"}],
    "actions":[{"type":"ACK_NEW"}]}')
[[ "${R%%|*}" == "400" ]] && ok "unknown tag -> 400" || fail "bad tag -> $R"
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules" \
  -H 'Content-Type: application/json' -d '{
    "name":"bad-op","priority":1,"enabled":true,
    "predicates":[{"tag":55,"op":"bogus","value":"x"}],
    "actions":[{"type":"ACK_NEW"}]}')
[[ "${R%%|*}" == "400" ]] && ok "unknown op -> 400" || fail "bad op -> $R"
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules" \
  -H 'Content-Type: application/json' -d '{
    "name":"bad-action","priority":1,"enabled":true,
    "predicates":[{"tag":55,"op":"eq","value":"x"}],
    "actions":[{"type":"EXPLODE"}]}')
[[ "${R%%|*}" == "400" ]] && ok "unknown action -> 400" || fail "bad action -> $R"
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules" \
  -H 'Content-Type: application/json' -d '{
    "name":"no-actions","priority":1,"enabled":true,
    "predicates":[{"tag":55,"op":"eq","value":"x"}],
    "actions":[]}')
[[ "${R%%|*}" == "400" ]] && ok "empty actions -> 400" || fail "no actions -> $R"

echo "== 8. manual execution on rule-filled order -> 409 =="
R=$(http_code_body -X POST \
  "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/orders/R4A/execute" \
  -H 'Content-Type: application/json' -d '{"action":"FILL","qty":1,"price":1}')
[[ "${R%%|*}" == "409" ]] && ok "fill on rule-FILLED order -> 409" || fail "conflict -> $R"

echo "== client logout =="
echo "LOGOUT" >&3
exec 3>&-   # close the FIFO writer
wait $CLI 2>/dev/null
grep -q 'FIXCLIENT: LOGOUT_OK' "$CLIENT_LOG" && ok "client: clean logout" || fail "logout"
kill $WS 2>/dev/null

kill $SRV 2>/dev/null; trap - EXIT; rm -f "$FIFO"
echo
echo "RESULT: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
