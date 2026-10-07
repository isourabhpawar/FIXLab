#!/bin/bash
# FixLab Phase 2.2 end-to-end acceptance test: MCP server (spec §49).
#
# Drives bin/mcp-server over stdio with a scripted JSON-RPC client
# (backend/mcp_e2e_driver.py) against the real REST API + FIX engine:
#   1. initialize handshake -> protocol version + exactly the six tools
#   2. sandbox_create (ACCEPTOR, FIX.4.4) -> token + endpoint + connect_howto
#   3. real fixclient connects to that endpoint, logon
#   4. client sends NOS -> sandbox_list_messages shows it with parsed fields
#   5. sandbox_send_execution PARTIAL_FILL then FILL -> client receives ERs;
#      sandbox_get_status shows the order FILLED
#   6. sandbox_send_raw (35=0 heartbeat) -> appears as OUT in list_messages
#   7. error paths: bad token, unknown order, send_order on acceptor
#   8. INITIATOR flow: sandbox_create INITIATOR against fixacceptor ->
#      sandbox_send_order D -> acceptor logs NOS_RX
#
# Usage: ./backend/e2e_phase2_2.sh   (run from the repo root)
set -u
trap '' PIPE
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
CID="E2EMCP_$$"
HTTP="127.0.0.1:8082"
API="http://$HTTP/api/v1/sessions"
SERVER_LOG=/tmp/fixlab-e2e22-server.log
CLIENT_LOG=/tmp/fixlab-e2e22-client.log
ACCEPTOR_LOG=/tmp/fixlab-e2e22-acceptor.log
DRIVER_OUT=/tmp/fixlab-e2e22-driver.json
ACC_PORT=11971
export PATH="$HOME/go/go/bin:$PATH"

cleanup_stale() {
  for pat in '[b]in/fixlab-server' '[b]in/fixacceptor' '[b]in/mcp-server'; do
    for pid in $(ps -eo pid,args | grep "$pat" | awk '{print $1}'); do
      [ "$pid" != "$$" ] && kill "$pid" 2>/dev/null || true
    done
  done
  sleep 1
}
cleanup_stale

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL+1)); echo "  FAIL: $1"; }

need() { command -v "$1" >/dev/null || { echo "missing: $1"; exit 2; }; }
need curl; need python3

wait_for() { # pattern logfile [timeout_s]
  local pat=$1 log=$2 timeout=${3:-20} i=0
  while ! grep -q "$pat" "$log" 2>/dev/null; do
    sleep 0.5; i=$((i+1))
    if [ "$i" -ge $((timeout*2)) ]; then
      echo "  TIMEOUT waiting for '$pat' in $log"; tail -5 "$log" 2>/dev/null; return 1
    fi
  done
  return 0
}

# --- build ---
echo "== build =="
(cd "$ROOT" && go build -o bin/fixlab-server ./backend/cmd/server \
              && go build -o bin/fixclient ./backend/cmd/fixclient \
              && go build -o bin/mcp-server ./backend/cmd/mcp-server \
              && go build -o bin/fixacceptor ./backend/cmd/fixacceptor) \
  || { echo "build failed"; exit 1; }
ok "binaries built"

# --- start server ---
echo "== start fixlab-server =="
env FIXLAB_HTTP_ADDR=$HTTP FIXLAB_TEST_ALLOW_PRIVATE=true FIXLAB_MAX_SESSIONS_PER_IP=50 \
  "$BIN/fixlab-server" >"$SERVER_LOG" 2>&1 &
SRV=$!
for i in $(seq 1 20); do
  curl -sf "http://$HTTP/healthz" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -sf "http://$HTTP/healthz" >/dev/null || { echo "server did not start"; tail -20 "$SERVER_LOG"; exit 1; }
ok "server up on $HTTP"

export FIXLAB_API_URL="http://$HTTP"

# mcp_call <steps.json> -> prints driver results JSON to $DRIVER_OUT
mcp_call() {
  (cd "$ROOT" && python3 backend/mcp_e2e_driver.py "$1" >"$DRIVER_OUT") || { echo "driver failed"; exit 1; }
}
# result <index> <jq-expr> — index 0 is the handshake; first tool call is 1.
# For ['text'] the raw text is printed (no JSON escaping); otherwise the
# value is printed as JSON.
result() {
  if [ "$2" = "['text']" ]; then
    python3 -c "import json,sys; d=json.load(open('$DRIVER_OUT')); sys.stdout.write(d[$1]['text'])"
  else
    python3 -c "
import json,sys
d=json.load(open('$DRIVER_OUT'))
print(json.dumps(d[$1]$2))" 2>/dev/null
  fi
}

echo "== 1. handshake: exactly six tools =="
cat > /tmp/e2e22-steps.json <<'EOF'
[]
EOF
mcp_call /tmp/e2e22-steps.json
TOOLS=$(result 0 "['tools']")
echo "$TOOLS" | grep -q sandbox_create && echo "$TOOLS" | grep -q sandbox_get_status \
  && echo "$TOOLS" | grep -q sandbox_list_messages && echo "$TOOLS" | grep -q sandbox_send_order \
  && echo "$TOOLS" | grep -q sandbox_send_execution && echo "$TOOLS" | grep -q sandbox_send_raw \
  && ok "six tools present" || fail "tool list: $TOOLS"
NTOOLS=$(python3 -c "import json;print(len(json.load(open('$DRIVER_OUT'))[0]['tools']))")
[ "$NTOOLS" = "8" ] && ok "exactly 8 tools (6 spec + get_scenario + replay)" || fail "tool count: $NTOOLS"

echo "== 2. sandbox_create ACCEPTOR =="
cat > /tmp/e2e22-steps.json <<'EOF'
[{"tool":"sandbox_create","args":{"role":"ACCEPTOR","beginString":"FIX.4.4","targetCompId":"MCPCLIENT"}}]
EOF
mcp_call /tmp/e2e22-steps.json
[ "$(result 1 "['isError']")" = "false" ] && ok "sandbox_create ok" || fail "sandbox_create: $(result 1 "['text']")"
TOKEN=$(result 1 "['structured']['sessionToken']" | tr -d '"')
PORT=$(result 1 "['structured']['endpoint']['port']")
[[ "$TOKEN" == fixlab_* ]] && ok "token shape" || fail "token: $TOKEN"
[ -n "$PORT" ] && [ "$PORT" -gt 0 ] && ok "endpoint port $PORT" || fail "port: $PORT"
result 1 "['structured']['connect_howto']" | grep -q "FIX engine" && ok "connect_howto present" \
  || fail "connect_howto missing"

echo "== 3. real FIX client connects =="
rm -f /tmp/e2e22.fifo; mkfifo /tmp/e2e22.fifo
(cd "$ROOT" && "$BIN/fixclient" --host 127.0.0.1 --port "$PORT" \
  --sender MCPCLIENT --target FIXLAB --heartbt 30 --script \
  --dict backend/specs/FIX44.xml </tmp/e2e22.fifo >"$CLIENT_LOG" 2>&1) &
CLI=$!
exec 3>/tmp/e2e22.fifo
if wait_for 'FIXCLIENT: LOGON_OK' "$CLIENT_LOG" 20; then ok "client logon"; else fail "client logon"; fi

echo "== 4. NOS via client -> sandbox_list_messages =="
BEFORE=$(wc -l <"$CLIENT_LOG")
echo "NOS MCPORD1 MSFT 1 100 300.00 2" >&3
sleep 2
cat > /tmp/e2e22-steps.json <<EOF
[{"tool":"sandbox_list_messages","args":{"token":"$TOKEN","limit":10,"direction":"IN"}}]
EOF
mcp_call /tmp/e2e22-steps.json
[ "$(result 1 "['isError']")" = "false" ] && ok "list_messages ok" || fail "list_messages: $(result 1 "['text']")"
MSGS=$(result 1 "['text']")
echo "$MSGS" | grep -q "MCPORD1" && ok "NOS visible with ClOrdID" || fail "NOS not in messages"
echo "$MSGS" | grep -q '"msgType": "D"' && ok "msgType D parsed" || fail "msgType parse"
echo "$MSGS" | grep -q '"enumDescription": "BUY"' && ok "enum BUY parsed" || fail "enum parse"

echo "== 5. sandbox_send_execution PARTIAL_FILL + FILL =="
cat > /tmp/e2e22-steps.json <<EOF
[{"tool":"sandbox_send_execution","args":{"token":"$TOKEN","clOrdId":"MCPORD1","action":"PARTIAL_FILL","params":{"qty":40,"price":300.00}}}]
EOF
mcp_call /tmp/e2e22-steps.json
[ "$(result 1 "['isError']")" = "false" ] && ok "PARTIAL_FILL ok" || fail "PARTIAL_FILL: $(result 1 "['text']")"
sleep 1
wait_for 'ER_RX' "$CLIENT_LOG" 10 && ok "client got partial ER" || fail "no partial ER"
cat > /tmp/e2e22-steps.json <<EOF
[{"tool":"sandbox_send_execution","args":{"token":"$TOKEN","clOrdId":"MCPORD1","action":"FILL","params":{"qty":60,"price":300.10}}}]
EOF
mcp_call /tmp/e2e22-steps.json
[ "$(result 1 "['isError']")" = "false" ] && ok "FILL ok" || fail "FILL: $(result 1 "['text']")"
sleep 1
grep -q 'ER_RX.*39=2.*150=F' "$CLIENT_LOG" && ok "client got fill ER 150=F/39=2" || fail "no fill ER"
cat > /tmp/e2e22-steps.json <<EOF
[{"tool":"sandbox_get_status","args":{"token":"$TOKEN"}}]
EOF
mcp_call /tmp/e2e22-steps.json
[ "$(result 1 "['isError']")" = "false" ] && ok "get_status ok" || fail "get_status: $(result 1 "['text']")"
result 1 "['text']" | grep -q '"FILLED": 1' && ok "status shows order FILLED" || fail "status: $(result 1 "['text']" | head -c 300)"
result 1 "['text']" | grep -q '"connectionStatus": "CONNECTED"' && ok "status CONNECTED" || fail "not connected"
result 1 "['text']" | grep -q '"ttlRemaining"' && ok "ttlRemaining present" || fail "no ttl"

echo "== 6. sandbox_send_raw heartbeat =="
cat > /tmp/e2e22-steps.json <<'EOF'
[{"tool":"sandbox_send_raw","args":{"token":"__TOKEN__","rawFix":"8=FIX.4.4|9=60|35=0|49=FIXLAB|56=MCPCLIENT|34=999|52=20260107-00:00:00|112=PING1|10=000"}}]
EOF
sed -i "s/__TOKEN__/$TOKEN/" /tmp/e2e22-steps.json
mcp_call /tmp/e2e22-steps.json
[ "$(result 1 "['isError']")" = "false" ] && ok "send_raw ok" || fail "send_raw: $(result 1 "['text']")"
result 1 "['text']" | grep -q '"msgType": "0"' && ok "returned record is 35=0" || fail "record: $(result 1 "['text']" | head -c 300)"
sleep 1
cat > /tmp/e2e22-steps.json <<EOF
[{"tool":"sandbox_list_messages","args":{"token":"$TOKEN","limit":5,"direction":"OUT"}}]
EOF
mcp_call /tmp/e2e22-steps.json
result 1 "['text']" | grep -q "PING1" && ok "raw heartbeat visible as OUT" || fail "PING1 not found"

echo "== 7. error paths =="
cat > /tmp/e2e22-steps.json <<'EOF'
[{"tool":"sandbox_get_status","args":{"token":"fixlab_bogus0000000000000000000000000000000000000000000000000000000000"}}]
EOF
mcp_call /tmp/e2e22-steps.json
[ "$(result 1 "['isError']")" = "true" ] && ok "bad token -> tool error" || fail "bad token not an error"
result 1 "['text']" | grep -qi "not found" && ok "error names it" || fail "error text: $(result 1 "['text']")"
cat > /tmp/e2e22-steps.json <<EOF
[{"tool":"sandbox_send_execution","args":{"token":"$TOKEN","clOrdId":"NOPE","action":"FILL","params":{"qty":1,"price":1}}}]
EOF
mcp_call /tmp/e2e22-steps.json
[ "$(result 1 "['isError']")" = "true" ] && ok "unknown order -> tool error" || fail "unknown order not an error"
cat > /tmp/e2e22-steps.json <<EOF
[{"tool":"sandbox_send_order","args":{"token":"$TOKEN","msgType":"D","fields":{"11":"X1","55":"T","54":"1","38":"10","40":"2"}}}]
EOF
mcp_call /tmp/e2e22-steps.json
[ "$(result 1 "['isError']")" = "true" ] && ok "send_order on acceptor -> tool error" || fail "acceptor send_order not an error"
result 1 "['text']" | grep -qi "initiator" && ok "error names initiator-only" || fail "error text: $(result 1 "['text']")"
cat > /tmp/e2e22-steps.json <<'EOF'
[{"tool":"sandbox_list_messages","args":{"token":"x","limit":999}}]
EOF
mcp_call /tmp/e2e22-steps.json
[ "$(result 1 "['isError']")" = "true" ] && ok "limit 999 -> tool error" || fail "limit not validated"

echo "== 8. INITIATOR flow: sandbox_create + sandbox_send_order =="
"$BIN/fixacceptor" --port $ACC_PORT --sender REMOTEACC --target FIXLAB >"$ACCEPTOR_LOG" 2>&1 &
ACCPID=$!
sleep 1
cat > /tmp/e2e22-steps.json <<EOF
[{"tool":"sandbox_create","args":{"role":"INITIATOR","beginString":"FIX.4.4","senderCompId":"FIXLAB","remoteHost":"127.0.0.1","remotePort":"$ACC_PORT","remoteTargetCompId":"REMOTEACC"}}]
EOF
mcp_call /tmp/e2e22-steps.json
[ "$(result 1 "['isError']")" = "false" ] && ok "initiator create ok" || fail "initiator create: $(result 1 "['text']")"
ITOKEN=$(result 1 "['structured']['sessionToken']" | tr -d '"')
for i in $(seq 1 30); do
  ST=$(curl -s "$API/$ITOKEN" | python3 -c "import json,sys;print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
  [ "$ST" = "LOGON_ACCEPTED" ] && break
  sleep 1
done
[ "$ST" = "LOGON_ACCEPTED" ] && ok "initiator LOGON_ACCEPTED" || fail "initiator status: $ST"
cat > /tmp/e2e22-steps.json <<EOF
[{"tool":"sandbox_send_order","args":{"token":"$ITOKEN","msgType":"D","fields":{"11":"MCP-OUT-1","55":"TEST","54":"1","38":"100","40":"2","44":"50.25"}}}]
EOF
mcp_call /tmp/e2e22-steps.json
[ "$(result 1 "['isError']")" = "false" ] && ok "send_order ok" || fail "send_order: $(result 1 "['text']")"
if wait_for 'NOS_RX.*11=MCP-OUT-1' "$ACCEPTOR_LOG" 15; then
  ok "counterparty received injected NOS"
else
  fail "counterparty did not receive NOS"
fi
sleep 2
cat > /tmp/e2e22-steps.json <<EOF
[{"tool":"sandbox_get_status","args":{"token":"$ITOKEN"}}]
EOF
mcp_call /tmp/e2e22-steps.json
result 1 "['text']" | grep -q '"FILLED": 1' && ok "initiator blotter FILLED from remote ER" \
  || fail "initiator status: $(result 1 "['text']" | head -c 400)"

echo "== cleanup =="
exec 3>&-
kill $CLI $ACCPID 2>/dev/null || true
curl -s -X DELETE "$API/$TOKEN" -o /dev/null
curl -s -X DELETE "$API/$ITOKEN" -o /dev/null
kill $SRV 2>/dev/null || true
sleep 1
cleanup_stale

echo
echo "RESULT: $PASS passed, $FAIL failed"
[ "$FAIL" = "0" ]
