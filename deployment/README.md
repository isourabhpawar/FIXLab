# FixLab Deployment

How to run FixLab in production with Docker Compose. The image
(`Dockerfile` at the repo root) is multi-stage: it compiles the Go backend
and the Next.js frontend into a single deployable image; `docker-compose.yml`
runs the two services separately via `command:` overrides
(`entrypoint backend` / `entrypoint frontend`).

## Quick start

```bash
cd ~/workspace/fixlab
FIXLAB_PUBLIC_HOST=fixlab.example.com docker compose up -d --build
```

- Backend REST/WS: `http://<host>:8080` (`/healthz` for liveness)
- Frontend: `http://<host>:3000`

`NEXT_PUBLIC_FIXLAB_API_URL` is baked into the frontend bundle at build
time, so the browser reaches the backend at the right host. Override the
build arg when browsers use a different host:

```bash
docker compose build --build-arg NEXT_PUBLIC_FIXLAB_API_URL=https://api.fixlab.example.com
```

## Production networking (spec §14)

- Put a **TCP load balancer — NOT an HTTP load balancer** — in front of the
  FIX gateway. Raw FIX sessions are long-lived plain-TCP (or TLS) streams;
  an HTTP LB will try to parse them and break them. Acceptable V1 topology:
  ```
  FIX clients ──TLS──> TCP LB ──> backend:8080 (REST/WS) + FIX port pool
                       browsers ──> frontend:3000
  ```
- **Single node is acceptable for V1.** Run one backend replica with the
  FIX port pool mapped on the host (see below); sessions are in-memory and
  ephemeral, so horizontal scaling needs sticky sessions and is out of scope
  for V1.

## TLS in production

In production, terminate TLS with real certificates (Let's Encrypt via
`cert-manager` or your ingress) and mount the cert + key into the backend
container, pointing at these variables:

| Variable | Meaning |
| -------- | ------- |
| `FIXLAB_TLS_CERT` | Path to the PEM certificate the FIX/HTTP edge serves |
| `FIXLAB_TLS_KEY`  | Path to the PEM private key |
| `FIXLAB_TLS_INSECURE_SKIP_VERIFY` | Initiator-side: skip verifying the *remote* counterparty's cert. Keep `false` in production |

Compose snippet:

```yaml
volumes:
  - ./certs:/certs:ro
environment:
  FIXLAB_TLS_CERT: /certs/fixlab.crt
  FIXLAB_TLS_KEY: /certs/fixlab.key
```

Without these mounts the dev image generates a self-signed certificate at
startup. The frontend shows each TLS session's **SHA-256 certificate
fingerprint** (session page → Connection → Cert SHA-256); verify that
fingerprint out of band before trusting the connection.

## Environment reference

Full backend configuration (from the main README's Configuration section):

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `FIXLAB_HTTP_ADDR` | `:8080` | REST API listen address |
| `FIXLAB_PUBLIC_HOST` | `127.0.0.1` | Host reported in session endpoints |
| `FIXLAB_SESSION_TTL` | `4h` | Sandbox lifetime |
| `FIXLAB_CLEANUP_INTERVAL` | `60s` | Expiry worker period |
| `FIXLAB_PORT_MIN` / `FIXLAB_PORT_MAX` | `10000` / `20000` | Dynamic FIX port pool |
| `FIXLAB_INTERNAL_BIND` | `127.0.0.1` | Internal QuickFIX acceptor bind — keep loopback-only |
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
| `FIXLAB_TLS_CERT` | unset | **Phase 7.** PEM certificate served by the TLS edge |
| `FIXLAB_TLS_KEY` | unset | **Phase 7.** PEM private key for the TLS edge |
| `FIXLAB_TLS_INSECURE_SKIP_VERIFY` | `false` | **Phase 7.** Skip remote-cert verification for initiator sessions; TEST/dev only, never production |
| `FIXLAB_TEST_ALLOW_PRIVATE` | unset | **TEST ONLY — never enable in production.** Bypasses private-range SSRF blocking so the acceptance suite can dial a 127.0.0.1 counterparty |

Frontend (standalone server):

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `PORT` | `3000` | Next.js standalone listen port |
| `HOSTNAME` | — | Set `0.0.0.0` inside the container |
| `NEXT_PUBLIC_FIXLAB_API_URL` | `http://localhost:8080` | Backend origin, **baked in at build time** |

## Resource sizing

- **Memory per session:** small. Each session holds bounded history
  (`FIXLAB_MSG_HISTORY_CAP`=500 messages, `FIXLAB_ORDER_CAP`=1000 orders,
  `FIXLAB_APP_MSG_LIMIT`=250 app messages) plus one QuickFIX session object
  and a WebSocket. Budget **~2–4 MB per active session**; a 2 GB container
  comfortably holds a few hundred concurrent sandboxes.
- **Port pool sizing:** each ACCEPTOR session consumes one TCP port from
  `FIXLAB_PORT_MIN..FIXLAB_PORT_MAX`, and the port must be published through
  Docker/LB. The compose file maps `10000-10099` (100 ports ≈ 100 concurrent
  ACCEPTOR sessions); raise `FIXLAB_PORT_MAX` and the published range to
  `10000-20000` for production capacity.
- **CPU:** sessions are mostly idle between FIX heartbeats; one vCPU serves
  hundreds of sessions. The rule engine and order simulation are
  deterministic and cheap; burst cost comes from concurrent logon handshakes
  and WS broadcasts.

## Operations

- Healthchecks are built into compose: backend `GET /healthz`, frontend
  `GET /`.
- Sessions expire after `FIXLAB_SESSION_TTL` (default 4h); no persistent
  state — the container is stateless and can be recreated freely.
- Logs: `docker compose logs backend` / `docker compose logs frontend`.
  Set `FIXLAB_LOG_FIX_MESSAGES=1` to trace raw FIX traffic when debugging.
