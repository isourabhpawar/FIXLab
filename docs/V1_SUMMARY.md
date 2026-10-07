# FixLab V1 — Build Summary

> **SIMULATION ENVIRONMENT — NOT A LIVE TRADING VENUE**

FixLab V1 is complete: all seven roadmap phases implemented, each gated
by compile → unit tests → integration tests → a real external FIX
connection test. The full V1 spec's MUST list is covered. After V1,
the first four spec §48 "Phase 2 / Future" items were also built and
verified (stochastic simulator, MCP server, scenario recording +
replay, custom FIX dictionaries) — see "Phase 2 roadmap" below.
Still untouched from the MUST-NOT / future list: billing, SSO,
enterprise accounts, multi-user workspaces, load testing, FIXatdl,
CI/CD integration, AI FIX assistant, multi-broker routing.

## What was built, per phase

**Phase 1 — FIX engine foundation** (`backend/`, Go + QuickFIX/Go).
FIX 4.4 acceptor on dynamic ports (pool 10000–20000, bind is
authoritative), tokenized ephemeral sessions (`fixlab_` + 256-bit
`crypto/rand`), 4h TTL with a 60s cleanup worker, REST API
(`POST/GET/DELETE /api/v1/sessions`), message ring buffer (500),
app-message quota (250, admin messages exempt), embedded FIX
dictionaries. The TCP guard proxy fronts every session: 8192-byte
frame cap, per-IP connection rate limiting, idle/logon timeouts,
minimum-heartbeat guard. Verified: `e2e_phase1.sh` 18/18 — a real
external QuickFIX/Go client completes logon → heartbeats → TestRequest
→ logout; token auth, port release, and expiry cleanup proven.

**Phase 2 — Live browser** (`frontend/`, Next.js 15 + TypeScript +
Tailwind + Zustand). Session workspace at `/session/[token]`: status
pill, TTL countdown, connection card, live FIX feed over native
WebSocket (`/ws/session/{token}`) with per-message latency readout,
message inspector (SUMMARY / FIELDS / RAW / JSON), diagnostics tab.
Messages render within milliseconds of receipt.

**Phase 3 — Trading simulation.** Inbound 35=D/F/G handling, order
blotter (NEW → PARTIALLY_FILLED → FILLED / REJECTED / CANCELED /
REPLACED), browser execution controls (Fill, Partial Fill, Reject,
Cancel accept/reject, Replace accept/reject) generating
engine-validated ExecutionReports with CumQty/LeavesQty/AvgPx
bookkeeping. Quota enforced (429). Verified: `e2e_phase3.sh` 42/42
against the real external FIX client.

**Phase 4 — Deterministic rules + kill switch.** Rule DSL (predicates
`eq/ne/gt/lt/contains` on tags 11/1/54/55/38/40/44; actions
`ACK_NEW/FULL_FILL/PARTIAL_FILL/REJECT/DELAY`; priority order, first
match fires), kill switch rejecting every inbound NOS with
`VENUE HALTED` plus a 🔴 browser banner. Verified: `e2e_phase4.sh`
43/43 (priority proof, delay measured, kill-switch bypass of rules).

**Phase 5 — Initiator mode.** FixLab dials out to a developer's FIX
acceptor: SSRF validation at three layers (port, hostname blocklist,
DNS → every resolved IP checked) with **IP pinning** — the raw
hostname is never handed to QuickFIX/Go, so DNS rebinding is
structurally impossible; re-validation on every restart.
CONNECTING → TCP_CONNECTED → LOGON_SENT → LOGON_ACCEPTED chain,
CONNECTION_FAILED watchdog, browser order injection (35=D/F/G),
OUTBOUND blotter driven by remote ExecutionReports. Verified:
`e2e_phase5.sh` 46/46 against a scripted QuickFIX/Go counterparty
(`bin/fixacceptor`), including the full SSRF battery (all 400s).

**Phase 6 — Developer tools.** Stateless `POST /api/v1/tools/decode`
(SOH/`|`/`^A` delimiters, Tag 9/10 validation, dictionary-driven)
plus five free browser tools: decoder, checksum calculator, timestamp
converter, QuickFIX/J + QuickFIX/n config generator (session prefill),
and the order state explorer (valid/invalid transition checker).

**Phase 7 — Hardening.** TLS 1.2+ terminated at the edge for acceptor
sessions (Go `tls.Listener`, stdlib only; the guard's byte-level
protections apply to the decrypted stream unchanged) and on initiator
dials (pinned IP + `tls.Client`, system root CAs, SNI against the typed
hostname); cert via `FIXLAB_TLS_CERT`/`KEY`, self-signed dev fallback
logged loudly, `tls:true` with no usable cert → 400. Per-IP
active-session cap (default 1 → 429). Full metrics
(`/api/v1/metrics`), structured JSON logs. Docker: multi-stage build +
compose, `deployment/README.md` (TCP load balancer per spec §14,
Let's Encrypt guidance, env reference, sizing). Verified:
`e2e_phase7.sh` 28/28, `go test -race ./backend/...` green,
`tsc --noEmit` + `next build` green.

## Demoing the MVP acceptance test (spec §50)

Prerequisites: Go 1.23+, Node 18+.

```bash
go build -o bin/fixlab-server ./backend/cmd/server
go build -o bin/fixclient ./backend/cmd/fixclient
./bin/fixlab-server &                                   # REST on :8080
cd frontend && npm install && npm run dev               # UI on :3000
```

### Acceptor scenario

1. Open http://localhost:3000 → **Create Free FIX Sandbox**
   (FIX 4.4, ACCEPTOR, TCP — or tick TLS).
2. Note host / port / CompIDs (e.g. `127.0.0.1:10000`,
   `FIXLAB` → `MYCLIENT`).
3. Connect a real FIX engine:
   ```bash
   ./bin/fixclient --host 127.0.0.1 --port 10000 \
     --sender MYCLIENT --target FIXLAB --dict backend/specs/FIX44.xml
   # add --tls when the session was created with TLS
   ```
   Watch the browser go
   `WAITING_FOR_CONNECTION → TCP_CONNECTED → CONNECTED`.
4. In another terminal, script an order through the client
   (`--script`, then `NOS ORD-1 MSFT 1 100 2 310.50`), or use the
   browser's rule panel to auto-fill symbol `MSFT`.
5. The inbound `35=D` appears in the live feed; the order lands in the
   blotter. Click **Partial Fill** (qty + price) → the client receives
   the ExecutionReport; click **Fill** → final ER, order `FILLED`.

### Initiator scenario

1. Start the scripted counterparty:
   ```bash
   go build -o bin/fixacceptor ./backend/cmd/fixacceptor
   ./bin/fixacceptor --tls --port 11982 --sender REMOTE --target FIXLAB
   ```
2. Create an INITIATOR sandbox: remote host `127.0.0.1`, port `11982`,
   remote TargetCompID `REMOTE`, tick TLS. (Local testing needs
   `FIXLAB_TEST_ALLOW_PRIVATE=true` and
   `FIXLAB_TLS_INSECURE_SKIP_VERIFY=true` on the server — test-only.)
3. Browser shows `CONNECTING → TCP_CONNECTED → LOGON_SENT →
   LOGON_ACCEPTED`.
4. Inject a NewOrderSingle from the browser panel → the counterparty
   logs `NOS_RX` → its fill ExecutionReport streams back as
   `FIX_MSG_IN` → the OUTBOUND blotter flips to `FILLED`.

## Known limitations (V1 + phase 2)

- FIX 4.4 / auth NONE only; other versions and auth modes are rejected
  with a clear error naming the gap.
- One FIX session identity per process (QuickFIX/Go's process-wide
  session registry): concurrent sandboxes need distinct CompID pairs.
- Dev TLS uses a self-signed certificate — verify `certFingerprint`
  out of band; production must mount a real cert.
- Single backend node; the guard binds `0.0.0.0` and production needs a
  TCP load balancer in front (spec §14).
- Custom dictionaries affect inspection/decoding, not engine wire
  validation: QuickFIX/Go binds the data dictionary at session
  creation and can't swap it hot, so broker tags ≥ 5000 pass through
  (`ValidateUserDefinedFields=N`) while unknown standard tags
  (< 5000) are still wire-rejected like any strict counterparty.
- Replay re-enacts the application story synthetically (nothing on the
  wire, no quota consumed); rules/stochastic never fire during replay.
- `docker compose build` needs one run on a normal network (see
  `deployment/DEPLOY.md` — the build VM's registry egress is blocked).

## Phase 2 roadmap (spec §48 — built and verified after V1)

**2.1 — Stochastic simulator** (`e2e_phase2_1.sh` 57/57). Per-session
policy `{acceptPct, rejectPct, partialFillPct, avgLatencyMs, stdDevMs,
maxDelayMs, seed}`; outcomes drawn from a per-session RNG and executed
after a `Normal(avg, stddev)` latency sample. Evaluation order:
kill switch → deterministic rules → stochastic → manual ack; a fixed
seed reproduces the identical outcome sequence. A manual execution
racing a pending stochastic outcome wins; the stochastic outcome is
dropped with a warning (never double-executed). Simulation tab in the
workspace; `stochastic_outcomes` metric.

**2.2 — MCP server** (`bin/mcp-server`, `e2e_phase2_2.sh` 35/35).
Stdio MCP server (official Go SDK) wrapping the REST API only — eight
tools: `sandbox_create`, `sandbox_get_status`, `sandbox_list_messages`,
`sandbox_send_order`, `sandbox_send_execution`, `sandbox_send_raw`
(engine restamps header tags — documented), plus
`sandbox_get_scenario` and `sandbox_replay` from 2.3. Claude Code and
Cursor config snippets in the README.

**2.3 — Scenario recording + replay** (`e2e_phase2_3.sh` 29/29).
Recording is automatic and always on: a 500-step ring of logon,
inbound app messages (35=D/F/G), executions, and logout/disconnect —
admin chatter excluded. `POST /api/v1/sessions/replay` replays a
scenario into a fresh sandbox (synthetic re-injection marked
`replayed: true`; rules/stochastic don't fire; nothing on the wire, no
quota consumed), with a speed multiplier. Scenario tab with timeline
and JSON export; export never contains the session token.

**2.4 — Custom FIX dictionaries** (`e2e_phase2_4.sh` 25/25).
Per-session QuickFIX XML upload: strict validation (well-formed,
`<fix>` root, numeric field numbers, 512 KiB cap), atomic swap,
ephemeral (dies with the session). Custom tags resolve to names and
enum descriptions in the WS feed, message history, decoder, and MCP
message listings.

## What's next (spec §48, still not implemented)

Automated testing API (`POST /test-runs`), CI/CD integration, AI FIX
assistant ("why was my order rejected?", sequence-error explanations,
message generation). The modular monolith (`backend/internal/...`)
keeps every one of these as a new package or an extension of an
existing interface — no re-architecture needed.
