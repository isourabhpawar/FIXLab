#!/bin/bash
# FixLab Phase 2.4 end-to-end acceptance test: custom FIX dictionaries
# (spec §48: "Upload: custom FIX XML").
#
#   1. Craft a custom dictionary (FIX44 shape + custom field 9001
#      "MyCustomField" with enums A=Alpha/B=Beta) -> POST -> 200 with
#      field/message counts; session GET shows dictionary.custom=true.
#   2. Real fixclient sends NOS with 9001=B -> WS FIX_MSG_IN streams and
#      /messages shows {tag:9001, name:"MyCustomField", value:"B",
#      enumDescription:"Beta"} (session pipeline uses the override).
#   3. Decode API with the session token resolves 9001; without the
#      token it is "Unknown".
#   4. Negatives: malformed XML / valid XML but not a FIX dictionary /
#      empty / >512 KiB -> 400 naming the problem; unknown token -> 404.
#   5. DELETE -> reverts: 9001 resolves to "Unknown" again.
#   6. Concurrency smoke: upload while 10 custom-tagged NOS stream in;
#      all resolve, no crash (the -race unit test covers the swap).
#
# Usage: ./backend/e2e_phase2_4.sh   (run from the repo root)
set -u
trap '' PIPE
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
SERVER_LOG=/tmp/fixlab-e2e24-server.log
WS_LOG=/tmp/fixlab-e2e24-ws.log
FIFO=/tmp/fixlab-e2e24-cmd
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

CUSTOM_XML="$ROOT/backend/e2e_phase2_4_dict.xml"
cat > "$CUSTOM_XML" << 'XMLEOF'
<?xml version="1.0" encoding="UTF-8"?>
<fix major="4" minor="4" servicepack="0" type="FIX">
  <header/>
  <trailer/>
  <fields>
    <field number="9001" name="MyCustomField" type="STRING">
      <value enum="A" description="Alpha"/>
      <value enum="B" description="Beta"/>
    </field>
    <field number="9002" name="AnotherCustom" type="INT"/>
  </fields>
  <messages>
    <message name="NewOrderSingle" msgtype="D" msgcat="app"/>
    <message name="Heartbeat" msgtype="0" msgcat="admin"/>
  </messages>
</fix>
XMLEOF

echo "== building =="
export PATH="$HOME/go/go/bin:$PATH"
(cd "$ROOT" && go build -o bin/fixlab-server ./backend/cmd/server \
              && go build -o bin/fixclient ./backend/cmd/fixclient \
              && go build -o bin/wsprobe ./backend/cmd/wsprobe) || exit 1

echo "== starting server =="
FIXLAB_HTTP_ADDR=127.0.0.1:8080 FIXLAB_MAX_SESSIONS_PER_IP=10 \
  "$BIN/fixlab-server" >"$SERVER_LOG" 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null; rm -f $FIFO $CUSTOM_XML' EXIT
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

upload_dict() { # xmlfile name -> code|body
  python3 - "$1" "$2" << 'PYEOF' > /tmp/fixlab-e2e24-upload.json
import json,sys
xml = open(sys.argv[1]).read()
print(json.dumps({"xml": xml, "name": sys.argv[2]}))
PYEOF
  http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/dictionary" \
    -H 'Content-Type: application/json' -d @/tmp/fixlab-e2e24-upload.json
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

echo "== session A =="
new_session "E2E24A_$$" || exit 1

echo "== 1. default dictionary is standard =="
DG=$(curl -s "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/dictionary")
echo "$DG" | python3 -c "
import json,sys; d=json.load(sys.stdin)
assert d['custom'] is False, d
assert d['beginString'] == 'FIX.4.4', d
assert d['fields'] > 500, d" && ok "GET /dictionary: standard FIX.4.4" || fail "default dictionary: $DG"
curl -s "http://127.0.0.1:8080/api/v1/sessions/$TOKEN" | python3 -c "
import json,sys; d=json.load(sys.stdin)['dictionary']
assert d['custom'] is False, d" && ok "session GET dictionary.custom=false" || fail "session GET dictionary"

echo "== 2. upload custom dictionary =="
R=$(upload_dict "$CUSTOM_XML" "acme-broker")
CODE="${R%%|*}"; BODY="${R#*|}"
[ "$CODE" == "200" ] && ok "upload -> 200" || fail "upload -> $R"
python3 - "$BODY" << 'PYEOF' > /dev/null && ok "upload stats: 2 fields, 2 messages, FIX.4.4" || fail "upload stats: $BODY"
import json,sys
d = json.loads(sys.argv[1])
assert d["custom"] is True and d["name"] == "acme-broker", d
assert d["fields"] == 2 and d["messages"] == 2, d
assert d["beginString"] == "FIX.4.4", d
PYEOF
curl -s "http://127.0.0.1:8080/api/v1/sessions/$TOKEN" | python3 -c "
import json,sys; d=json.load(sys.stdin)['dictionary']
assert d['custom'] is True and d['name'] == 'acme-broker', d" \
  && ok "session GET dictionary.custom=true name=acme-broker" || fail "session GET after upload"

echo "== ws probe =="
"$BIN/wsprobe" --url "ws://127.0.0.1:8080/ws/session/$TOKEN" --dur 300s >"$WS_LOG" 2>&1 &
WS=$!
sleep 1
grep -q 'WSPROBE: CONNECTED' "$WS_LOG" && ok "ws probe subscribed" || fail "ws probe"

echo "== 3. real client sends NOS with custom tag 9001=B =="
start_client "E2E24A_$$" "$PORT" /tmp/fixlab-e2e24-client-a.log && ok "client logon" || { fail "client logon"; tail -5 "$CLIENT_LOG"; }
TS=$(date -u +%Y%m%d-%H:%M:%S)
echo "RAW D 11=C1 55=MSFT 54=1 38=100 40=2 44=310.50 60=$TS 9001=B" >&3
sleep 3
grep -q 'WSPROBE: FIX_MSG_IN' "$WS_LOG" && ok "ws: FIX_MSG_IN streamed" || fail "ws FIX_MSG_IN"
# The ack ER proves the engine let the user-defined field through.
grep -q 'FIXCLIENT: ER_RX' "$CLIENT_LOG" && ok "client: ack ER received (engine passed 9001 through)" || fail "client ack ER"
MSG9001=$(curl -s "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/messages?limit=20" | python3 -c "
import json,sys
msgs = json.load(sys.stdin)['messages']
for m in msgs:
    if m['msgType'] == 'D' and m['direction'] == 'INBOUND':
        for f in m['fields']:
            if f['tag'] == 9001:
                print(json.dumps(f)); break
        break
")
echo "   9001 field record: $MSG9001"
echo "$MSG9001" | python3 -c "
import json,sys; f=json.load(sys.stdin)
assert f['name'] == 'MyCustomField', f
assert f['value'] == 'B', f
assert f['enumDescription'] == 'Beta', f" \
  && ok "messages: 9001 resolves to MyCustomField/Beta" || fail "messages 9001 resolution"

echo "== 4. decode API with/without session token =="
# Build a raw message with a correct checksum containing 9001=B.
RAWMSG=$(python3 - << 'PYEOF'
body = "35=D|49=C1|56=FIXLAB|34=2|52=20260107-05:30:00|11=O1|55=MSFT|54=1|38=100|40=2|44=310.50|9001=B|"
head = "8=FIX.4.4|9=%d|" % len(body)
s = sum(head.encode()) + sum(body.encode())
print(head + body + "10=%03d|" % (s % 256))
PYEOF
)
decode_with() { # token-or-empty -> name of 9001
  local tok=$1
  curl -s -X POST http://127.0.0.1:8080/api/v1/tools/decode \
    -H 'Content-Type: application/json' \
    -d "{\"rawFix\": \"$RAWMSG\", \"token\": \"$tok\"}" | python3 -c "
import json,sys
for f in json.load(sys.stdin)['fields']:
    if f['tag'] == 9001:
        print(f['name']); break"
}
[ "$(decode_with "")" == "Unknown" ] && ok "decode without token: 9001=Unknown" || fail "decode without token"
[ "$(decode_with "$TOKEN")" == "MyCustomField" ] && ok "decode with token: 9001=MyCustomField" || fail "decode with token"

echo "== 5. negatives =="
up_raw() { # raw-json-body -> code|body
  http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/dictionary" \
    -H 'Content-Type: application/json' -d "$1"
}
R=$(up_raw '{"xml":"","name":"x"}'); [ "${R%%|*}" == "400" ] && ok "empty xml -> 400" || fail "empty -> $R"
R=$(up_raw '{"xml":"<fix major=\"4\"","name":"x"}'); [ "${R%%|*}" == "400" ] && echo "${R#*|}" | grep -q "malformed" && ok "malformed XML -> 400 naming it" || fail "malformed -> $R"
R=$(up_raw '{"xml":"<config><a\/><\/config>","name":"x"}'); [ "${R%%|*}" == "400" ] && echo "${R#*|}" | grep -q "not a FIX data dictionary" && ok "non-dict XML -> 400 naming it" || fail "non-dict -> $R"
python3 -c "import json; print(json.dumps({'xml': 'x' * (512*1024 + 1), 'name': 'big'}))" > /tmp/fixlab-e2e24-big.json
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/dictionary" \
  -H 'Content-Type: application/json' -d @/tmp/fixlab-e2e24-big.json)
[ "${R%%|*}" == "400" ] && echo "${R#*|}" | grep -q "too large" && ok "oversized XML -> 400 naming the cap" || fail "oversized -> ${R:0:120}"
R=$(http_code_body -X POST "http://127.0.0.1:8080/api/v1/sessions/fixlab_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef00/dictionary" \
  -H 'Content-Type: application/json' -d '{"xml":"<fix major=\"4\" minor=\"4\"><fields><field number=\"1\" name=\"X\" type=\"STRING\"\/><\/fields><\/fix>"}')
[ "${R%%|*}" == "404" ] && ok "unknown token -> 404" || fail "unknown token -> $R"

echo "== 6. DELETE reverts to standard =="
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/dictionary")
[ "$CODE" == "200" ] && ok "DELETE -> 200" || fail "DELETE -> $CODE"
curl -s "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/dictionary" | python3 -c "
import json,sys; d=json.load(sys.stdin); assert d['custom'] is False, d" \
  && ok "GET /dictionary: custom=false after delete" || fail "revert GET"
[ "$(decode_with "$TOKEN")" == "Unknown" ] && ok "decode with token after delete: 9001=Unknown" || fail "decode after delete"

echo "== 7. concurrency smoke: upload while 10 custom-tagged NOS stream =="
R=$(upload_dict "$CUSTOM_XML" "acme2")
[ "${R%%|*}" == "200" ] && ok "re-upload -> 200" || fail "re-upload -> $R"
for i in $(seq 1 10); do
  echo "RAW D 11=CC$i 55=MSFT 54=1 38=10 40=2 44=300 60=$TS 9001=A" >&3
done
sleep 4
RESOLVED=$(curl -s "http://127.0.0.1:8080/api/v1/sessions/$TOKEN/messages?limit=60" | python3 -c "
import json,sys
n = 0
for m in json.load(sys.stdin)['messages']:
    if m['msgType'] == 'D' and m['direction'] == 'INBOUND':
        for f in m['fields']:
            if f['tag'] == 9001 and f['name'] == 'MyCustomField' and f['enumDescription'] == 'Alpha':
                n += 1
print(n)")
[ "$RESOLVED" -ge 10 ] && ok "10/10 streamed NOS resolve 9001=MyCustomField/Alpha" || fail "resolved=$RESOLVED"
curl -sf http://127.0.0.1:8080/healthz >/dev/null && ok "server alive after smoke" || fail "server died"

stop_client && ok "client logout" || fail "client logout"
kill $WS 2>/dev/null

echo
echo "== $PASS passed, $FAIL failed =="
[ "$FAIL" -eq 0 ]
