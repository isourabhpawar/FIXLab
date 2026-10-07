#!/bin/bash
# FixLab Phase 7 end-to-end acceptance test: hardening (spec §15 TLS,
# §41 security, §44 rate limits, §45 observability).
#
# Part A — TLS acceptor (edge termination):
#   1. POST {"tls":true} -> 201, endpoint.tls=true, certFingerprint
#      (64 hex), certSelfSigned=true
#   2. openssl s_client -tls1_2 -> handshake OK, protocol >= TLS 1.2
#   3. openssl s_client -tls1_1 -> handshake REFUSED
#   4. fixclient --tls -> LOGON_OK / TESTREQ_OK / LOGOUT_OK (proves the
#      TLS-termination -> guard -> QuickFIX engine path end to end)
#   5. plaintext FIX bytes to the TLS port -> rejected, no FIX response
#      (no protocol confusion)
#   6. oversized frame (>8192) over TLS -> dropped (guard still enforced
#      on the decrypted stream)
#   7. TCP connect burst -> per-IP rate limit throttles
#      (guard_rate_limited > 0, server stays up)
# Part B — authn / expiry:
#   8. bad token -> API 404, WS 404
#   9. short-TTL server: TLS session expires -> TCP port refused (TLS
#      listener closed, no leak), GET -> 404, session_expirations >= 1
# Part C — initiator TLS:
#  10. fixacceptor --tls; initiator session tls:true (with
#      FIXLAB_TLS_INSECURE_SKIP_VERIFY=true) -> LOGON_ACCEPTED;
#      send-order 35=D -> acceptor logs NOS_RX
# Part D — spot checks:
#  11. SSRF battery spot-check: 169.254.169.254 -> 400 naming it
#  12. per-IP active-session cap (spec §44 default 1): 2nd concurrent
#      session from the same IP -> 429; works again after DELETE
#
# Usage: ./backend/e2e_phase7.sh   (run from the repo root)
set -u
trap '' PIPE
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
HTTP="127.0.0.1:8081"
API="http://$HTTP/api/v1/sessions"
SERVER_LOG=/tmp/fixlab-e2e7-server.log
ACCEPTOR_LOG=/tmp/fixlab-e2e7-acceptor.log
PASS=0; FAIL=0
ACC_PORT=11982
export PATH="$HOME/go/go/bin:$PATH"

cleanup_stale() {
  for pat in '[b]in/fixlab-server' '[b]in/fixacceptor' '[b]in/fixclient'; do
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
need curl; need python3; need openssl

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

jget() { # json-field-path... : read JSON from stdin
  python3 -c "import json,sys; d=json.load(sys.stdin); print(d$1)"
}

echo "== building =="
(cd "$ROOT" && go build -o bin/fixlab-server ./backend/cmd/server \
              && go build -o bin/fixclient ./backend/cmd/fixclient \
              && go build -o bin/fixacceptor ./backend/cmd/fixacceptor) || exit 1

echo "== starting server (self-signed dev cert) =="
FIXLAB_HTTP_ADDR=$HTTP FIXLAB_TEST_ALLOW_PRIVATE=true \
  FIXLAB_TLS_INSECURE_SKIP_VERIFY=true FIXLAB_INITIATOR_CONNECT_TIMEOUT=8s \
  FIXLAB_MAX_SESSIONS_PER_IP=50 \
  "$BIN/fixlab-server" >"$SERVER_LOG" 2>&1 &
SRV=$!
sleep 2
curl -sf "http://$HTTP/healthz" >/dev/null || { echo "server did not start"; cat "$SERVER_LOG"; exit 1; }
grep -q "SELF-SIGNED" "$SERVER_LOG" && ok "server logs the self-signed dev-cert warning loudly" \
  || fail "no self-signed warning in server log"

echo "== A1. TLS acceptor session creation =="
CREATE=$(curl -sf -X POST "$API" -H 'Content-Type: application/json' \
  -d '{"targetCompId":"TLSCLIENT","tls":true}') || { echo "create failed"; exit 1; }
TOKEN=$(jget "['sessionToken']" <<<"$CREATE")
[[ "$TOKEN" == fixlab_* ]] && ok "token issued" || fail "bad token: $TOKEN"
[[ "$(jget "['endpoint']['tls']" <<<"$CREATE")" == "True" ]] && ok "endpoint.tls=true" || fail "endpoint.tls"
FP=$(jget "['certFingerprint']" <<<"$CREATE")
[[ "$FP" =~ ^[0-9a-f]{64}$ ]] && ok "certFingerprint is 64 hex chars" || fail "fingerprint: $FP"
[[ "$(jget "['certSelfSigned']" <<<"$CREATE")" == "True" ]] && ok "certSelfSigned=true" || fail "certSelfSigned"
PORT=$(jget "['endpoint']['port']" <<<"$CREATE")
echo "    TLS port: $PORT"

echo "== A2. TLS 1.2+ handshake succeeds =="
OUT=$(echo | openssl s_client -connect "127.0.0.1:$PORT" -tls1_2 -brief 2>&1)
if echo "$OUT" | grep -qE "Protocol version: TLSv1\.[23]"; then
  ok "TLS handshake OK ($(echo "$OUT" | grep -oE 'TLSv1\.[23]' | head -1))"
else
  fail "TLS 1.2 handshake failed: $(echo "$OUT" | head -5 | tr '\n' ' ')"
fi

echo "== A3. TLS 1.0/1.1 handshake refused =="
if echo | openssl s_client -connect "127.0.0.1:$PORT" -tls1_1 -brief </dev/null >/dev/null 2>&1; then
  fail "TLS 1.1 handshake was accepted"
else
  ok "TLS 1.1 handshake refused"
fi

echo "== A4. logon through the TLS tunnel (edge -> guard -> engine) =="
"$BIN/fixclient" --tls --host 127.0.0.1 --port "$PORT" --sender TLSCLIENT --target FIXLAB \
  --dict backend/specs/FIX44.xml --stay 6s > /tmp/fixlab-e2e7-fixclient.log 2>&1
FC=$?
grep -q "FIXCLIENT: LOGON_OK" /tmp/fixlab-e2e7-fixclient.log && ok "TLS logon OK" || fail "no LOGON_OK (exit $FC)"
grep -q "FIXCLIENT: TESTREQ_OK" /tmp/fixlab-e2e7-fixclient.log && ok "TLS TestRequest round-trip OK" || fail "no TESTREQ_OK"
grep -q "FIXCLIENT: LOGOUT_OK" /tmp/fixlab-e2e7-fixclient.log && ok "TLS logout OK" || fail "no LOGOUT_OK"

echo "== A5. plaintext FIX bytes to the TLS port are rejected =="
python3 - "$PORT" <<'EOF' > /tmp/fixlab-e2e7-plaintext.log 2>&1
import socket, sys
port = int(sys.argv[1])
s = socket.create_connection(("127.0.0.1", port), timeout=5)
# well-formed logon frame, but PLAINTEXT on a TLS port
s.sendall(b"8=FIX.4.4\x019=999\x0135=A\x0149=TLSCLIENT\x0156=FIXLAB\x0134=99\x0110=000\x01")
s.settimeout(3)
try:
    data = s.recv(4096)
    print("RECEIVED:", data[:80])
except Exception as e:
    print("CLOSED:", type(e).__name__)
EOF
if grep -q "8=FIX" /tmp/fixlab-e2e7-plaintext.log; then
  fail "TLS port answered plaintext: $(cat /tmp/fixlab-e2e7-plaintext.log)"
else
  ok "plaintext to TLS port rejected ($(head -1 /tmp/fixlab-e2e7-plaintext.log | cut -c1-60))"
fi

echo "== A6. oversized frame over TLS is dropped (guard enforced) =="
DROPPED_BEFORE=$(curl -s "http://$HTTP/api/v1/metrics" | jget "['guard_dropped_frames']")
python3 - "$PORT" <<'EOF'
import socket, ssl, sys
port = int(sys.argv[1])
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
raw = socket.create_connection(("127.0.0.1", port), timeout=5)
s = ctx.wrap_socket(raw, server_hostname="localhost")
s.sendall(b"X" * 9000)  # > 8192 max frame, no SOH terminator
s.settimeout(4)
try:
    data = s.recv(1024)
    print("RESULT: recv returned", len(data), "bytes (closed)" if data == b"" else "bytes?!")
except Exception as e:
    print("RESULT: closed:", type(e).__name__)
EOF
sleep 1
DROPPED_AFTER=$(curl -s "http://$HTTP/api/v1/metrics" | jget "['guard_dropped_frames']")
[[ "$DROPPED_AFTER" -gt "$DROPPED_BEFORE" ]] && ok "oversized TLS frame dropped (guard_dropped_frames $DROPPED_BEFORE -> $DROPPED_AFTER)" \
  || fail "guard_dropped_frames did not move ($DROPPED_BEFORE -> $DROPPED_AFTER)"

echo "== A7. TCP connect burst is rate-limited, server stays up =="
RL_BEFORE=$(curl -s "http://$HTTP/api/v1/metrics" | jget "['guard_rate_limited']")
python3 - "$PORT" <<'EOF'
import socket, sys
port = int(sys.argv[1])
conns = []
for _ in range(12):
    try:
        c = socket.create_connection(("127.0.0.1", port), timeout=3)
        conns.append(c)
    except Exception:
        pass
import time; time.sleep(1)
for c in conns:
    try: c.close()
    except Exception: pass
print("burst done")
EOF
sleep 1
RL_AFTER=$(curl -s "http://$HTTP/api/v1/metrics" | jget "['guard_rate_limited']")
[[ "$RL_AFTER" -gt "$RL_BEFORE" ]] && ok "connect burst throttled (guard_rate_limited $RL_BEFORE -> $RL_AFTER)" \
  || fail "rate limiter did not throttle the burst"
curl -sf "http://$HTTP/healthz" >/dev/null && ok "server alive after burst" || fail "server died"

echo "== B8. bad token: API 404, WS 404 =="
R=$(http_code_body "http://$HTTP/api/v1/sessions/fixlab_0000000000000000000000000000000000000000000000000000000000000000")
[[ "${R%%|*}" == "404" ]] && ok "API bad token -> 404" || fail "API bad token -> $R"
R=$(http_code_body "http://$HTTP/ws/session/fixlab_0000000000000000000000000000000000000000000000000000000000000000")
[[ "${R%%|*}" == "404" ]] && ok "WS bad token -> 404" || fail "WS bad token -> $R"

echo "== B9. TLS session expiry closes the TLS listener (no leak) =="
FIXLAB_HTTP_ADDR=127.0.0.1:8082 FIXLAB_SESSION_TTL=4s FIXLAB_CLEANUP_INTERVAL=1s \
  FIXLAB_MAX_SESSIONS_PER_IP=50 \
  "$BIN/fixlab-server" >/tmp/fixlab-e2e7-expiry.log 2>&1 &
ESRV=$!
sleep 2
ECREATE=$(curl -sf -X POST http://127.0.0.1:8082/api/v1/sessions \
  -H 'Content-Type: application/json' -d '{"targetCompId":"EXPTLS","tls":true}')
ETOKEN=$(jget "['sessionToken']" <<<"$ECREATE")
EPORT=$(jget "['endpoint']['port']" <<<"$ECREATE")
EXP_BEFORE=$(curl -s http://127.0.0.1:8082/api/v1/metrics | jget "['session_expirations']")
sleep 7
if python3 -c "import socket; socket.create_connection(('127.0.0.1', $EPORT), timeout=3)" 2>/dev/null; then
  fail "TLS port $EPORT still accepting connections after expiry"
else
  ok "expired TLS listener closed (port $EPORT refused)"
fi
R=$(http_code_body "http://127.0.0.1:8082/api/v1/sessions/$ETOKEN")
[[ "${R%%|*}" == "404" ]] && ok "expired session -> 404" || fail "expired session -> $R"
EXP_AFTER=$(curl -s http://127.0.0.1:8082/api/v1/metrics | jget "['session_expirations']")
[[ "$EXP_AFTER" -gt "$EXP_BEFORE" ]] && ok "session_expirations counted" || fail "no expiration counted"
kill $ESRV 2>/dev/null; wait $ESRV 2>/dev/null

echo "== C10. initiator TLS to a TLS counterparty =="
"$BIN/fixacceptor" --tls --port $ACC_PORT --sender REMOTE --target FIXLABTLS7 --stay 60s >"$ACCEPTOR_LOG" 2>&1 &
ACC=$!
sleep 2
grep -q "listening" "$ACCEPTOR_LOG" || { echo "TLS acceptor did not start"; cat "$ACCEPTOR_LOG"; exit 1; }
ICREATE=$(curl -sf -X POST "$API" -H 'Content-Type: application/json' \
  -d '{"role":"INITIATOR","tls":true,"remoteHost":"127.0.0.1","remotePort":"'$ACC_PORT'","remoteCompId":"REMOTE","localSenderCompId":"FIXLABTLS7"}') \
  || { echo "initiator TLS create failed"; exit 1; }
ITOKEN=$(jget "['sessionToken']" <<<"$ICREATE")
[[ "$(jget "['endpoint']['tls']" <<<"$ICREATE")" == "True" ]] && ok "initiator endpoint.tls=true" || fail "initiator tls flag"
ST=""; i=0
while [ "$ST" != "LOGON_ACCEPTED" ] && [ $i -lt 40 ]; do
  ST=$(curl -s "$API/$ITOKEN" | jget "['status']"); sleep 0.5; i=$((i+1))
done
[[ "$ST" == "LOGON_ACCEPTED" ]] && ok "initiator TLS logon accepted" || fail "initiator TLS status=$ST"
R=$(http_code_body -X POST "$API/$ITOKEN/send-order" -H 'Content-Type: application/json' \
  -d '{"msgType":"D","fields":{"11":"E2E7-TLS1","55":"TEST","54":"1","38":"10","40":"2","44":"50.25"}}')
[[ "${R%%|*}" == "200" ]] && ok "send-order over TLS initiator -> 200" || fail "send-order -> $R"
wait_for "ACCEPTOR: NOS_RX 11=E2E7-TLS1" "$ACCEPTOR_LOG" 15 && ok "TLS counterparty received the order" \
  || fail "counterparty never saw the order"
curl -sf -X DELETE "$API/$ITOKEN" >/dev/null && ok "initiator TLS session deleted" || fail "delete"
kill $ACC 2>/dev/null; wait $ACC 2>/dev/null

echo "== D12. per-IP active-session cap (default 1) =="
# NOTE: this server runs WITHOUT FIXLAB_TEST_ALLOW_PRIVATE, so it also
# serves the SSRF spot-check below.
echo "== D11/D12 setup: strict server on :8083 (no SSRF override, default caps) =="
FIXLAB_HTTP_ADDR=127.0.0.1:8083 "$BIN/fixlab-server" >/tmp/fixlab-e2e7-cap.log 2>&1 &
CSRV=$!
sleep 2

echo "== D11. SSRF battery spot-check (override OFF) =="
R=$(http_code_body -X POST http://127.0.0.1:8083/api/v1/sessions -H 'Content-Type: application/json' \
  -d '{"role":"INITIATOR","remoteHost":"169.254.169.254","remotePort":"80","remoteCompId":"X"}')
if [[ "${R%%|*}" == "400" && "${R#*|}" == *"169.254"* ]]; then
  ok "link-local metadata IP -> 400 naming it"
else
  fail "SSRF spot -> $R"
fi
echo "== D12. per-IP active-session cap (default 1) =="
C1=$(curl -sf -X POST http://127.0.0.1:8083/api/v1/sessions \
  -H 'Content-Type: application/json' -d '{"targetCompId":"CAP1"}') || { echo "cap server create 1 failed"; exit 1; }
CT1=$(jget "['sessionToken']" <<<"$C1")
R=$(http_code_body -X POST http://127.0.0.1:8083/api/v1/sessions \
  -H 'Content-Type: application/json' -d '{"targetCompId":"CAP2"}')
if [[ "${R%%|*}" == "429" && "${R#*|}" == *"active session limit"* ]]; then
  ok "2nd concurrent session from same IP -> 429 naming the limit"
else
  fail "per-IP cap -> $R"
fi
curl -sf -X DELETE "http://127.0.0.1:8083/api/v1/sessions/$CT1" >/dev/null
C3=$(curl -sf -X POST http://127.0.0.1:8083/api/v1/sessions \
  -H 'Content-Type: application/json' -d '{"targetCompId":"CAP3"}') \
  && ok "create works again after DELETE" || fail "recreate after delete"
kill $CSRV 2>/dev/null; wait $CSRV 2>/dev/null

echo "== cleanup =="
curl -sf -X DELETE "$API/$TOKEN" >/dev/null
kill $SRV 2>/dev/null; wait $SRV 2>/dev/null
# the TLS port must be released on destroy
sleep 1
if python3 -c "import socket; socket.create_connection(('127.0.0.1', $PORT), timeout=3)" 2>/dev/null; then
  fail "TLS port $PORT still open after DELETE"
else
  ok "TLS port released on destroy"
fi

echo
echo "phase7: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
