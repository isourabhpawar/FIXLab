#!/bin/bash
# FixLab Phase 5 end-to-end acceptance test: initiator mode + SSRF
# protection (spec §8, §9).
#
# Part A (FIXLAB_TEST_ALLOW_PRIVATE=true): against the real scripted
#   QuickFIX/Go acceptor (fixacceptor):
#   1. create INITIATOR session -> 201, CONNECTING
#   2. poll GET until LOGON_ACCEPTED; WS shows
#      CONNECTING -> TCP_CONNECTED -> LOGON_SENT -> LOGON_ACCEPTED
#   3. send-order 35=D -> acceptor logs NOS_RX, blotter OUTBOUND/NEW,
#      remote fill ER arrives as FIX_MSG_IN, blotter -> FILLED
#   4. send-order 35=F and 35=G against HOLD* working orders ->
#      acceptor logs CANCEL_RX/REPLACE_RX, blotter -> CANCELED/REPLACED
#   5. negatives: send-order on CONNECTING session -> 400; bad ports ->
#      400; TLS initiator create -> 201 (handshake proven in e2e_phase7);
#      duplicate ClOrdID -> 409; cancel on
#      FILLED order -> 409; send-order on acceptor session -> 400;
#      unknown msgType -> 400; dial to closed port -> CONNECTION_FAILED
# Part B (override OFF): SSRF battery -> 127.0.0.1, 10.1.2.3,
#   192.168.1.1, 169.254.169.254, localhost, metadata.google.internal,
#   and a hostname resolving to a private IP are ALL rejected 400
#   naming the violation.
# Part C (override ON): 127.0.0.1 allowed again (documented test-only).
#
# Usage: ./backend/e2e_phase5.sh   (run from the repo root)
set -u
trap '' PIPE
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
CID="E2E5_$$"
HTTP="127.0.0.1:8081"
API="http://$HTTP/api/v1/sessions"
SERVER_LOG=/tmp/fixlab-e2e5-server.log
ACCEPTOR_LOG=/tmp/fixlab-e2e5-acceptor.log
WS_LOG=/tmp/fixlab-e2e5-ws.log
PASS=0; FAIL=0
ACC_PORT=11981
export PATH="$HOME/go/go/bin:$PATH"

cleanup_stale() {
  for pat in '[b]in/fixlab-server' '[b]in/fixacceptor' '[b]in/wsprobe'; do
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
      echo "    wait_for timeout after ${timeout}s for [$pat] in $log"
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

session_status() { # token -> status
  curl -s "$API/$1" | python3 -c "import json,sys; print(json.load(sys.stdin)['status'])"
}

wait_status() { # token want [timeout]
  local tok=$1 want=$2 timeout=${3:-25} i=0 st=""
  while [ "$st" != "$want" ]; do
    st=$(session_status "$tok" 2>/dev/null || echo "?")
    sleep 0.5; i=$((i+1))
    if [ "$i" -ge $((timeout*2)) ]; then
      echo "    wait_status timeout: last [$st], want [$want]"
      return 1
    fi
  done
}

order_json() { # token clOrdId -> order json ({} if missing)
  curl -s "$API/$1/orders" | python3 -c "
import json,sys
ds=json.load(sys.stdin)['orders']
m={o['clOrdId']:o for o in ds}
print(json.dumps(m.get('$2', {})))"
}

wait_order_status() { # token clOrdId want [timeout]
  local tok=$1 id=$2 want=$3 timeout=${4:-20} i=0 st=""
  while [ "$st" != "$want" ]; do
    st=$(order_json "$tok" "$id" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status','MISSING'))")
    sleep 0.5; i=$((i+1))
    if [ "$i" -ge $((timeout*2)) ]; then
      echo "    wait_order_status timeout: $id last [$st], want [$want]"
      return 1
    fi
  done
}

echo "== building =="
(cd "$ROOT" && go build -o bin/fixlab-server ./backend/cmd/server \
              && go build -o bin/fixacceptor ./backend/cmd/fixacceptor \
              && go build -o bin/wsprobe ./backend/cmd/wsprobe) || exit 1

start_server() { # extra env assignments passed as $1
  # shellcheck disable=SC2086
  env $1 FIXLAB_HTTP_ADDR=$HTTP FIXLAB_INITIATOR_CONNECT_TIMEOUT=8s \
    "$BIN/fixlab-server" >"$SERVER_LOG" 2>&1 &
  SRV=$!
  sleep 2
  curl -sf "http://$HTTP/healthz" >/dev/null || { echo "server did not start"; cat "$SERVER_LOG"; exit 1; }
}
stop_server() { kill $SRV 2>/dev/null; sleep 1; }

echo "== Part A: initiator happy path (test override ON) =="
start_server "FIXLAB_TEST_ALLOW_PRIVATE=true FIXLAB_MAX_SESSIONS_PER_IP=50"
ok "server healthy (override on)"

"$BIN/fixacceptor" --port $ACC_PORT --sender REMOTE --target FIXLAB >"$ACCEPTOR_LOG" 2>&1 &
ACC=$!
wait_for "ACCEPTOR: listening" "$ACCEPTOR_LOG" 10 || { echo "acceptor did not start"; cat "$ACCEPTOR_LOG"; exit 1; }
ok "scripted counterparty listening"

echo "== A1. create INITIATOR session =="
CREATE=$(curl -sf -X POST "$API" -H 'Content-Type: application/json' \
  -d '{"role":"INITIATOR","remoteHost":"127.0.0.1","remotePort":"'$ACC_PORT'","remoteCompId":"REMOTE","localSenderCompId":"FIXLAB"}') || exit 1
TOKEN=$(python3 -c "import json,sys; print(json.load(sys.stdin)['sessionToken'])" <<<"$CREATE")
ROLE=$(python3 -c "import json,sys; print(json.load(sys.stdin)['role'])" <<<"$CREATE")
RIP=$(python3 -c "import json,sys; print(json.load(sys.stdin).get('remoteIp',''))" <<<"$CREATE")
[[ "$ROLE" == "INITIATOR" ]] && ok "role INITIATOR" || fail "role -> $ROLE"
[[ "$RIP" == "127.0.0.1" ]] && ok "pinned dial IP reported (remoteIp=127.0.0.1)" || fail "remoteIp -> $RIP"

echo "== A2. WS status chain =="
"$BIN/wsprobe" --url "ws://$HTTP/ws/session/$TOKEN" --dur 120s >"$WS_LOG" 2>&1 &
WS=$!
sleep 1
wait_status "$TOKEN" "LOGON_ACCEPTED" 25 && ok "session LOGON_ACCEPTED" || fail "no LOGON_ACCEPTED"
wait_for "ACCEPTOR: LOGON_OK" "$ACCEPTOR_LOG" 10 && ok "counterparty saw logon" || fail "acceptor logon"
# Subsequence check on the WS stream: CONNECTING -> TCP_CONNECTED -> LOGON_SENT -> LOGON_ACCEPTED
python3 - "$WS_LOG" <<'EOF'
import re, sys
seq = []
for line in open(sys.argv[1]):
    m = re.search(r'WSPROBE: CONNECTION_STATUS status=(\S+)', line)
    if m: seq.append(m.group(1))
want = ["CONNECTING", "TCP_CONNECTED", "LOGON_SENT", "LOGON_ACCEPTED"]
i = 0
for s in seq:
    if i < len(want) and s == want[i]: i += 1
sys.exit(0 if i == len(want) else 1)
EOF
[[ $? -eq 0 ]] && ok "WS chain CONNECTING -> TCP_CONNECTED -> LOGON_SENT -> LOGON_ACCEPTED" \
  || { fail "WS chain wrong: $(grep -o 'status=[A-Z_]*' "$WS_LOG" | tr '\n' ' ')"; }

echo "== A3. inject 35=D, remote fill drives blotter =="
R=$(http_code_body -X POST "$API/$TOKEN/send-order" -H 'Content-Type: application/json' \
  -d '{"msgType":"D","fields":{"11":"E2E5-1","55":"TEST","54":"1","38":"100","40":"2","44":"50"}}')
[[ "${R%%|*}" == "200" ]] && ok "send-order D -> 200" || fail "send-order D -> $R"
wait_for "ACCEPTOR: NOS_RX 11=E2E5-1" "$ACCEPTOR_LOG" 10 && ok "counterparty received 35=D" || fail "no NOS_RX"
wait_order_status "$TOKEN" "E2E5-1" "FILLED" 20 && ok "blotter E2E5-1 -> FILLED via remote ER" || fail "no FILLED"
OJ=$(order_json "$TOKEN" "E2E5-1")
python3 -c "
import json,sys; o=json.load(sys.stdin)
assert o['direction']=='OUTBOUND', o
assert o['cumQty']==100 and o['leavesQty']==0 and o['avgPx']==50, o
" <<<"$OJ" && ok "blotter OUTBOUND, cum/leaves/avgPx from remote ER" || fail "blotter fields: $OJ"
grep -q "WSPROBE: FIX_MSG_IN" "$WS_LOG" && ok "WS streamed FIX_MSG_IN (remote ERs)" || fail "no FIX_MSG_IN on WS"

echo "== A4. inject 35=F cancel =="
R=$(http_code_body -X POST "$API/$TOKEN/send-order" -H 'Content-Type: application/json' \
  -d '{"msgType":"D","fields":{"11":"E2E5-H1","55":"HOLD1","54":"1","38":"50","40":"2","44":"51"}}')
[[ "${R%%|*}" == "200" ]] && ok "HOLD order injected" || fail "HOLD inject -> $R"
wait_order_status "$TOKEN" "E2E5-H1" "NEW" 20 && ok "HOLD1 stays NEW (ack only)" || fail "HOLD1 status"
R=$(http_code_body -X POST "$API/$TOKEN/send-order" -H 'Content-Type: application/json' \
  -d '{"msgType":"F","fields":{"11":"E2E5-C1","41":"E2E5-H1","55":"HOLD1","54":"1","38":"50"}}')
[[ "${R%%|*}" == "200" ]] && ok "send-order F -> 200" || fail "send-order F -> $R"
wait_for "ACCEPTOR: CANCEL_RX 11=E2E5-C1 41=E2E5-H1" "$ACCEPTOR_LOG" 10 && ok "counterparty received 35=F" || fail "no CANCEL_RX"
wait_order_status "$TOKEN" "E2E5-H1" "CANCELED" 20 && ok "blotter E2E5-H1 -> CANCELED via remote ER" || fail "no CANCELED"

echo "== A5. inject 35=G replace =="
R=$(http_code_body -X POST "$API/$TOKEN/send-order" -H 'Content-Type: application/json' \
  -d '{"msgType":"D","fields":{"11":"E2E5-H2","55":"HOLD2","54":"1","38":"60","40":"2","44":"52"}}')
wait_order_status "$TOKEN" "E2E5-H2" "NEW" 20 || fail "HOLD2 NEW"
R=$(http_code_body -X POST "$API/$TOKEN/send-order" -H 'Content-Type: application/json' \
  -d '{"msgType":"G","fields":{"11":"E2E5-R1","41":"E2E5-H2","55":"HOLD2","54":"1","38":"120","40":"2","44":"53"}}')
[[ "${R%%|*}" == "200" ]] && ok "send-order G -> 200" || fail "send-order G -> $R"
wait_for "ACCEPTOR: REPLACE_RX 11=E2E5-R1 41=E2E5-H2" "$ACCEPTOR_LOG" 10 && ok "counterparty received 35=G" || fail "no REPLACE_RX"
wait_order_status "$TOKEN" "E2E5-H2" "REPLACED" 20 && ok "blotter E2E5-H2 -> REPLACED via remote ER" || fail "no REPLACED"
OJ=$(order_json "$TOKEN" "E2E5-H2")
python3 -c "
import json,sys; o=json.load(sys.stdin)
assert o['orderQty']==120 and o['price']==53, o
" <<<"$OJ" && ok "replace terms applied (qty 120 @ 53)" || fail "replace terms: $OJ"

echo "== A6. negatives =="
R=$(http_code_body -X POST "$API/$TOKEN/send-order" -H 'Content-Type: application/json' \
  -d '{"msgType":"D","fields":{"11":"E2E5-1","55":"TEST","54":"1","38":"10","40":"2"}}')
[[ "${R%%|*}" == "409" ]] && ok "duplicate ClOrdID -> 409" || fail "dup -> $R"
R=$(http_code_body -X POST "$API/$TOKEN/send-order" -H 'Content-Type: application/json' \
  -d '{"msgType":"F","fields":{"11":"E2E5-C2","41":"E2E5-1","55":"TEST","54":"1","38":"100"}}')
[[ "${R%%|*}" == "409" ]] && ok "cancel on FILLED order -> 409" || fail "cancel filled -> $R"
R=$(http_code_body -X POST "$API/$TOKEN/send-order" -H 'Content-Type: application/json' \
  -d '{"msgType":"X","fields":{"11":"E2E5-X"}}')
[[ "${R%%|*}" == "400" ]] && ok "unknown msgType -> 400" || fail "msgType X -> $R"
R=$(http_code_body -X POST "$API/$TOKEN/send-order" -H 'Content-Type: application/json' \
  -d '{"msgType":"D","fields":{"11":"E2E5-N1","55":"TEST","54":"1","38":"10"}}')
[[ "${R%%|*}" == "400" ]] && ok "missing required field (40) -> 400" || fail "missing 40 -> $R"
R=$(http_code_body -X POST "$API/$TOKEN/send-order" -H 'Content-Type: application/json' \
  -d '{"msgType":"D","fields":{"11":"E2E5-N2","55":"TEST","54":"1","38":"10","40":"2","49":"EVIL"}}')
[[ "${R%%|*}" == "400" ]] && ok "engine-stamped tag 49 rejected -> 400" || fail "tag 49 -> $R"
# acceptor session: send-order not available
ACCEPTOR_SESS=$(curl -sf -X POST "$API" -H 'Content-Type: application/json' -d '{"targetCompId":"E2E5ACC"}')
ATOKEN=$(python3 -c "import json,sys; print(json.load(sys.stdin)['sessionToken'])" <<<"$ACCEPTOR_SESS")
R=$(http_code_body -X POST "$API/$ATOKEN/send-order" -H 'Content-Type: application/json' \
  -d '{"msgType":"D","fields":{"11":"E2E5-A1","55":"TEST","54":"1","38":"10","40":"2"}}')
[[ "${R%%|*}" == "400" ]] && ok "send-order on ACCEPTOR session -> 400" || fail "acceptor send-order -> $R"
# TLS requested -> accepted in phase 7 (201; the full TLS handshake is
# proven in e2e_phase7.sh against a TLS counterparty)
CREATE_TLS=$(curl -sf -X POST "$API" -H 'Content-Type: application/json' \
  -d '{"role":"INITIATOR","transport":"TLS","tls":true,"remoteHost":"127.0.0.1","remotePort":"'$ACC_PORT'","remoteCompId":"REMOTE","localSenderCompId":"FIXLABTLS"}') \
  && ok "TLS initiator create -> 201" || fail "TLS initiator create"
TOKENTLS=$(python3 -c "import json,sys; print(json.load(sys.stdin)['sessionToken'])" <<<"$CREATE_TLS")
EPTLS=$(python3 -c "import json,sys; print(json.load(sys.stdin)['endpoint']['tls'])" <<<"$CREATE_TLS")
[[ "$EPTLS" == "True" ]] && ok "initiator endpoint.tls=true" || fail "endpoint.tls=$EPTLS"
curl -sf -X DELETE "$API/$TOKENTLS" >/dev/null && ok "TLS initiator session deleted" || fail "TLS initiator delete"
# bad ports
for p in "abc" "0" "99999" "-1"; do
  R=$(http_code_body -X POST "$API" -H 'Content-Type: application/json' \
    -d '{"role":"INITIATOR","remoteHost":"127.0.0.1","remotePort":"'$p'","remoteCompId":"REMOTE"}')
  [[ "${R%%|*}" == "400" ]] && ok "bad port [$p] -> 400" || fail "port $p -> $R"
done
# non-connected session: dial a closed port, send-order -> 400, then CONNECTION_FAILED
CREATE2=$(curl -sf -X POST "$API" -H 'Content-Type: application/json' \
  -d '{"role":"INITIATOR","remoteHost":"127.0.0.1","remotePort":"11999","remoteCompId":"REMOTE2","localSenderCompId":"FIXLAB2"}') || fail "closed-port session create"
TOKEN2=$(python3 -c "import json,sys; print(json.load(sys.stdin)['sessionToken'])" <<<"$CREATE2")
R=$(http_code_body -X POST "$API/$TOKEN2/send-order" -H 'Content-Type: application/json' \
  -d '{"msgType":"D","fields":{"11":"E2E5-NC1","55":"TEST","54":"1","38":"10","40":"2"}}')
[[ "${R%%|*}" == "400" && "${R#*|}" == *"not connected"* ]] && ok "send-order while CONNECTING -> 400" || fail "not-connected -> $R"
wait_status "$TOKEN2" "CONNECTION_FAILED" 20 && ok "closed port -> CONNECTION_FAILED" || fail "no CONNECTION_FAILED"
ST2=$(session_status "$TOKEN2")
[[ "$ST2" == "CONNECTION_FAILED" ]] && ok "failure is terminal (no redial storm)" || fail "status -> $ST2"

kill $WS 2>/dev/null; kill $ACC 2>/dev/null; stop_server

echo
echo "== Part B: SSRF battery (override OFF) =="
start_server "FIXLAB_MAX_SESSIONS_PER_IP=50"
ok "server healthy (override off)"
ssrf_case() { # host port want_fragment
  local host=$1 port=$2 want=$3
  local R
  R=$(http_code_body -X POST "$API" -H 'Content-Type: application/json' \
    -d '{"role":"INITIATOR","remoteHost":"'"$host"'","remotePort":"'"$port"'","remoteCompId":"REMOTE"}')
  if [[ "${R%%|*}" == "400" && "${R#*|}" == *"$want"* ]]; then
    ok "SSRF [$host:$port] -> 400 ($want)"
  else
    fail "SSRF [$host:$port] -> $R (want 400 containing [$want])"
  fi
}
ssrf_case "127.0.0.1" "9878" "blocked range 127.0.0.0/8"
ssrf_case "10.1.2.3" "9878" "blocked range 10.0.0.0/8"
ssrf_case "192.168.1.1" "9878" "blocked range 192.168.0.0/16"
ssrf_case "169.254.169.254" "80" "blocked range 169.254.0.0/16"
ssrf_case "localhost" "9878" "blocked"
ssrf_case "metadata.google.internal" "443" "blocked"
ssrf_case "169.254.169.254" "abc" "not numeric"
# hostname resolving to a private IP via /etc/hosts (when writable)
if [ -w /etc/hosts ]; then
  grep -q "ssrf-e2e-test" /etc/hosts || echo "10.9.9.9 ssrf-e2e-test.invalid" >> /etc/hosts
  R=$(http_code_body -X POST "$API" -H 'Content-Type: application/json' \
    -d '{"role":"INITIATOR","remoteHost":"ssrf-e2e-test.invalid","remotePort":"9878","remoteCompId":"REMOTE"}')
  if [[ "${R%%|*}" == "400" && "${R#*|}" == *"10.9.9.9"* && "${R#*|}" == *"blocked range"* ]]; then
    ok "SSRF [hostname -> 10.9.9.9] -> 400 naming the IP"
  else
    fail "SSRF [hostname -> private IP] -> $R"
  fi
  sed -i '/ssrf-e2e-test/d' /etc/hosts
else
  echo "  SKIP: /etc/hosts not writable (multi-IP path covered by unit tests)"
fi
stop_server

echo
echo "== Part C: override ON allows 127.0.0.1 (documented test-only) =="
start_server "FIXLAB_TEST_ALLOW_PRIVATE=true FIXLAB_MAX_SESSIONS_PER_IP=50"
R=$(http_code_body -X POST "$API" -H 'Content-Type: application/json' \
  -d '{"role":"INITIATOR","remoteHost":"127.0.0.1","remotePort":"11998","remoteCompId":"R","localSenderCompId":"F"}')
[[ "${R%%|*}" == "201" ]] && ok "127.0.0.1 allowed with override -> 201" || fail "override -> $R"
stop_server
cleanup_stale

echo
echo "RESULT: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
