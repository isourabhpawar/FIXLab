# FixLab — FIX Developer Sandbox & Counterparty Simulator

> **SIMULATION ENVIRONMENT — NOT A LIVE TRADING VENUE**

FixLab lets developers connect a real FIX engine to a hosted FIX
counterparty without installing a simulator or requesting broker test
access. Create a sandbox, get connection details, connect — first FIX
message in under 5 minutes.

This repo is at **Phase 7 — V1 complete**: the FIX engine foundation
(Go + QuickFIX/Go acceptor, dynamic ports, session lifecycle, REST API),
the live browser (Next.js workspace streaming every FIX message over
WebSocket, with a tag-level message inspector), the trading simulation
(inbound NewOrderSingle / OrderCancelRequest / OrderCancelReplaceRequest
handling, a live order blotter, and browser-driven executions generating
engine-validated ExecutionReports), the deterministic rule engine
with the kill switch, initiator mode — FixLab dials your FIX
acceptor (FIX 4.4, TCP or TLS) with full SSRF protection, a live
CONNECTING → TCP_CONNECTED → LOGON_SENT → LOGON_ACCEPTED status chain,
browser order injection (35=D/F/G), and an OUTBOUND blotter driven by
the remote's ExecutionReports — the free developer tools (FIX decoder,
checksum, timestamp converter, QuickFIX/J + QuickFIX/n config
generator, order state explorer) — **and hardening**: TLS 1.2+
terminated at the edge (acceptor) and on outbound dials (initiator),
per-IP session caps, the full rate-limit battery, and Docker
deployment. See `docs/V1_SUMMARY.md` for the per-phase record.

## Prerequisites

- Go 1.23+ (`go version`)
- Node 18+ (`node --version`) for the frontend
- No other backend dependencies — the FIX dictionaries
  (`backend/specs/*.xml`) are embedded in the binary.

## Build

```bash
go build -o bin/fixlab-server ./backend/cmd/server
go build -o bin/fixclient   ./backend/cmd/fixclient

cd frontend && npm install && npm run build
```

## Run

```bash
# Terminal 1 — backend (REST on :8080, FIX ports from the pool)
./bin/fixlab-server

# Terminal 2 — frontend (http://localhost:3000)
cd frontend && npm run dev
# or: npm start   (after npm run build)
```

Point the frontend at a non-default backend with
`NEXT_PUBLIC_FIXLAB_API_URL` (see `frontend/.env.local`).

## Quick start (manual)

```bash
# 1. Create a sandbox (FIX 4.4 acceptor, TCP, no auth — the phase-1 set)
curl -s -X POST localhost:8080/api/v1/sessions \
  -H 'Content-Type: application/json' \
  -d '{"targetCompId":"MYCLIENT"}' | python3 -m json.tool
```

Response (spec §38):

```json
{
  "sessionToken": "fixlab_…",
  "role": "ACCEPTOR",
  "endpoint": { "host": "127.0.0.1", "port": 10000, "tls": false },
  "identifiers": { "beginString": "FIX.4.4", "senderCompId": "FIXLAB", "targetCompId": "MYCLIENT" },
  "status": "WAITING_FOR_CONNECTION",
  "createdAt": "…",
  "expiresAt": "…"
}
```

```bash
# 2. Connect a real FIX engine. The repo ships a QuickFIX/Go test client:
./bin/fixclient --host 127.0.0.1 --port 10000 \
  --sender MYCLIENT --target FIXLAB --dict backend/specs/FIX44.xml
# completes logon → heartbeats → TestRequest round-trip → clean logout

# 3. Inspect / destroy
curl -s localhost:8080/api/v1/sessions/<token> | python3 -m json.tool
curl -s -X DELETE localhost:8080/api/v1/sessions/<token> -w '%{http_code}\n'
curl -s localhost:8080/api/v1/metrics | python3 -m json.tool
```

## Automated acceptance tests

```bash
./backend/e2e_phase1.sh   # engine foundation: 18 passed, 0 failed
./backend/e2e_phase3.sh   # trading simulation: 42 passed, 0 failed
./backend/e2e_phase4.sh   # rules + kill switch: 43 passed, 0 failed
./backend/e2e_phase5.sh   # initiator + SSRF: 46 passed, 0 failed
./backend/e2e_phase7.sh   # hardening (TLS, rate limits, expiry): 28 passed, 0 failed
./backend/e2e_phase2_1.sh # stochastic simulator: 57 passed, 0 failed
./backend/e2e_phase2_2.sh # MCP server over stdio: 35 passed, 0 failed
./backend/e2e_phase2_3.sh # scenario recording + replay: 29 passed, 0 failed
./backend/e2e_phase2_4.sh # custom FIX dictionaries: 25 passed, 0 failed
```

Phase 4 runs the deterministic rule engine against the real external FIX
client: a Symbol rule auto-fills with no API call in between, a
higher-priority quantity rule wins the priority contest, a DELAY rule
holds the fill ≥ 500 ms, the kill switch rejects everything immediately
(rules unevaluated), rule CRUD + validation, and the manual-execution
conflict (409) on a rule-filled order.

Phase 5 runs initiator mode against a real scripted QuickFIX/Go acceptor
(`bin/fixacceptor`): session creation → WS status chain
(CONNECTING → TCP_CONNECTED → LOGON_SENT → LOGON_ACCEPTED) →
injected 35=D received by the counterparty → its fill ER streams back as
FIX_MSG_IN and flips the OUTBOUND blotter order to FILLED → 35=F cancel
and 35=G replace round-trips → the full negative battery (400/409/503
cases, closed-port → CONNECTION_FAILED) → TLS initiator creation is
accepted (201; the handshake is proven in e2e_phase7) → the SSRF battery
(private ranges, localhost, metadata hosts, bad ports — all 400 naming
the violation) → the test-only override re-allowing 127.0.0.1.

Phase 7 runs the hardening battery: TLS acceptor session creation
(`endpoint.tls`, 64-hex cert fingerprint, self-signed flag) → TLS 1.2+
handshake via openssl → TLS 1.0/1.1 refused → a full FIX logon through
the TLS tunnel (edge → guard → QuickFIX engine) → plaintext bytes to a
TLS port rejected → oversized frame over TLS dropped (guard enforced on
the decrypted stream) → TCP connect burst throttled with the server
alive → bad tokens 404 on API and WS → short-TTL TLS session expiry
closes the TLS listener (port refused, no leak) → initiator TLS to a
TLS counterparty (`fixacceptor --tls`) reaches LOGON_ACCEPTED and
delivers an injected order → SSRF spot-check → the per-IP
active-session cap (429, then 201 again after DELETE).

Phase 1 spins up the server, creates a session, runs the external FIX
client through the full lifecycle, and verifies token auth, port
release, and expiry cleanup.

Phase 3 runs the full order lifecycle against the real external FIX
client: NewOrderSingle → ack ExecutionReport (150=0/39=0) → partial fill
→ full fill (weighted AvgPx) → reject → cancel accept → cancel reject
(35=9) → replace accept → replace reject (35=9), plus invalid-transition
handling (409/404/400), WebSocket ORDER_CREATED/ORDER_UPDATED/
EXECUTION_SENT events, and the application-message quota (429 past the
limit).

## REST API

| Method | Path | Description |
| ------ | ---- | ----------- |
| POST | `/api/v1/sessions` | Create sandbox → `201` + session details |
| GET | `/api/v1/sessions/{token}` | Session details (token is the credential) |
| DELETE | `/api/v1/sessions/{token}` | Destroy sandbox → `204` |
| GET | `/api/v1/sessions/{token}/messages?limit=N` | Message history (newest N, default 100) |
| GET | `/api/v1/sessions/{token}/orders` | Order blotter (phase 3) |
| POST | `/api/v1/sessions/{token}/orders/{clOrdId}/execute` | Execution action: FILL, PARTIAL_FILL, REJECT, CANCEL_ACCEPT, CANCEL_REJECT, REPLACE_ACCEPT, REPLACE_REJECT |
| GET | `/api/v1/sessions/{token}/rules` | List rules in evaluation order (phase 4) |
| POST | `/api/v1/sessions/{token}/rules` | Create rule → `200` + rule (phase 4) |
| PUT | `/api/v1/sessions/{token}/rules/{ruleId}` | Update rule (phase 4) |
| DELETE | `/api/v1/sessions/{token}/rules/{ruleId}` | Delete rule (phase 4) |
| POST | `/api/v1/sessions/{token}/killswitch` | Toggle venue halt `{"enabled": bool}` (phase 4) |
| POST | `/api/v1/sessions/{token}/send-order` | Inject 35=D/F/G into the remote counterparty (phase 5, initiator only) |
| POST | `/api/v1/sessions/{token}/send-raw` | Send one raw FIX message through the session engine (phase 2.2, MCP); engine-stamped tags are restamped |
| GET | `/api/v1/sessions/{token}/stochastic` | Stochastic policy: config + outcomes drawn (phase 2.1) |
| PUT | `/api/v1/sessions/{token}/stochastic` | Set stochastic policy (phase 2.1) |
| GET | `/api/v1/sessions/{token}/scenario` | Recorded scenario: logon, inbound app messages, executions, logout/disconnect (phase 2.3). Never contains the session token |
| POST | `/api/v1/sessions/replay` | Replay a scenario into a FRESH acceptor session → `201` + new `sessionToken` (phase 2.3). Body: `{sourceToken}` or `{scenario}`, optional `speed` (absent = 1×; 0 = immediate) |
| POST | `/api/v1/tools/decode` | Decode one raw FIX message (phase 6): `{rawFix, beginString?, token?}` → parsed fields + Tag 9/10 validation. SOH, `\|` and `^A` delimiters accepted. Malformed input → 400 naming the problem; checksum/body-length mismatches → 200 with `valid: false` and expected vs actual. With `token`, the session's active dictionary is used (phase 2.4: custom override included) |
| POST | `/api/v1/sessions/{token}/dictionary` | Upload a custom FIX dictionary (phase 2.4): `{xml, name?}` → 200 + field/message counts. Strictly validated; 400 names the exact problem |
| GET | `/api/v1/sessions/{token}/dictionary` | Active dictionary info: `{custom, name?, beginString, fields, messages}` (phase 2.4) |
| DELETE | `/api/v1/sessions/{token}/dictionary` | Revert to the standard embedded dictionary (phase 2.4) |
| GET | `/ws/session/{token}` | Live WebSocket event stream (RFC 6455) |
| GET | `/api/v1/metrics` | Backend counters (JSON) |
| GET | `/healthz` | Liveness probe |

### WebSocket events (spec §19)

Every event is `{type, sessionId, timestamp, payload}`:

| Type | Payload |
| ---- | ------- |
| `CONNECTION_STATUS` | `{status, previous}` — WAITING_FOR_CONNECTION → TCP_CONNECTED → LOGON_RECEIVED → CONNECTED … |
| `FIX_MSG_IN` / `FIX_MSG_OUT` | spec §18 message: `rawFix` (pipe-delimited), `fields[]` with tag/name/value/enumDescription, direction, seq, CompIDs |
| `SEQUENCE_GAP` | `{expected, received, direction}` |
| `SESSION_ERROR` | `{error}` |
| `SESSION_EXPIRED` | `{reason: "ttl_expired" \| "destroyed"}` |
| `KILL_SWITCH` | `{enabled}` — venue halt toggled (phase 4) |
| `SCENARIO_REPLAY_STARTED` | `{speed, totalSteps}` — a scenario replay began (phase 2.3) |
| `SCENARIO_REPLAY_FINISHED` | `{appliedSteps, totalSteps, failed, error, durationMs}` — replay ended (phase 2.3) |

The token in the path is the authenticator; unknown tokens are rejected
before the upgrade. On subscribe the server immediately sends the
current `CONNECTION_STATUS` snapshot, and on destroy/expiry it sends
`SESSION_EXPIRED` then closes the stream. The stream stays open across
FIX reconnects. Fan-out is bounded: slow browsers drop events instead of
blocking the engine.

### Rule DSL (phase 4)

Rules are evaluated on every inbound NewOrderSingle, in priority order
(lowest first, ties broken by creation order); the first matching rule
fires. All predicates must match (AND); a rule with no predicates
matches everything. The kill switch is evaluated before all rules.

```json
{
  "name": "msft-autofill",
  "priority": 10,
  "enabled": true,
  "predicates": [{ "tag": 55, "op": "eq", "value": "MSFT" }],
  "actions": [
    { "type": "DELAY", "delayMs": 500 },
    { "type": "FULL_FILL", "price": 310.50 }
  ]
}
```

- Predicates: `eq`, `ne`, `gt`, `lt`, `contains` on tags 11 (ClOrdID),
  1 (Account), 54 (Side), 55 (Symbol), 38 (OrderQty), 40 (OrdType),
  44 (Price). `gt`/`lt` are numeric (38/40/44 only); the rest compare
  strings. Legacy op names (`EQUALS`, `NOT_EQUALS`, …) are accepted.
- Actions: `ACK_NEW` (send the 150=0/39=0 ack), `FULL_FILL` (price
  optional — fills at the order's own price; market orders need the
  param), `PARTIAL_FILL` (`qty` = LastQty, `price` = LastPx, both > 0),
  `REJECT` (`ordRejReason` and/or `text`; one required), `DELAY`
  (`delayMs`, 0–60000, sleeps then continues the chain).
- Rule-fired executions consume the session app-message quota (429 past
  the limit), reuse the simulator's ER generation (CumQty/LeavesQty/AvgPx
  bookkeeping stays consistent), and stream the same ORDER_UPDATED /
  EXECUTION_SENT events. `rule_matches` counts fires; each rule also
  tracks `matchedCount`. Rules are created disabled unless
  `"enabled": true` is passed.
- Kill switch (`POST …/killswitch {"enabled": true}`): every inbound
  NewOrderSingle is immediately rejected (150=8/39=8, 58=
  `VENUE HALTED: kill switch enabled`); rules are not evaluated. A
  `KILL_SWITCH` event streams to browsers.

Create body (all optional, acceptor values shown):

```json
{ "fixVersion": "FIX.4.4", "role": "ACCEPTOR", "transport": "TCP",
  "tls": false, "auth": "NONE", "targetCompId": "MYCLIENT" }
```

`"tls": true` enables TLS 1.2+ (`"transport": "TLS"` is an accepted
alias). For acceptor sessions the public port terminates TLS at the
edge (see TLS below); for initiator sessions FixLab dials the remote
acceptor over TLS with the system root CAs. The response's
`endpoint.tls` reports the mode, and TLS sessions additionally carry
`certFingerprint` (SHA-256 of the serving certificate) and
`certSelfSigned`.

Initiator creation (phase 5):

```json
{ "role": "INITIATOR", "remoteHost": "fix.acme.com", "remotePort": "9878",
  "remoteCompId": "THEIRFIX", "localSenderCompId": "FIXLAB" }
```

The response's `endpoint` is the remote target and `remoteIp` is the
pinned, SSRF-validated dial address. Anything else (`FIX.4.2`, `TLS`, …)
is rejected with `400` and a message naming the phase that will support
it.

`POST /api/v1/sessions/{token}/send-order` injects one order message
into the remote counterparty (initiator sessions only, must be
connected):

```json
{ "msgType": "D",
  "fields": {"11": "ORD-1", "55": "TEST", "54": "1", "38": "100", "40": "2", "44": "50.25"} }
```

Required fields: D → 11/55/54/38/40; F → 11/41/55/54/38;
G → 11/41/55/54/38/40. Engine-stamped tags (8/9/10/34/35/49/52/56/…) are
rejected with 400. Injected orders are tracked `OUTBOUND` on the blotter;
the remote's 35=8/9 reports drive their status.

## Configuration (environment)

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `FIXLAB_HTTP_ADDR` | `:8080` | REST API listen address |
| `FIXLAB_PUBLIC_HOST` | `127.0.0.1` | Host reported in session endpoints |
| `FIXLAB_SESSION_TTL` | `4h` | Sandbox lifetime |
| `FIXLAB_CLEANUP_INTERVAL` | `60s` | Expiry worker period |
| `FIXLAB_PORT_MIN` / `FIXLAB_PORT_MAX` | `10000` / `20000` | Dynamic FIX port pool |
| `FIXLAB_INTERNAL_BIND` | `127.0.0.1` | Where the QuickFIX/Go acceptor binds internally — keep loopback-only; the public listener is the guard proxy |
| `FIXLAB_APP_MSG_LIMIT` | `250` | App-message quota (enforced on outbound executions; 429 past the limit) |
| `FIXLAB_MSG_HISTORY_CAP` | `500` | Bounded message history |
| `FIXLAB_ORDER_CAP` | `1000` | Order cap (phase 3) |
| `FIXLAB_MIN_HEARTBEAT_INT` | `10` | Minimum HeartBtInt (tag 108) on logon |
| `FIXLAB_MAX_FIX_FRAME` | `8192` | Max FIX frame bytes |
| `FIXLAB_TCP_IDLE_TIMEOUT` | `5m` | Idle TCP close |
| `FIXLAB_TCP_LOGON_TIMEOUT` | `30s` | Grace period for first logon |
| `FIXLAB_TCP_CONN_RATE_PER_MIN` | `5` | Connection attempts / min / IP |
| `FIXLAB_HTTP_RATE_PER_HOUR` | `10000` | HTTP requests / hour / IP |
| `FIXLAB_HTTP_MAX_BODY` | `1048576` | Max HTTP request body bytes |
| `FIXLAB_CORS_ORIGIN` | `*` | Access-Control-Allow-Origin for the API/WS (the Next.js dev server is cross-origin) |
| `FIXLAB_LOG_FIX_MESSAGES` | unset | Set `1` to log every FIX message |
| `FIXLAB_INITIATOR_CONNECT_TIMEOUT` | `10s` | Initiator dial+logon budget; expiry without logon → CONNECTION_FAILED and dialing stops |
| `FIXLAB_INITIATOR_MAX_OUTBOUND` | `50` | Max concurrent initiator sessions per server |
| `FIXLAB_INITIATOR_DIAL_RATE_PER_MIN` | `10` | Outbound dial attempts per minute per destination IP |
| `FIXLAB_INITIATOR_DNS_TIMEOUT` | `5s` | Timeout for the SSRF DNS-resolution step |
| `FIXLAB_TEST_ALLOW_PRIVATE` | unset | **TEST ONLY — never enable in production.** Bypasses private-range SSRF blocking so the acceptance suite can dial a 127.0.0.1 counterparty |
| `FIXLAB_TLS_CERT` / `FIXLAB_TLS_KEY` | unset | PEM certificate + private key for edge TLS termination. When unset, the server generates a self-signed certificate at startup (**dev-only**, logged loudly). If set but unloadable, the server still starts but `tls:true` sessions are refused with a 400 naming the problem |
| `FIXLAB_TLS_INSECURE_SKIP_VERIFY` | `false` | **TEST ONLY — never enable in production.** Skips remote certificate verification on initiator TLS dials (the acceptance suite dials a self-signed test counterparty) |
| `FIXLAB_MAX_SESSIONS_PER_IP` | `1` | Concurrent active sessions per client IP (spec §44); past the limit, creation returns 429 |

## Stochastic simulator (phase 2.1, spec §48)

The stochastic simulator draws a random outcome for every inbound
NewOrderSingle and executes it after a random latency sampled from
`Normal(avgLatencyMs, stdDevMs)`, clamped to `[0, maxDelayMs]`.
Evaluation order is fixed and documented:

```text
kill switch → deterministic rules (first match fires) → stochastic → manual ack
```

Stochastic fires only when enabled **and** no deterministic rule fired;
it never overrides the kill switch or a rule. Outcomes reuse the
phase-3 execution path (`Fill`/`PartialFill`/`Reject`), so
CumQty/LeavesQty/AvgPx bookkeeping, the blotter, WS events
(`ORDER_UPDATED`/`EXECUTION_SENT`), quota consumption, and ExecID
sequencing are identical to manual executions. A manual execution that
races a pending stochastic delay wins; the stochastic outcome is then
dropped with a logged warning (never double-executed).

```jsonc
PUT /api/v1/sessions/{token}/stochastic
{
  "enabled": true,
  "acceptPct": 85, "rejectPct": 15, "partialFillPct": 0,  // must sum to 100
  "avgLatencyMs": 45, "stdDevMs": 10, "maxDelayMs": 5000,
  "seed": 0  // 0 = random; nonzero reproduces the exact outcome sequence
}
```

- Full fill at the order's price; reject is `150=8/39=8` with
  `103=99` and a text naming the stochastic simulator; partial fill
  takes a random 10–90% of the quantity at the order price and leaves
  the remainder working for manual execution.
- Market orders (no price) can't be stochastically filled — the outcome
  is dropped with a warning and the order stays NEW for manual
  execution.
- Each session owns one RNG: draws happen synchronously on the FIX
  message pump (outcome → latency sample → fill-size roll), so a fixed
  seed reproduces the identical outcome sequence. Replacing the config
  resets the stream.
- Metrics: `stochastic_outcomes` (`{"fill": n, "reject": n, "partial": n}`)
  on `/api/v1/metrics`; per-session outcome counts ride on
  `GET …/stochastic` and the session GET response.
- The workspace's **Simulation** tab edits the policy (percentage
  inputs with live sum-to-100 validation, latency fields, seed) and
  shows the session's outcome counts.

## Scenario recording + replay (phase 2.3, spec §48)

Recording is **automatic and always on**: every session keeps a bounded
log (500 steps, ring semantics) of its application-level story —

```text
logon → inbound (35=D/F/G with fields) → execution (action + params)
      → logout / disconnect
```

Session-level admin messages (heartbeats, TestRequests, …) are
deliberately NOT recorded: the scenario is the trading story, not the
wire chatter. `GET /api/v1/sessions/{token}/scenario` exports it as
JSON; the export never contains the session token or any secret.

**Replay** re-enacts a scenario in a FRESH acceptor session — one call:

```bash
curl -X POST localhost:8080/api/v1/sessions/replay \
  -H 'Content-Type: application/json' \
  -d '{"sourceToken":"fixlab_…","speed":0}'   # speed 0 = apply immediately
# → 201 {"sessionToken":"fixlab_…","totalSteps":7,"speed":0}
```

or with a previously exported document: `{"scenario": {…}, "speed": 2}`.

Semantics, stated honestly:

- Inbound steps are re-injected **synthetically through the simulator**
  (marked `replayed: true`, FIX engine bypassed); execution steps are
  re-applied through the normal execution path, so blotter bookkeeping
  and WS events (`ORDER_CREATED`/`ORDER_UPDATED`/`EXECUTION_SENT`,
  `FIX_MSG_IN`/`FIX_MSG_OUT` marked replayed) are identical to the live
  path — but **nothing is transmitted on the wire and no quota is
  consumed**. Replay reproduces the application story, not the wire bytes.
- Steps run at their recorded relative offsets divided by `speed`
  (absent = 1 = real time; 0 = immediate). Logon/logout/disconnect steps
  are narrative only and are skipped.
- Rules, stochastic and the kill switch **never fire during replay**:
  replay replays actions, not automation config.
- Only ACCEPTOR-recorded scenarios replay; initiator scenarios are
  refused with a 400 naming it (the remote counterparty cannot be
  re-enacted). Empty scenarios, unknown step kinds and negative speeds
  are 400s; unknown source tokens are 404s.
- The replay target is a real sandbox (port, guard, engine, TTL); it
  gets a distinct CompID pair (`<source>-replay-<rand>`) because
  QuickFIX/Go keeps a process-wide session registry.
- `GET /api/v1/sessions/{token}` carries `replay:
  {inProgress, totalSteps, doneSteps, failed, error?}` while/after a
  replay runs.

The workspace's **Scenario** tab shows the recorded timeline
(human-readable steps with `T+` offsets), exports the JSON, and replays
into a new session with a speed selector. The MCP server exposes
`sandbox_get_scenario` and `sandbox_replay`, so an agent can record a
manual test session and replay it hands-free.

## Custom FIX dictionaries (phase 2.4, spec §48)

Upload your broker's QuickFIX data-dictionary XML to a session so
custom tags resolve to names and enum descriptions everywhere FixLab
inspects messages:

```bash
curl -X POST localhost:8080/api/v1/sessions/<token>/dictionary \
  -H 'Content-Type: application/json' \
  -d @- << 'EOF'
{"name": "acme-broker",
 "xml": "<?xml version=\"1.0\"?><fix major=\"4\" minor=\"4\" type=\"FIX\"><fields><field number=\"9001\" name=\"MyCustomField\" type=\"STRING\"><value enum=\"A\" description=\"Alpha\"/></field></fields><messages><message name=\"NewOrderSingle\" msgtype=\"D\"/></messages></fix>"}
EOF
# → 200 {"custom": true, "name": "acme-broker", "beginString": "FIX.4.4",
#         "fields": 1, "messages": 1}
```

- **Validation is strict and every failure names the problem (400):**
  well-formed XML, `<fix>` root, `major`/`minor` attributes, at least
  one `<field>` with a numeric `number` and a non-empty `name`,
  512 KiB size cap (the HTTP layer caps bodies at 1 MiB anyway).
- **Ephemeral:** the override lives only on the session and dies with
  it; it is never persisted. `GET …/dictionary` reports
  `{custom, name?, beginString, fields, messages}` (the XML itself is
  never echoed back); `DELETE …/dictionary` reverts to standard;
  session GET carries `dictionary: {custom, name?}`.
- **Where it applies:** the live WS `FIX_MSG_IN`/`FIX_MSG_OUT` field
  enrichment, `/messages`, the decoder (`POST /api/v1/tools/decode`
  with `"token"` — stateless by default, session-aware when a token
  is passed), and MCP `sandbox_list_messages`. The swap is atomic, so
  an upload never races in-flight message parsing.
- **Engine validation, stated honestly:** the QuickFIX/Go engine keeps
  validating the wire against the standard embedded dictionary — a
  per-session engine dictionary would require re-creating the FIX
  session (dropping the TCP connection and resetting sequence numbers),
  which QuickFIX/Go does not support hot. What the engine *does* do:
  the session config sets `ValidateUserDefinedFields=N`, so
  user-defined fields (tags ≥ 5000 — the conventional range for
  broker custom fields) pass inbound validation to the app instead of
  being session-rejected. Unknown *standard* tags (< 5000) are still
  wire-rejected, exactly like any strict QuickFIX counterparty.
- The workspace's **Dictionary** tab shows the active dictionary
  (standard FIX44 vs custom name + field/message counts), uploads via
  file picker or paste (validation errors shown verbatim), tests tag
  lookups against the active dictionary, and reverts to standard.

## MCP server (phase 2.2, spec §49)

`bin/mcp-server` exposes FixLab to AI coding agents over the
[Model Context Protocol](https://modelcontextprotocol.io) (stdio):

```text
Claude / Cursor
        │
        ▼
   MCP Server (bin/mcp-server, stdio)
        │  thin wrapper — never touches backend internals
        ▼
   FixLab REST API  (FIXLAB_API_URL, default http://127.0.0.1:8080)
        │
        ▼
   Session Manager → QuickFIX/Go
```

Build and point it at a running FixLab backend:

```bash
go build -o bin/mcp-server ./backend/cmd/mcp-server
FIXLAB_API_URL=http://127.0.0.1:8080 ./bin/mcp-server   # stdio; logs go to stderr
```

The six tools (spec §49) plus the two scenario tools (phase 2.3):

| Tool | Args | Does |
| ---- | ---- | ---- |
| `sandbox_create` | `role` (ACCEPTOR\|INITIATOR, required), `beginString`, `senderCompId`, `targetCompId`, `tls`; initiator also needs `remoteHost`, `remotePort`, `remoteTargetCompId` | Creates a sandbox → `sessionToken` (bearer secret), `endpoint`, `identifiers`, `expiresAt`, plus a human-readable `connect_howto` |
| `sandbox_get_status` | `token` | Role, FIX connection status, endpoint, TTL remaining, message/order counts (by status), kill-switch and stochastic state |
| `sandbox_list_messages` | `token`, `limit` (default 50, max 500), `direction` (IN\|OUT) | Message history, newest-first: raw pipe-delimited FIX + parsed fields (tag/name/value/enum) |
| `sandbox_send_order` | `token`, `msgType` (D\|F\|G), `fields` (tag→value strings) | Injects 35=D/F/G into the remote counterparty — **initiator sessions only** |
| `sandbox_send_execution` | `token`, `clOrdId`, `action` (FILL\|PARTIAL_FILL\|REJECT\|CANCEL_ACCEPT\|CANCEL_REJECT\|REPLACE_ACCEPT\|REPLACE_REJECT), `params` (`qty`, `price`, `text`, `ordRejReason`, `cxlRejReason`) | Drives a working order on an acceptor sandbox; returns the ExecutionReport sent |
| `sandbox_send_raw` | `token`, `rawFix` | Sends one raw FIX message (SOH/`|`/`^A`; must start with `8=`, end with `10=`) through the live session. **Limitation, stated honestly:** the engine always stamps 8/9/10/34/49/52/56 — truly raw byte injection is not possible |
| `sandbox_get_scenario` | `token` | Recorded scenario JSON: logon, inbound app messages, executions, logout/disconnect (phase 2.3). Never contains the token |
| `sandbox_replay` | `token` (source session) **or** `scenario`, `speed` (optional; 0 = immediate) | Replays a scenario into a fresh acceptor sandbox → new `sessionToken`; the story is re-enacted synthetically (`replayed:true`, nothing transmitted) |

Tool failures (bad token, unknown order, state conflict, quota) come
back as MCP tool errors carrying the API's message, so the agent can
self-correct. Tokens are never logged by the MCP server. The server is
stateless: pass the token on every call.

Claude Code (`~/.claude.json`):

```json
{
  "mcpServers": {
    "fixlab": {
      "command": "/path/to/fixlab/bin/mcp-server",
      "env": { "FIXLAB_API_URL": "http://127.0.0.1:8080" }
    }
  }
}
```

Cursor (`~/.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "fixlab": {
      "command": "/path/to/fixlab/bin/mcp-server",
      "env": { "FIXLAB_API_URL": "http://127.0.0.1:8080" }
    }
  }
}
```

Example agent loop: `sandbox_create` → connect a FIX engine to the
returned endpoint → `sandbox_list_messages` → `sandbox_send_execution`
→ `sandbox_list_messages`. Acceptance: `./backend/e2e_phase2_2.sh`
(35 checks, all green).

## Architecture

```
Developer FIX engine
        │ TCP (public port, guard proxy)
        ▼
security.Guard ── rate limit / idle + logon timeout /
                   8192-byte frame cap / min-heartbeat guard
        │ TCP (127.0.0.1, ephemeral port)
        ▼
engine (QuickFIX/Go acceptor) ── session state, framing, seq nums,
        │                          heartbeats, resends, logon/logout
        ▼ hooks (FIXEvent stream)
session.Session ── tokens, quotas, ring buffer, expiry worker
        │            │ publishes CONNECTION_STATUS / FIX_MSG_* /
        │            │ SEQUENCE_GAP / SESSION_ERROR / SESSION_EXPIRED
        ▼            ▼
api.Server ── REST + /ws/session/{token} ──► websocket.Hub ──► browsers
   (POST/GET/DELETE sessions, /messages)        (bounded fan-out)
```

### Frontend (`frontend/` — Next.js 15, TypeScript, Tailwind, Zustand)

| Route | Description |
| ----- | ----------- |
| `/` | Homepage: "Create Free FIX Sandbox" + simulation banner |
| `/sandbox` | Sandbox creation page (FIX 4.4 / acceptor / TCP; other options marked with their future phase) |
| `/session/[token]` | Live workspace: status pill, TTL countdown, connection details card, QuickFIX config snippet, live FIX feed, message inspector (SUMMARY / FIELDS / RAW / JSON), order blotter with execution controls (Fill / Partial Fill / Reject / cancel & replace accept-reject), Rules tab (deterministic engine + kill switch), Simulation tab (stochastic policy, phase 2.1), Scenario tab (recorded timeline, JSON export, replay into a new session — phase 2.3), Dictionary tab (custom FIX dictionary upload + tag lookup — phase 2.4), diagnostics tab |
| `/tools` | Free developer tools index (no account, no session) |
| `/tools/decoder` | FIX decoder: paste raw FIX (SOH / `\|` / `^A`) → parsed fields, dictionary names + enums, Tag 9/10 verdicts, SUMMARY / FIELDS / RAW / JSON views |
| `/tools/checksum` | Checksum calculator: compute the correct `10=` for a message, or verify an existing one (client-side) |
| `/tools/timestamp` | Timestamp converter: UTCTimestamp ↔ UTCDateOnly ↔ UTCTimeOnly ↔ ISO-8601 ↔ epoch (client-side) |
| `/tools/config` | QuickFIX/J + QuickFIX/n config generator with copy + download; prefills from a live session via `?session=<token>` (the session page's config card links here) |
| `/tools/order-state` | Order State Explorer (spec §30): interactive valid/invalid transition checker with the FIX messages behind each flow, plus reference copy |

Messages render within milliseconds of receipt over the WebSocket
stream, with auto-scroll + pause, direction badges (IN/OUT), 35= chips,
and a per-message server→browser latency readout. The UI components in
`components/ui/` follow shadcn/ui conventions (hand-rolled for the
phase-2 set: button, card, badge, input, select, separator).

Key design points:

- **QuickFIX/Go owns the session protocol.** Nothing reimplements
  framing, sequence numbers, or resend logic.
- **The guard is byte-level only.** It enforces size/timeout/rate
  limits by scanning FIX framing; it never interprets session state.
  The real acceptor binds loopback-only behind it.
- **Port bind is authoritative.** `PortManager.Acquire` probes each
  candidate with a real bind instead of trusting its map.
- **Admin messages never consume quota.** `A,0,1,2,4,5` are recorded
  in history but excluded from `AppMsgCount` (spec §4).
- **Tokens are 256-bit `crypto/rand`, `fixlab_` prefixed**, and are the
  only session identifier — no internal IDs leak (logs show a prefix).
- **Initiator SSRF: validate once, pin the IP, never re-resolve.**
  `security.ValidateInitiatorTarget` checks the port, the hostname
  blocklist, then every DNS-resolved IP against the blocked ranges, and
  pins one dial IP. The engine dials only that IP (the raw hostname is
  never handed to QuickFIX/Go), so DNS rebinding — at dial time and on
  reconnects — is structurally impossible. `StartInitiator`
  re-validates the pinned IP on the connect path, and restarts
  re-validate with fresh DNS. A custom session log observes QuickFIX/Go
  dial failures to feed the CONNECTION_FAILED reason; the connect
  watchdog stops redialing after the timeout.

## TLS (phase 7, spec §15)

TLS terminates **at the edge** — never inside the FIX engine:

```text
Developer FIX engine
        │ TLS 1.2+ (public pool port, Go tls.Listener, stdlib only)
        ▼
security.Guard ── TLS handshake (10s cap) ──► decrypt ──► the SAME
        │ byte-level pipeline: per-IP rate limit (pre-handshake),
        │ 8192-byte frame cap, idle/logon timeouts, min-heartbeat guard
        │ TCP (127.0.0.1, ephemeral port, plaintext)
        ▼
engine (QuickFIX/Go acceptor)
```

- **Acceptor sessions** (`"tls": true`): the public port runs a
  `tls.Listener` with `MinVersion: TLS 1.2` (Go standard library, no
  custom cryptography). Plaintext sent to a TLS port fails the
  handshake and is dropped before reaching the FIX engine — no
  protocol confusion is possible. The session response carries
  `endpoint.tls: true`, `certFingerprint` (SHA-256 hex), and
  `certSelfSigned`.
- **Initiator sessions** (`"tls": true`): FixLab dials the pinned IP
  and wraps the socket with `tls.Client` — TLS 1.2+, system root CAs,
  SNI/verification against the hostname you typed (never the dial IP,
  so SSRF IP-pinning is preserved). `FIXLAB_TLS_INSECURE_SKIP_VERIFY`
  exists for tests only.
- **Certificates**: `FIXLAB_TLS_CERT` / `FIXLAB_TLS_KEY` (PEM). Unset
  → a self-signed certificate is generated at startup, logged loudly
  as DEV-ONLY. Set-but-unloadable → the server still starts, but
  `tls:true` sessions are refused with a 400 naming the problem.
- The browser shows a TLS toggle on `/sandbox`; the session page's
  connection card shows the transport, the cert SHA-256 fingerprint,
  and a self-signed warning when applicable.

## Hardening (phase 7, spec §41/§44/§45)

Verification table — every item confirmed wired, not just configured:

| Spec item | Status | Where |
| --------- | ------ | ----- |
| Max FIX frame 8192 bytes | ✅ enforced (TLS too, on the decrypted stream) | `security.Guard` frame parser |
| Connection / idle / logon timeouts | ✅ enforced | `security.Guard` |
| Per-IP TCP connection rate limit (5/min) | ✅ enforced, pre-handshake for TLS | `security.Guard` + `ConnLimiter` |
| HTTP rate limit (10k/hr/IP) + max body 1 MiB | ✅ enforced | `api.Server.ServeHTTP` |
| 1 active session / IP (anonymous default) | ✅ enforced → 429 past the limit | `session.Manager.CreateSession` (`FIXLAB_MAX_SESSIONS_PER_IP`) |
| Token entropy (256-bit `crypto/rand`, `fixlab_` prefix) | ✅ | `security.GenerateSessionToken` |
| Session isolation + 4h TTL expiry + cleanup worker | ✅ destroys, closes listeners, releases ports | `session.Manager` + `RunCleanupLoop` |
| Initiator SSRF (private ranges, metadata, localhost; DNS rebinding impossible via IP pinning) | ✅ + spot-checked in e2e | `security.ValidateInitiatorTarget` |
| Metrics endpoint (`/api/v1/metrics`) | ✅ `active_sessions`, `fix_messages_in/out`, `sequence_errors`, `authentication_failures`, `session_expirations`, `rule_matches`, `execution_reports`, `guard_dropped_frames`, `guard_rate_limited`, `guard_idle_timeouts`, `guard_logon_timeouts`, `tls_handshake_failures` | `common.Metrics` |
| Structured JSON logs | ✅ `slog` JSON handler everywhere | `common.NewLogger` |
| Min heartbeat interval (10s) | ✅ logons below it are dropped | `security.Guard` |
| `tls:true` with no usable cert → 400 naming the problem | ✅ | `session.Manager.validate` |

## Docker

```bash
docker compose up --build
# backend  → http://localhost:8080 (REST/WS), FIX pool per compose ports
# frontend → http://localhost:3000
```

The image is multi-stage (Go builder → Node builder → single runtime
image running the backend on `:8080` and the Next.js standalone server
on `:3000`). The FIX pool is mapped as a documented subset by default
(see `docker-compose.yml`); widen it and set `FIXLAB_PORT_MIN` /
`FIXLAB_PORT_MAX` to match for more concurrent sandboxes. **Deploying
to production? Follow the step-by-step runbook:
[`deployment/DEPLOY.md`](deployment/DEPLOY.md)** (prerequisites,
env config, Let's Encrypt, TCP-load-balancer networking per spec §14,
security checklist, operations, troubleshooting) with the full env
template at [`deployment/.env.production.example`](deployment/.env.production.example).
The earlier production notes — TCP (not HTTP) load balancing per spec
§14, real certificates via `FIXLAB_TLS_CERT` / `FIXLAB_TLS_KEY`, env
reference, sizing — live in `deployment/README.md`.

## Package layout

```
backend/
├── cmd/server/        # main: config, manager, API, cleanup loop
├── cmd/fixclient/     # acceptance-test QuickFIX/Go initiator
├── cmd/fixacceptor/    # phase-5 scripted counterparty (accepts 35=D/F/G, replies with ERs)
├── cmd/mcp-server/     # phase-2.2 MCP server over stdio (thin wrapper over the REST API)
├── internal/
│   ├── api/           # REST API (create/get/delete, metrics, healthz)
│   ├── session/       # Manager, Session, PortManager
│   ├── engine/        # QuickFIX/Go adapter (acceptor + initiator)
│   ├── simulator/     # interface only (phase 3)
│   ├── orders/        # order types + blotter interface (phase 3)
│   ├── rules/         # rule types + engine interface (phase 4)
│   ├── messages/      # FIX message record + bounded ring buffer
│   ├── websocket/     # RFC 6455 event types + in-memory fan-out hub
│   ├── dictionary/    # embedded FIX dictionaries, tag→name/enum lookup
│   ├── security/      # tokens, rate limiter, TCP guard proxy
│   ├── storage/       # metadata store interface + in-memory impl
│   └── common/        # config, structured logging, metrics
├── specs/             # FIX40/42/44, FIXT11, FIX50SP2 XML (embedded)
└── e2e_phase1.sh      # end-to-end acceptance test
frontend/
├── app/               # /, /sandbox, /session/[token] (App Router)
├── components/        # ui/* (shadcn-style) + banner, feed, inspector…
├── lib/               # REST client, WebSocket hook, utils
├── store/             # Zustand session store
└── types/             # backend JSON contracts
```

## Tests

```bash
go test ./backend/...            # unit + integration (real engine/guard)
go test -race ./backend/...      # concurrency check
./backend/e2e_phase1.sh          # phase-1 live external-client acceptance
./backend/e2e_phase3.sh          # phase-3 trading-simulation acceptance
./backend/e2e_phase7.sh          # phase-7 hardening acceptance (TLS, rate limits)

cd frontend && npm run typecheck && npm run test:tools && npm run build
# test:tools runs the order-state transition-map assertions under node
```

## Known limitations (V1)

- FIX 4.4 / no-auth only; other versions and auth modes are rejected
  with a clear error, not silently ignored.
- One FIX session identity per process is effectively unique because
  QuickFIX/Go keeps a process-wide session registry — concurrent
  sandboxes (either role) must use distinct CompID pairs (collisions
  return a clean 400 from the engine path, e.g. "Duplicate SessionID").
- Inbound 35=D is acknowledged immediately with an ExecutionReport
  (ExecType=0/New, OrdStatus=0/New) so the client's order state stays
  consistent with the blotter; fills, rejects, cancels and replaces are
  driven from the browser or by deterministic rules.
- Initiator sessions dial the PINNED IP only (never the hostname again);
  the remote's 35=8/9 reports are validated strictly against the
  embedded FIX44 dictionary (unknown fields and out-of-range enums are
  session-rejected, exactly as any QuickFIX/Go counterparty would).
  Dictionary note: the shipped FIX44.xml was missing the standard
  `39=5` (REPLACED) enum value; it has been added back.
- Dev deployments use a self-signed TLS certificate: verify the
  `certFingerprint` out of band before trusting it. Production must
  mount a real certificate via `FIXLAB_TLS_CERT` / `FIXLAB_TLS_KEY`.
- The guard binds the public port on `0.0.0.0`; production puts this
  behind a TCP load balancer per spec §14 (see `deployment/README.md`).
  Single backend node for V1.
