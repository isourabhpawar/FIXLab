#!/bin/bash
# FixLab Phase 2.3 end-to-end acceptance test: scenario recording + replay
# (spec §48).
#
# With the REAL external QuickFIX/Go initiator (fixclient in script mode):
#   1. Session A: logon -> NOS (MSFT Buy 1000 @ 310.50) -> API PARTIAL_FILL
#      400 @ 310.50 -> API FILL 600 @ 310.60 -> logout.
#      GET /scenario has [logon, inbound D (fields), execution ACK_NEW,
#      execution PARTIAL_FILL, execution FILL, logout] in order, atMs
#      non-decreasing, and no session token anywhere in the JSON.
#   2. POST /sessions/replay {sourceToken: A, speed: 0} -> token B.
#      B's blotter: R1A FILLED, cumQty=1000, leavesQty=0, replayed=true.
#      B's /messages shows replayed FIX_MSG_IN (35=D) and replayed
#      FIX_MSG_OUT (35=8) records.
#   3. Hand-crafted 3-step scenario (4s atMs gap), replay speed=1, wsprobe
#      on the new session subscribed right after POST -> sees
#      ORDER_CREATED then SCENARIO_REPLAY_FINISHED, in that order.
#   4. Export A's scenario JSON, POST it back as {scenario} -> token C;
#      C's blotter reproduces B's (R1A FILLED, cumQty 1000).
#   5. Negatives: empty steps -> 400; unknown sourceToken -> 404;
#      initiator-role scenario -> 400 naming it; unknown kind -> 400;
#      speed -1 -> 400; sourceToken+scenario together -> 400.
#   6. MCP: tools/list has sandbox_get_scenario + sandbox_replay (8 tools);
#      sandbox_get_scenario on A returns the steps; sandbox_replay on A
#      yields a fresh token whose blotter reaches FILLED.
#
# Usage: ./backend/e2e_phase2_3.sh   (run from the repo root)
set -u
trap '' PIPE
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
HTTP="127.0.0.1:8080"
API="http://$HTTP/api/v1/sessions"
SERVER_LOG=/tmp/fixlab-e2e23-server.log
WS_LOG=/tmp/fixlab-e2e23-ws.log
FIFO=/tmp/fixlab-e2e23-cmd
PASS=0; FAIL=0

cleanup_stale() {
  for pat in '[b]in/fixlab-server' '[b]in/fixclient' '[b]in/wsprobe' '[b]in/mcp-server'; do
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

order_json() { # token clOrdId -> order json ({} if missing)
  curl -s "$API/$1/orders" | python3 -c "
import json,sys
ds=json.load(sys.stdin)['orders']
m={o['clOrdId']:o for o in ds}
print(json.dumps(m.get('$2', {})))"
}

wait_order_status() { # token clOrdId want_status [timeout]
  local tok=$1 id=$2 want=$3 timeout=${4:-30} i=0 st=""
  while [ "$st" != "$want" ]; do
    st=$(order_json "$tok" "$id" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status','MISSING'))")
    sleep 0.5; i=$((i+1))
    if [ "$i" -ge $((timeout*2)) ]; then
      echo "    wait_order_status timeout: $id last status [$st], want [$want]"
      return 1
    fi
  done
}

echo "== building =="
export PATH="$HOME/go/go/bin:$PATH"
(cd "$ROOT" && go build -o bin/fixlab-server ./backend/cmd/server \
              && go build -o bin/fixclient ./backend/cmd/fixclient \
              && go build -o bin/wsprobe ./backend/cmd/wsprobe \
              && go build -o bin/mcp-server ./backend/cmd/mcp-server) || exit 1
ok "binaries built"

echo "== starting server =="
FIXLAB_HTTP_ADDR=$HTTP FIXLAB_MAX_SESSIONS_PER_IP=10 \
  "$BIN/fixlab-server" >"$SERVER_LOG" 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null; rm -f $FIFO' EXIT
sleep 2
curl -sf "http://$HTTP/healthz" >/dev/null || { echo "server did not start"; cat "$SERVER_LOG"; exit 1; }
ok "server healthy"

new_session() { # cid -> sets TOKEN PORT
  local cid=$1 create
  create=$(curl -sf -X POST "$API" -H 'Content-Type: application/json' \
    -d '{"targetCompId":"'"$cid"'"}') || return 1
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

echo "== session A (record the story) =="
new_session "E2E23A_$$" || exit 1
TOKEN_A=$TOKEN
start_client "E2E23A_$$" "$PORT" /tmp/fixlab-e2e23-client-a.log && ok "client logon" || { fail "client logon"; exit 1; }

echo "== 1. live story: NOS -> partial -> fill -> logout =="
echo "NOS R1A MSFT 1 1000 310.50 2" >&3
sleep 2
wait_order_status "$TOKEN_A" R1A NEW 20 && ok "blotter: R1A NEW (ack)" || fail "R1A NEW"
R=$(http_code_body -X POST "$API/$TOKEN_A/orders/R1A/execute" -H 'Content-Type: application/json' \
  -d '{"action":"PARTIAL_FILL","qty":400,"price":310.50}')
[[ "${R%%|*}" == "200" ]] && ok "PARTIAL_FILL 400 @ 310.50 -> 200" || fail "partial fill: $R"
R=$(http_code_body -X POST "$API/$TOKEN_A/orders/R1A/execute" -H 'Content-Type: application/json' \
  -d '{"action":"FILL","qty":600,"price":310.60}')
[[ "${R%%|*}" == "200" ]] && ok "FILL 600 @ 310.60 -> 200" || fail "fill: $R"
wait_order_status "$TOKEN_A" R1A FILLED 20 && ok "blotter: R1A FILLED" || fail "R1A FILLED"
stop_client && ok "client logout" || fail "client logout"
sleep 1  # let the logout step land in the scenario log

echo "== GET /scenario =="
SC_JSON=$(curl -s "$API/$TOKEN_A/scenario")
echo "$SC_JSON" | python3 -c "
import json,sys
sc = json.load(sys.stdin)
kinds = [s['kind'] for s in sc['steps']]
assert kinds[0] == 'logon', kinds
assert 'inbound' in kinds and 'logout' in kinds, kinds
# the execution actions in order: auto-ack, partial, fill
acts = [s['payload']['action'] for s in sc['steps'] if s['kind'] == 'execution']
assert acts == ['ACK_NEW','PARTIAL_FILL','FILL'], acts
# inbound D carries the order fields
inb = [s for s in sc['steps'] if s['kind'] == 'inbound'][0]
assert inb['payload']['msgType'] == 'D', inb
f = inb['payload']['fields']
assert f['11'] == 'R1A' and f['55'] == 'MSFT' and f['38'] == '1000', f
# atMs non-decreasing, seq dense from 1
at = [s['atMs'] for s in sc['steps']]
assert all(b >= a for a, b in zip(at, at[1:])), at
assert [s['seq'] for s in sc['steps']] == list(range(1, len(at)+1))
assert sc['role'] == 'ACCEPTOR', sc['role']
print('steps:', len(at), 'kinds:', kinds)
" && ok "scenario: logon/inbound/ACK_NEW/PARTIAL_FILL/FILL/logout in order" || fail "scenario content: $SC_JSON"
echo "$SC_JSON" | grep -q 'fixlab_' && fail "scenario JSON leaks a token" || ok "scenario JSON contains no session token"

echo "== 2. replay into fresh session B (speed=0) =="
R=$(http_code_body -X POST "http://$HTTP/api/v1/sessions/replay" -H 'Content-Type: application/json' \
  -d '{"sourceToken":"'"$TOKEN_A"'","speed":0}')
[[ "${R%%|*}" == "201" ]] || { fail "replay -> $R"; exit 1; }
TOKEN_B=$(python3 -c "import json,sys; print(json.load(sys.stdin)['sessionToken'])" <<<"${R#*|}")
echo "   replay target B = ${TOKEN_B:0:16}…"
ok "replay accepted -> 201, new token"
wait_order_status "$TOKEN_B" R1A FILLED 30 && ok "B blotter: R1A FILLED" || fail "B blotter R1A"
order_json "$TOKEN_B" R1A | python3 -c "
import json,sys
o = json.load(sys.stdin)
assert o['cumQty'] == 1000, o
assert o['leavesQty'] == 0, o
assert abs(o['avgPx'] - 310.56) < 1e-9, o['avgPx']
assert o.get('replayed') is True, o
print('B order:', o['status'], o['cumQty'], o['leavesQty'], o['avgPx'])
" && ok "B order: cumQty=1000 leaves=0 avgPx=310.56 replayed=true" || fail "B order fields"
curl -s "$API/$TOKEN_B/messages?limit=50" | python3 -c "
import json,sys
ms = json.load(sys.stdin)['messages']
ins = [m for m in ms if m['direction'] == 'INBOUND' and m.get('replayed') and m['msgType'] == 'D']
outs = [m for m in ms if m['direction'] == 'OUTBOUND' and m.get('replayed') and m['msgType'] == '8']
assert ins, 'no replayed FIX_MSG_IN'
assert len(outs) >= 3, f'want >=3 replayed ERs, got {len(outs)}'
print('replayed msgs: in=%d out=%d' % (len(ins), len(outs)))
" && ok "B messages: replayed FIX_MSG_IN + replayed FIX_MSG_OUT ERs" || fail "B replayed messages"
curl -s "http://$HTTP/api/v1/sessions/$TOKEN_B" | python3 -c "
import json,sys
rp = json.load(sys.stdin).get('replay') or {}
assert rp.get('failed') is not True, rp
assert rp.get('doneSteps', 0) >= 5, rp
print('replay status:', rp)
" && ok "B session GET: replay finished, no failure" || fail "B replay status"

echo "== 3. timed replay with live WS observation =="
cat > /tmp/fixlab-e2e23-scen.json <<'EOF'
{"version":1,"role":"ACCEPTOR","beginString":"FIX.4.4","targetCompId":"WS2",
 "steps":[
  {"seq":1,"atMs":0,"kind":"logon","payload":{}},
  {"seq":2,"atMs":100,"kind":"inbound","payload":{"msgType":"D","fields":{"11":"W1","55":"TEST","54":"1","38":"100","40":"2","44":"50"}}},
  {"seq":3,"atMs":4100,"kind":"execution","payload":{"action":"FILL","clOrdId":"W1","qty":100,"price":50}}
 ]}
EOF
R=$(http_code_body -X POST "http://$HTTP/api/v1/sessions/replay" -H 'Content-Type: application/json' \
  -d '{"scenario":'"$(cat /tmp/fixlab-e2e23-scen.json)"',"speed":1}')
[[ "${R%%|*}" == "201" ]] || { fail "timed replay -> $R"; exit 1; }
TOKEN_D=$(python3 -c "import json,sys; print(json.load(sys.stdin)['sessionToken'])" <<<"${R#*|}")
"$BIN/wsprobe" --url "ws://$HTTP/ws/session/$TOKEN_D" --dur 25s >"$WS_LOG" 2>&1 &
WS=$!
sleep 1
grep -q 'WSPROBE: CONNECTED' "$WS_LOG" || { fail "wsprobe subscribe"; }
wait_for 'WSPROBE: ORDER_CREATED' "$WS_LOG" 10 && ok "ws: ORDER_CREATED during replay" || fail "ws ORDER_CREATED"
wait_for 'WSPROBE: SCENARIO_REPLAY_FINISHED' "$WS_LOG" 20 && ok "ws: SCENARIO_REPLAY_FINISHED" || fail "ws REPLAY_FINISHED"
wait $WS 2>/dev/null || true
python3 - <<'EOF' || fail "ws event order"
import re
log = open('/tmp/fixlab-e2e23-ws.log').read()
i1 = log.index('WSPROBE: ORDER_CREATED')
i2 = log.index('WSPROBE: SCENARIO_REPLAY_FINISHED')
assert i1 < i2, "ORDER_CREATED must precede REPLAY_FINISHED"
print("ws order ok")
EOF
ok "ws: ORDER_CREATED precedes SCENARIO_REPLAY_FINISHED"
wait_order_status "$TOKEN_D" W1 FILLED 20 && ok "D blotter: W1 FILLED after timed replay" || fail "D blotter W1"

echo "== 4. export round-trip: POST A's scenario back =="
R=$(http_code_body -X POST "http://$HTTP/api/v1/sessions/replay" -H 'Content-Type: application/json' \
  -d '{"scenario":'"$(curl -s "$API/$TOKEN_A/scenario")"',"speed":0}')
[[ "${R%%|*}" == "201" ]] || { fail "export replay -> $R"; exit 1; }
TOKEN_C=$(python3 -c "import json,sys; print(json.load(sys.stdin)['sessionToken'])" <<<"${R#*|}")
wait_order_status "$TOKEN_C" R1A FILLED 30 && ok "C blotter: R1A FILLED" || fail "C blotter R1A"
order_json "$TOKEN_C" R1A | python3 -c "
import json,sys
o = json.load(sys.stdin)
assert o['status'] == 'FILLED' and o['cumQty'] == 1000 and o['leavesQty'] == 0, o
assert abs(o['avgPx'] - 310.56) < 1e-9, o
print('C reproduces B exactly')
" && ok "exported scenario reproduces identically" || fail "C order fields"

echo "== 5. negatives =="
FAKE="fixlab_$(python3 -c "import secrets; print(secrets.token_hex(32))")"
R=$(http_code_body -X POST "http://$HTTP/api/v1/sessions/replay" -H 'Content-Type: application/json' \
  -d '{"sourceToken":"'"$FAKE"'"}')
[[ "${R%%|*}" == "404" ]] && ok "unknown sourceToken -> 404" || fail "unknown source: $R"
R=$(http_code_body -X POST "http://$HTTP/api/v1/sessions/replay" -H 'Content-Type: application/json' \
  -d '{"scenario":{"version":1,"role":"ACCEPTOR","steps":[]}}')
[[ "${R%%|*}" == "400" ]] && ok "empty steps -> 400" || fail "empty steps: $R"
R=$(http_code_body -X POST "http://$HTTP/api/v1/sessions/replay" -H 'Content-Type: application/json' \
  -d '{"scenario":{"version":1,"role":"ACCEPTOR","steps":[{"seq":1,"atMs":0,"kind":"bogus","payload":{}}]}}')
[[ "${R%%|*}" == "400" ]] && ok "unknown kind -> 400" || fail "unknown kind: $R"
R=$(http_code_body -X POST "http://$HTTP/api/v1/sessions/replay" -H 'Content-Type: application/json' \
  -d '{"scenario":{"version":1,"role":"INITIATOR","steps":[{"seq":1,"atMs":0,"kind":"logon","payload":{}}]}}')
[[ "${R%%|*}" == "400" ]] && echo "${R#*|}" | grep -qi "initiator" && ok "initiator scenario -> 400 naming it" || fail "initiator: $R"
R=$(http_code_body -X POST "http://$HTTP/api/v1/sessions/replay" -H 'Content-Type: application/json' \
  -d '{"sourceToken":"'"$TOKEN_A"'","speed":-1}')
[[ "${R%%|*}" == "400" ]] && ok "speed -1 -> 400" || fail "speed -1: $R"
R=$(http_code_body -X POST "http://$HTTP/api/v1/sessions/replay" -H 'Content-Type: application/json' \
  -d '{"sourceToken":"'"$TOKEN_A"'","scenario":{"version":1,"steps":[{"seq":1,"atMs":0,"kind":"logon","payload":{}}]}}')
[[ "${R%%|*}" == "400" ]] && ok "sourceToken+scenario together -> 400" || fail "both: $R"

echo "== 6. MCP tools =="
cat > /tmp/fixlab-e2e23-mcp-steps.json <<EOF
[{"tool":"sandbox_get_scenario","args":{"token":"$TOKEN_A"}},
 {"tool":"sandbox_replay","args":{"token":"$TOKEN_A","speed":0}}]
EOF
(cd "$ROOT" && FIXLAB_API_URL="http://$HTTP" python3 backend/mcp_e2e_driver.py /tmp/fixlab-e2e23-mcp-steps.json) > /tmp/fixlab-e2e23-mcp.json 2>/tmp/fixlab-e2e23-mcp.err || { fail "mcp driver"; cat /tmp/fixlab-e2e23-mcp.err; }
python3 - <<'EOF' || fail "mcp tools"
import json
out = json.load(open('/tmp/fixlab-e2e23-mcp.json'))
tools = out[0]['tools']
assert 'sandbox_get_scenario' in tools and 'sandbox_replay' in tools, tools
assert len(tools) == 8, tools
sc = out[1]
assert not sc['isError'], sc['text'][:200]
doc = json.loads(sc['text'])
assert any(s['kind'] == 'inbound' for s in doc['steps']), doc
rp = out[2]
assert not rp['isError'], rp['text'][:200]
new_token = json.loads(rp['text'])['sessionToken']
assert new_token.startswith('fixlab_'), new_token
open('/tmp/fixlab-e2e23-mcp-token', 'w').write(new_token)
print('mcp tools ok:', tools)
EOF
ok "mcp: 8 tools incl. sandbox_get_scenario + sandbox_replay"
MCP_TOKEN=$(cat /tmp/fixlab-e2e23-mcp-token)
wait_order_status "$MCP_TOKEN" R1A FILLED 30 && ok "mcp replay: blotter FILLED" || fail "mcp replay blotter"

echo
echo "== results: $PASS passed, $FAIL failed =="
[ "$FAIL" -eq 0 ]
