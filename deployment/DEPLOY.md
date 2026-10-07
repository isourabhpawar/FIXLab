# FixLab — Production Deployment Runbook

> **SIMULATION ENVIRONMENT — NOT A LIVE TRADING VENUE**

Step-by-step guide for a competent engineer who has never seen FixLab.
Assumes a normal network (this repo's dev VM cannot pull container
images; see "Known build limitation" below if that sounds like you).

## 0. What you are deploying

One Docker image, two compose services:

- **backend** — Go binary: FIX 4.4 acceptor + initiator engines
  (QuickFIX/Go), REST API + WebSocket on `:8080`, dynamic FIX port pool.
- **frontend** — Next.js 15 standalone server on `:3000`.

Ephemeral by design: **no database, no volumes required.** Sessions,
orders, messages, rules, and dictionaries all live in memory and die
with the container. There is nothing to back up — say this to your
on-call plainly: **a restart drops every active sandbox.**

## 1. Prerequisites

| Need | Guidance |
| ---- | -------- |
| Host | Single Linux VM, x86-64. 2 vCPU / 4 GB RAM comfortably holds hundreds of concurrent sandboxes (~2–4 MB per session: bounded 500-message history, 1000 orders, one QuickFIX session object, one WS fan-out). |
| Software | Docker Engine 24+ with the compose plugin. |
| DNS | An A record for your domain, e.g. `fixlab.example.com` → VM IP. |
| Ports (inbound) | `80`/`443` browsers → reverse proxy; `8080` backend API/WS (or keep internal behind the proxy); `3000` frontend (or behind the proxy); **FIX pool TCP** `10000–10099` (widen to `10000–20000` for capacity) — reachable by FIX clients. |

## 2. Get the code and configure

```bash
git clone <your-fixlab-remote> && cd fixlab
cp deployment/.env.production.example deployment/.env.production
$EDITOR deployment/.env.production
```

Every `FIXLAB_*` variable is documented in that file. The decisions
that matter most:

- `FIXLAB_PUBLIC_HOST=fixlab.example.com` — this hostname is what
  session responses advertise to FIX clients. Get it wrong and clients
  dial the wrong host.
- `FIXLAB_CORS_ORIGIN=https://fixlab.example.com` — lock this down;
  the dev default `*` is only acceptable because tokens are bearer
  credentials in the URL path, never cookies.
- `FIXLAB_PORT_MIN`/`FIXLAB_PORT_MAX` must match the published port
  range in `docker-compose.yml` (default `10000–10099` = ~100 concurrent
  ACCEPTOR sessions; widen both together for more).
- `FIXLAB_MAX_SESSIONS_PER_IP=1` is the anonymous default (spec §44);
  raise it if one NAT-ed office creates many sandboxes.

`NEXT_PUBLIC_FIXLAB_API_URL` is **baked into the frontend bundle at
build time**. If browsers reach the backend at anything other than
`http://localhost:8080`, pass it as a build arg (see step 5).

## 3. TLS certificates

FIX edge TLS (`"tls": true` sessions) terminates **in the backend**
using `FIXLAB_TLS_CERT` / `FIXLAB_TLS_KEY` (PEM). Get a real
certificate — Let's Encrypt:

```bash
sudo certbot certonly --standalone -d fixlab.example.com
mkdir -p certs
sudo cp /etc/letsencrypt/live/fixlab.example.com/fullchain.pem certs/fixlab.crt
sudo cp /etc/letsencrypt/live/fixlab.example.com/privkey.pem certs/fixlab.key
sudo chown -R $USER certs && chmod 600 certs/fixlab.key
```

Then uncomment in `docker-compose.yml`:

```yaml
environment:
  FIXLAB_TLS_CERT: /certs/fixlab.crt
  FIXLAB_TLS_KEY: /certs/fixlab.key
volumes:
  - ./certs:/certs:ro
```

Without these mounts the server generates a **self-signed** certificate
at startup (logged loudly as DEV-ONLY). Each TLS session response
carries `certFingerprint` (SHA-256) so clients can verify out of band —
fine for testing, not for production. If the paths are set but the
files can't be loaded, the server still starts but `tls:true` sessions
are refused with a 400 naming the problem.

For the HTTP edge (browsers → frontend/backend), terminate TLS at a
reverse proxy (Caddy/nginx) in front of `:3000`/`:8080`, or reuse the
same certificates.

## 4. Build and start

```bash
docker compose --env-file deployment/.env.production up -d --build
```

With a different browser-facing API origin:

```bash
docker compose --env-file deployment/.env.production build \
  --build-arg NEXT_PUBLIC_FIXLAB_API_URL=https://fixlab.example.com/api
docker compose --env-file deployment/.env.production up -d
```

Check health:

```bash
curl -s http://localhost:8080/healthz
curl -s http://localhost:3000/ -o /dev/null -w '%{http_code}\n'
docker compose ps
```

## 5. Smoke test (spec §50 MVP acceptance)

From the deploy host (needs Go 1.23+ for the test client, or use any
FIX engine — QuickFIX/J, QuickFIX/n, etc.):

```bash
# 1. Create a sandbox
TOKEN=$(curl -s -X POST http://localhost:8080/api/v1/sessions \
  -H 'Content-Type: application/json' \
  -d '{"targetCompId":"SMOKE"}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["sessionToken"])')
PORT=$(curl -s http://localhost:8080/api/v1/sessions/$TOKEN \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["endpoint"]["port"])')
echo "token=$TOKEN port=$PORT"

# 2. Connect a real FIX engine (repo's QuickFIX/Go test client)
go run ./backend/cmd/fixclient --host 127.0.0.1 --port $PORT \
  --sender SMOKE --target FIXLAB --dict backend/specs/FIX44.xml --stay 30s &
CLIENT=$!

# 3. While it is connected: full order lifecycle via the API
sleep 6
CLORD=SMOKE-1
curl -s -X POST http://localhost:8080/api/v1/sessions/$TOKEN/orders/$CLORD/execute \
  -H 'Content-Type: application/json' \
  -d '{"action":"FILL","qty":100,"price":50.25}' | head -c 300; echo
# (send a 35=D for $CLORD from your engine first; the client above only
#  does session-level traffic — use QuickFIX/J with the generated config
#  from /tools/config for the full scripted run)

# 4. Housekeeping checks
curl -s http://localhost:8080/api/v1/metrics | python3 -m json.tool | head -20
curl -s -X DELETE http://localhost:8080/api/v1/sessions/$TOKEN -w '%{http_code}\n'
wait $CLIENT
```

The repo's scripted suites (`./backend/e2e_phase*.sh`) encode the full
acceptance battery if you want machine-checked proof on the deploy
host.

## 6. Critical networking (spec §14) — read this before exposing FIX ports

```
FIX clients ──TCP/TLS──> TCP load balancer ──> backend FIX port pool
browsers ──────────────> HTTP reverse proxy ──> frontend:3000, backend:8080
```

- Raw FIX sessions are long-lived plain-TCP (or TLS) byte streams. An
  **HTTP-only load balancer will try to parse them and break them.**
  Put a **TCP (Layer-4) load balancer** — or direct port exposure — in
  front of the FIX port pool. This is the single most common
  misconfiguration.
- Single backend node is acceptable for V1. Sessions are in-memory;
  horizontal scaling needs sticky sessions and is out of scope.
- The guard binds the public FIX ports on `0.0.0.0`; firewall the pool
  to expected client IP ranges if you can.

## 7. Security checklist

- [ ] `FIXLAB_TEST_ALLOW_PRIVATE` is **unset** (test-only; bypasses SSRF
      private-range blocking so the suite can dial 127.0.0.1).
- [ ] `FIXLAB_TLS_INSECURE_SKIP_VERIFY` is `false` (test-only; skips
      remote-cert verification on initiator dials).
- [ ] Real certificate mounted via `FIXLAB_TLS_CERT`/`FIXLAB_TLS_KEY`
      (not the dev self-signed one).
- [ ] `FIXLAB_CORS_ORIGIN` set to your exact frontend origin.
- [ ] Session tokens are **bearer secrets in the URL path**
      (`/session/<token>`): anyone with the token owns the sandbox.
      Never log them, never put them in shared screenshots.
- [ ] Rate-limit defaults are sane (5 TCP connects/min/IP, 10k HTTP
      req/hour/IP, 1 session/IP); raise `FIXLAB_MAX_SESSIONS_PER_IP`
      deliberately, not reflexively.
- [ ] FIX pool firewalled to client IPs where possible.
- [ ] Image rebuilt from a clean checkout on updates; no ad-hoc
      `docker exec` changes.

## 8. Operations

- **Expiry:** sessions live `FIXLAB_SESSION_TTL` (default 4h); the
  cleanup worker runs every `FIXLAB_CLEANUP_INTERVAL` (60s) and
  destroys expired sessions: stops the FIX session, closes the TCP/TLS
  listener, closes WebSockets, releases the port, clears state.
- **Logs:** `docker compose logs backend` / `docker compose logs frontend`.
  Structured JSON via `slog`. `FIXLAB_LOG_FIX_MESSAGES=1` traces raw
  FIX traffic when debugging (noisy — don't leave it on).
- **Metrics:** `GET /api/v1/metrics` — `active_sessions`,
  `fix_messages_in/out`, `sequence_errors`, `authentication_failures`,
  `session_expirations`, `rule_matches`, `execution_reports`,
  `guard_dropped_frames`, `guard_rate_limited`, `guard_idle_timeouts`,
  `guard_logon_timeouts`, `tls_handshake_failures`, `stochastic_outcomes`.
  Scrape it or `watch` it.
- **Updating:** `git pull && docker compose --env-file
  deployment/.env.production up -d --build`. Sessions do not survive
  restarts — schedule updates off-peak and say so in the change note.
- **No backups.** There is no persistent state to back up.

## 9. Troubleshooting

| Symptom | Likely cause |
| ------- | ------------ |
| `400` naming a phase on session create | Requested FIX version/auth/transport isn't implemented (FIX 4.4 + NONE only in V1) |
| FIX client can't reach the port | Firewall/security group, or an HTTP LB in front of the FIX pool (§6) |
| `tls:true` → 400 about certificates | `FIXLAB_TLS_CERT`/`KEY` set but unloadable, or cert files not mounted |
| WS `404` / API `404` on a token | Typo'd token, or the session expired/was destroyed (tokens are the only ID) |
| Order execute → `429` | App-message quota (`FIXLAB_APP_MSG_LIMIT`, default 250) exhausted |
| Order execute → `409` | State conflict — e.g. filling an already-terminal order |
| Initiator stuck at `CONNECTING` → `CONNECTION_FAILED` | Remote unreachable, or SSRF validation rejected it (400 at create time names the violation) |
| Container exits immediately | `docker compose logs backend` — usually a bad env value (durations like `4h`, ints) or port clash |

## Known build limitation (dev VM only)

The sandbox VM where this repo was built cannot pull container-registry
images (the Docker daemon's containerd transfer service ignores the
egress proxy, so `registry-1.docker.io` is unreachable from the daemon
even though `curl` reaches it). `docker compose build` therefore needs
**one run on a normal network**. Statically validated instead: every
Dockerfile instruction reviewed, `docker compose config` green, the Go
builder stage reproduced byte-for-byte (`CGO_ENABLED=0 go build
-trimpath`, 14 MB static ELF), `output:"standalone"` confirmed in
`next.config.ts`, entrypoint syntax-checked, ports and env ranges
cross-checked between compose and `FIXLAB_PORT_MIN/MAX`.
