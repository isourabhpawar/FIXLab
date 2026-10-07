#!/bin/bash
# FixLab Phase 2.1 end-to-end acceptance test: stochastic simulator
# (spec §48).
#
# With the REAL external QuickFIX/Go initiator (fixclient in script mode):
#   1. 100/0/0 latency 0  -> NOS -> ER(150=F/39=2), no API call in between
#   2. 0/100/0            -> NOS -> ER(150=8/39=8, 103=99)
#   3. 0/0/100            -> NOS -> ER(150=D/39=1), leaves > 0, still working
#   4. 50/30/20 seed 7    -> 20 orders: all three outcomes occur AND the
#      identical outcome sequence reproduces on a fresh session with the
#      same seed (determinism proof)
#   5. precedence: deterministic rule REJECT beats stochastic 100% accept;
#      kill switch beats stochastic (VENUE HALTED)
#   6. latency: avg 300ms/stddev 0 -> NOS->ER >= 280ms; avg 0 -> fast path
#   7. invalid configs -> 400 naming the problem (sum 99, negative latency)
#   8. manual race: stochastic 100% accept with 2000ms delay; immediate
#      manual FILL wins; exactly one terminal ER, no double-execution
#
# Usage: ./backend/e2e_phase2_1.sh   (run from the repo root)
set -u
trap '' PIPE
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
SERVER_LOG=/tmp/fixlab-e2e21-server.log
WS_LOG=/tmp/fixlab-e2e21-ws.log
FIFO=/tmp/fixlab-e2e21-cmd
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

stoch_put() { # json -> code|body
  http_code_body -X PUT "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/stochastic" \
    -H 'Content-Type: application/json' -d "$1"
}

stoch_metric() { # label -> count
  curl -s http://127.0.0.1:8080/api/v1/metrics | python3 -c "
import json,sys; print(json.load(sys.stdin)['stochastic_outcomes']['$1'])"
}

echo "== building =="
export PATH="$HOME/go/go/bin:$PATH"
(cd "$ROOT" && go build -o bin/fixlab-server ./backend/cmd/server \
              && go build -o bin/fixclient ./backend/cmd/fixclient \
              && go build -o bin/wsprobe ./backend/cmd/wsprobe) || exit 1

echo "== starting server =="
# The phase-7 per-IP session cap defaults to 1; this suite runs several
# concurrent sessions from 127.0.0.1, so raise it for the test server.
FIXLAB_HTTP_ADDR=127.0.0.1:8080 FIXLAB_MAX_SESSIONS_PER_IP=10 \
  "$BIN/fixlab-server" >"$SERVER_LOG" 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null; rm -f $FIFO' EXIT
sleep 2
curl -sf http://127.0.0.1:8080/healthz >/dev/null || { echo "server did not start"; cat "$SERVER_LOG"; exit 1; }
ok "server healthy"

new_session() { # cid -> sets TOKEN PORT
  local cid=$1 create
  create=$(curl -sf -X POST http://127.0.0.1:8080/api/v1/sessions \
    -H 'Content-Type: application/json' -d '{"targetCompId":"'"$cid"'"}') || return 1
  TOKEN=$(python3 -c "import json,sys; print(json.load(sys.stdin)['sessionToken'])" <<<"$create")
  PORT=$(python3 -c "import json,sys; print(json.load(sys.stdin)['endpoint']['port'])" <<<"$create")
  echo "   session $cid port=$PORT"
}

start_client() { # cid port logfile
  CLIENT_LOG=$3
  rm -f "$FIFO"; mkfifo "$FIFO"
  (cd "$ROOT" && "$BIN/fixclient" --host 127.0.0.1 --port "$2" \
    --sender "$1" --target FIXLAB --heartbt 30 --script \
    --dict backend/specs/FIX44.xml <"$FIFO" >"$CLIENT_LOG" 2>&1) &
  CLI=$!
  exec 3>"$FIFO"
  wait_for 'FIXCLIENT: LOGON_OK' "$CLIENT_LOG" 20
}

stop_client() {
  echo "LOGOUT" >&3
  exec 3>&-
  wait $CLI 2>/dev/null
  grep -q 'FIXCLIENT: LOGOUT_OK' "$CLIENT_LOG"
}

echo "== session A (main) =="
new_session "E2E21A_$$" || exit 1

echo "== ws probe =="
"$BIN/wsprobe" --url "ws://127.0.0.1:8080/ws/session/$TOKEN" --dur 600s >"$WS_LOG" 2>&1 &
WS=$!
sleep 1
grep -q 'WSPROBE: CONNECTED' "$WS_LOG" && ok "ws probe subscribed" || fail "ws probe"

echo "== GET /stochastic default (disabled) =="
SG=$(curl -s "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/stochastic")
echo "$SG" | python3 -c "import json,sys; d=json.load(sys.stdin); assert d['config']['enabled'] is False, d" \
  && ok "default policy disabled" || fail "default policy: $SG"
curl -s "http://127.0.0.1:8080/api/v1/sessions/$TOKEN" | python3 -c "
import json,sys; d=json.load(sys.stdin); assert 'stochastic' in d, 'missing stochastic in session GET'" \
  && ok "session GET carries stochastic state" || fail "session GET stochastic"

echo "== client A =="
start_client "E2E21A_$$" "$PORT" /tmp/fixlab-e2e21-client-a.log && ok "client logon" || { fail "client logon"; tail -5 "$CLIENT_LOG"; }

echo "== 1. 100/0/0 latency 0 -> immediate FULL_FILL, no API call =="
R=$(stoch_put '{"enabled":true,"acceptPct":100,"rejectPct":0,"partialFillPct":0,"avgLatencyMs":0,"stdDevMs":0}')
[[ "${R%%|*}" == "200" ]] && ok "policy 100/0/0 -> 200" || fail "stoch PUT -> $R"
BEFORE=$(wc -l <"$CLIENT_LOG")
echo "NOS S1A TEST 1 100 50.00 2" >&3
ER=$(new_er_fields "$BEFORE" 20) || fail "fill ER received"
check_fields "$ER" "150=F" "39=2" "11=S1A" "14=100" "151=0" "32=100" \
  && ok "client: stochastic fill ER 150=F/39=2" || fail "fill ER fields"
wait_order_status S1A FILLED 20 && ok "blotter: S1A FILLED" || fail "blotter S1A"
grep -q 'WSPROBE: ORDER_CREATED' "$WS_LOG" && ok "ws: ORDER_CREATED" || fail "ws ORDER_CREATED"
grep -q 'WSPROBE: ORDER_UPDATED' "$WS_LOG" && ok "ws: ORDER_UPDATED" || fail "ws ORDER_UPDATED"
grep -q 'WSPROBE: EXECUTION_SENT' "$WS_LOG" && ok "ws: EXECUTION_SENT" || fail "ws EXECUTION_SENT"
[[ "$(stoch_metric fill)" == "1" ]] && ok "metric: stochastic_outcomes.fill=1" || fail "stochastic fill metric"

echo "== 2. 0/100/0 -> REJECT =="
R=$(stoch_put '{"enabled":true,"acceptPct":0,"rejectPct":100,"partialFillPct":0,"avgLatencyMs":0,"stdDevMs":0}')
[[ "${R%%|*}" == "200" ]] && ok "policy 0/100/0 -> 200" || fail "stoch PUT -> $R"
BEFORE=$(wc -l <"$CLIENT_LOG")
echo "NOS S2A TEST 1 100 50.00 2" >&3
ER=$(new_er_fields "$BEFORE" 20) || fail "reject ER received"
check_fields "$ER" "150=8" "39=8" "11=S2A" "103=99" \
  && ok "client: stochastic reject ER 150=8/39=8 103=99" || fail "reject ER fields"
echo "$ER" | grep -q "58=stochastic" && ok "client: reject text names stochastic" || fail "reject text"
wait_order_status S2A REJECTED 20 && ok "blotter: S2A REJECTED" || fail "blotter S2A"
[[ "$(stoch_metric reject)" == "1" ]] && ok "metric: stochastic_outcomes.reject=1" || fail "stochastic reject metric"

echo "== 3. 0/0/100 -> PARTIAL_FILL, remainder stays working =="
R=$(stoch_put '{"enabled":true,"acceptPct":0,"rejectPct":0,"partialFillPct":100,"avgLatencyMs":0,"stdDevMs":0}')
[[ "${R%%|*}" == "200" ]] && ok "policy 0/0/100 -> 200" || fail "stoch PUT -> $R"
BEFORE=$(wc -l <"$CLIENT_LOG")
echo "NOS S3A TEST 1 1000 50.00 2" >&3
ER=$(new_er_fields "$BEFORE" 20) || fail "partial ER received"
check_fields "$ER" "150=D" "39=1" "11=S3A" \
  && ok "client: stochastic partial ER 150=D/39=1" || fail "partial ER fields"
LEAVES=$(echo "$ER" | python3 -c "
import re,sys
f=dict(x.split('=',1) for x in sys.stdin.read().split() if '=' in x)
lq=float(f['32']); lv=float(f['151'])
assert 100 <= lq <= 900, f'lastQty {lq} outside [10%,90%]'
assert lv == 1000 - lq, f'leaves {lv} != 1000 - {lq}'
print('ok')")
[[ "$LEAVES" == "ok" ]] && ok "partial qty in [10%,90%], leaves consistent" || fail "partial qty: $LEAVES"
wait_order_status S3A PARTIALLY_FILLED 20 && ok "blotter: S3A PARTIALLY_FILLED (working)" || fail "blotter S3A"
[[ "$(stoch_metric partial)" == "1" ]] && ok "metric: stochastic_outcomes.partial=1" || fail "stochastic partial metric"

echo "== 4. seeded determinism: 50/30/20 seed 7, 20 orders x2 sessions =="
run_twenty() { # prefix -> prints outcome sequence (150 per order)
  local pfx=$1 i id seq=""
  for i in $(seq -w 1 20); do
    id="${pfx}${i}"
    BEFORE=$(wc -l <"$CLIENT_LOG")
    echo "NOS $id TEST 1 100 50.00 2" >&3
    ER=$(new_er_fields "$BEFORE" 20) || { echo "ER_TIMEOUT:$id"; return 1; }
    # The ER is sent after the blotter update (execute() persists first),
    # so receipt proves the outcome; no status poll needed here.
    seq="$seq $(echo "$ER" | grep -oE '(^| )150=[A-Z0-9]+' | grep -oE '[A-Z0-9]+$')"
  done
  echo "$seq"
}
R=$(stoch_put '{"enabled":true,"acceptPct":50,"rejectPct":30,"partialFillPct":20,"avgLatencyMs":0,"stdDevMs":0,"seed":7}')
[[ "${R%%|*}" == "200" ]] && ok "seeded policy -> 200" || fail "seeded PUT -> $R"
SEQ_A=$(run_twenty "S4A") || fail "session A 20 orders"
echo "   seq A:$SEQ_A"
echo "$SEQ_A" | grep -q "F" && echo "$SEQ_A" | grep -q "8" && echo "$SEQ_A" | grep -q "D" \
  && ok "seed 7: all three outcomes occurred in 20 draws" || fail "outcome mix: $SEQ_A"
stop_client && ok "client A clean logout" || fail "client A logout"

echo "== session B (same seed) =="
new_session "E2E21B_$$" || exit 1
R=$(stoch_put '{"enabled":true,"acceptPct":50,"rejectPct":30,"partialFillPct":20,"avgLatencyMs":0,"stdDevMs":0,"seed":7}')
[[ "${R%%|*}" == "200" ]] && ok "session B seeded policy -> 200" || fail "session B PUT -> $R"
start_client "E2E21B_$$" "$PORT" /tmp/fixlab-e2e21-client-b.log && ok "client B logon" || fail "client B logon"
SEQ_B=$(run_twenty "S4B") || fail "session B 20 orders"
echo "   seq B:$SEQ_B"
[[ "$SEQ_A" == "$SEQ_B" ]] && ok "determinism: identical outcome sequences across sessions" \
  || fail "sequences differ: A=[$SEQ_A] B=[$SEQ_B]"
stop_client && ok "client B clean logout" || fail "client B logout"

echo "== back to session A for precedence/latency/validation/race =="
new_session "E2E21C_$$" || exit 1
start_client "E2E21C_$$" "$PORT" /tmp/fixlab-e2e21-client-c.log && ok "client C logon" || fail "client C logon"

echo "== 5. precedence: deterministic rule beats stochastic 100% accept =="
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/rules" \
  -H 'Content-Type: application/json' -d '{
    "name":"stoch-precedence","priority":10,"enabled":true,
    "predicates":[{"tag":55,"op":"eq","value":"MSFT"}],
    "actions":[{"type":"REJECT","ordRejReason":"99","text":"rule says no"}]}]}')
[[ "${R%%|*}" == "200" ]] && ok "precedence rule created" || fail "rule create -> $R"
F_BEFORE=$(stoch_metric fill)
R=$(stoch_put '{"enabled":true,"acceptPct":100,"rejectPct":0,"partialFillPct":0,"avgLatencyMs":0,"stdDevMs":0}')
[[ "${R%%|*}" == "200" ]] && ok "stochastic 100% accept armed" || fail "stoch PUT -> $R"
BEFORE=$(wc -l <"$CLIENT_LOG")
echo "NOS P5A MSFT 1 100 300.00 2" >&3
ER=$(new_er_fields "$BEFORE" 20) || fail "precedence ER received"
check_fields "$ER" "150=8" "39=8" "11=P5A" \
  && ok "rule REJECT wins over stochastic accept" || fail "precedence ER: $ER"
echo "$ER" | grep -q "58=rule says no" && ok "reject came from the rule" || fail "reject text: $ER"
[[ "$(stoch_metric fill)" == "$F_BEFORE" ]] && ok "stochastic drew nothing (rule fired first)" || fail "stochastic fired"

echo "== 5b. kill switch beats stochastic =="
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/killswitch" \
  -H 'Content-Type: application/json' -d '{"enabled":true}')
[[ "${R%%|*}" == "200" ]] && ok "kill switch on" || fail "killswitch -> $R"
BEFORE=$(wc -l <"$CLIENT_LOG")
echo "NOS P5B AAPL 1 100 150.00 2" >&3
ER=$(new_er_fields "$BEFORE" 20) || fail "halt ER received"
check_fields "$ER" "150=8" "39=8" "11=P5B" \
  && ok "kill switch rejects despite stochastic" || fail "halt ER: $ER"
echo "$ER" | grep -q "VENUE HALTED" && ok "58= names the venue halt" || fail "halt text: $ER"
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/killswitch" \
  -H 'Content-Type: application/json' -d '{"enabled":false}')
[[ "${R%%|*}" == "200" ]] && ok "kill switch off" || fail "killswitch off -> $R"

echo "== 6. latency: avg 300ms stddev 0 =="
R=$(stoch_put '{"enabled":true,"acceptPct":100,"rejectPct":0,"partialFillPct":0,"avgLatencyMs":300,"stdDevMs":0}')
[[ "${R%%|*}" == "200" ]] && ok "latency policy -> 200" || fail "latency PUT -> $R"
BEFORE=$(wc -l <"$CLIENT_LOG")
START_MS=$(date +%s%3N)
echo "NOS P6A TEST 1 100 50.00 2" >&3
ER=$(new_er_fields "$BEFORE" 20) || fail "delayed ER received"
END_MS=$(date +%s%3N)
ELAPSED=$((END_MS - START_MS))
check_fields "$ER" "150=F" "39=2" "11=P6A" && ok "delayed fill ER correct" || fail "delayed ER: $ER"
if [ "$ELAPSED" -ge 280 ]; then
  ok "latency measured ${ELAPSED}ms >= 280ms (target 300)"
else
  fail "latency measured ${ELAPSED}ms < 280ms"
fi

echo "== 6b. latency: avg 0 -> fast path =="
R=$(stoch_put '{"enabled":true,"acceptPct":100,"rejectPct":0,"partialFillPct":0,"avgLatencyMs":0,"stdDevMs":0}')
[[ "${R%%|*}" == "200" ]] && ok "zero-latency policy -> 200" || fail "zero latency PUT -> $R"
BEFORE=$(wc -l <"$CLIENT_LOG")
START_MS=$(date +%s%3N)
echo "NOS P6B TEST 1 100 50.00 2" >&3
ER=$(new_er_fields "$BEFORE" 20) || fail "fast ER received"
END_MS=$(date +%s%3N)
ELAPSED=$((END_MS - START_MS))
check_fields "$ER" "150=F" "39=2" "11=P6B" && ok "fast fill ER correct" || fail "fast ER: $ER"
if [ "$ELAPSED" -lt 2000 ]; then
  ok "fast path measured ${ELAPSED}ms < 2000ms"
else
  fail "fast path too slow: ${ELAPSED}ms"
fi

echo "== 7. invalid configs -> 400 =="
R=$(stoch_put '{"enabled":true,"acceptPct":50,"rejectPct":30,"partialFillPct":19}')
[[ "${R%%|*}" == "400" ]] && ok "sum 99 -> 400" || fail "sum 99 -> $R"
echo "${R#*|}" | grep -qi "sum to 100" && ok "400 names the sum problem" || fail "400 body: ${R#*|}"
R=$(stoch_put '{"enabled":true,"acceptPct":100,"avgLatencyMs":-5}')
[[ "${R%%|*}" == "400" ]] && ok "negative latency -> 400" || fail "neg latency -> $R"
R=$(stoch_put '{"enabled":true,"acceptPct":100,"rejectPct":5,"partialFillPct":-5}')
[[ "${R%%|*}" == "400" ]] && ok "negative pct -> 400" || fail "neg pct -> $R"

echo "== 8. manual race: 2000ms stochastic delay vs immediate manual FILL =="
R=$(stoch_put '{"enabled":true,"acceptPct":100,"rejectPct":0,"partialFillPct":0,"avgLatencyMs":2000,"stdDevMs":0}')
[[ "${R%%|*}" == "200" ]] && ok "delayed policy -> 200" || fail "race PUT -> $R"
BEFORE=$(wc -l <"$CLIENT_LOG")
echo "NOS P8A TEST 1 100 50.00 2" >&3
sleep 0.3  # let the NOS land; the stochastic outcome is still sleeping
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/orders/P8A/execute" \
  -H 'Content-Type: application/json' -d '{"action":"FILL","qty":100,"price":50}')
[[ "${R%%|*}" == "200" ]] && ok "manual FILL wins the race -> 200" || fail "manual fill -> $R"
wait_order_status P8A FILLED 20 && ok "blotter: P8A FILLED (manual)" || fail "blotter P8A"
sleep 3  # past the 2000ms stochastic delay
N_ER=$(grep -cE "ER_FIELDS.*11=P8A( |$)" "$CLIENT_LOG")
[[ "$N_ER" == "1" ]] && ok "exactly one terminal ER (no double-execution)" || fail "ER count for P8A: $N_ER"
CUM=$(order_json P8A | python3 -c "import json,sys; o=json.load(sys.stdin); print(o.get('cumQty'), o.get('leavesQty'))")
[[ "$CUM" == "100 0" ]] && ok "CumQty=100 LeavesQty=0" || fail "blotter qtys: $CUM"
grep -q 'stochastic: outcome stopped' "$SERVER_LOG" && ok "server logged the stopped stochastic outcome" \
  || fail "no 'outcome stopped' warning in server log"

echo "== GET /stochastic reflects outcomes =="
SG=$(curl -s "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/stochastic")
echo "$SG" | python3 -c "
import json,sys
d=json.load(sys.stdin); o=d['outcomes']
assert d['config']['enabled'] is True and o['fill'] >= 1, d" \
  && ok "GET /stochastic: config + outcome counts" || fail "GET /stochastic: $SG"

echo "== client C logout =="
stop_client && ok "client C clean logout" || fail "client C logout"
kill $WS 2>/dev/null

kill $SRV 2>/dev/null; trap - EXIT; rm -f "$FIFO"
echo
echo "RESULT: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
