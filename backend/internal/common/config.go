// Package common holds shared configuration, logging, and metrics
// primitives used across the FixLab backend.
package common

import (
	"os"
	"strconv"
	"time"
)

// Config carries all server-side tunables for FixLab. Every value has a
// sane default and can be overridden with an environment variable, so
// quotas and limits stay configurable without code changes (spec §4, §44).
type Config struct {
	// HTTPAddr is the address the REST API listens on.
	HTTPAddr string
	// PublicHost is the hostname reported in session endpoint details.
	PublicHost string

	// SessionTTL is how long a sandbox lives before automatic destruction.
	SessionTTL time.Duration
	// CleanupInterval controls how often the expiry worker runs.
	CleanupInterval time.Duration

	// PortPoolMin/Max bound the dynamic FIX port pool (spec §13).
	PortPoolMin int
	PortPoolMax int

	// Quotas (spec §4).
	AppMessageLimit   int64
	MessageHistoryCap int
	OrderCap          int

	// MinHeartBtInt is the minimum acceptable HeartBtInt (tag 108) on
	// inbound Logon messages. Aggressive heartbeats are rejected to
	// protect resources (spec §43).
	MinHeartBtInt int

	// TCP guard settings (spec §41).
	MaxFIXFrameBytes  int
	TCPIdleTimeout    time.Duration
	TCPLogonTimeout   time.Duration
	TCPConnRatePerMin int // connection attempts per minute per IP

	// HTTP API rate limiting (spec §41).
	HTTPRequestsPerHourPerIP int
	HTTPMaxBodyBytes         int64

	// InternalBindHost is where the QuickFIX/Go acceptor binds. It must be
	// loopback-only: the public listener is the guard proxy, which applies
	// rate limiting, idle timeouts, and frame-size enforcement.
	InternalBindHost string

	// CORSOrigin is the Access-Control-Allow-Origin value served by the
	// HTTP API (and the allowed WebSocket origin). The Next.js dev server
	// runs on a different port, so the browser UI needs this. "*" is
	// acceptable here because session tokens are bearer credentials in
	// the URL path, not cookies.
	CORSOrigin string

	// Initiator settings (phase 5, spec §9).
	// InitiatorConnectTimeout bounds the initial outbound dial+logon;
	// if the session is not logged on by then it moves to
	// CONNECTION_FAILED and the initiator stops dialing.
	InitiatorConnectTimeout time.Duration
	// InitiatorMaxOutbound caps concurrent initiator sessions per server.
	InitiatorMaxOutbound int
	// InitiatorDialRatePerMin caps outbound dial attempts per destination
	// IP per minute (protects remote targets from dial spam).
	InitiatorDialRatePerMin int
	// InitiatorDNSTimeout bounds the SSRF DNS resolution step.
	InitiatorDNSTimeout time.Duration

	// TLS settings (phase 7, spec §15).
	// TLSCertFile/TLSKeyFile point at a PEM certificate and private key
	// used to terminate TLS on acceptor session ports. When both are
	// unset the server generates a self-signed certificate at startup
	// (dev-only; logged loudly).
	TLSCertFile string
	TLSKeyFile  string
	// TLSInsecureSkipVerify disables remote certificate verification
	// for INITIATOR TLS dials. TEST ONLY — never enable in production.
	TLSInsecureSkipVerify bool

	// MaxSessionsPerIP caps concurrently active sessions per client IP
	// (spec §44 anonymous default: 1).
	MaxSessionsPerIP int
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func getenvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// LoadConfig builds a Config from the environment, falling back to the
// spec defaults.
func LoadConfig() Config {
	return Config{
		HTTPAddr:   getenv("FIXLAB_HTTP_ADDR", ":8080"),
		PublicHost: getenv("FIXLAB_PUBLIC_HOST", "127.0.0.1"),

		SessionTTL:      getenvDuration("FIXLAB_SESSION_TTL", 4*time.Hour),
		CleanupInterval: getenvDuration("FIXLAB_CLEANUP_INTERVAL", 60*time.Second),

		PortPoolMin: getenvInt("FIXLAB_PORT_MIN", 10000),
		PortPoolMax: getenvInt("FIXLAB_PORT_MAX", 20000),

		AppMessageLimit:   getenvInt64("FIXLAB_APP_MSG_LIMIT", 250),
		MessageHistoryCap: getenvInt("FIXLAB_MSG_HISTORY_CAP", 500),
		OrderCap:          getenvInt("FIXLAB_ORDER_CAP", 1000),

		MinHeartBtInt: getenvInt("FIXLAB_MIN_HEARTBEAT_INT", 10),

		MaxFIXFrameBytes:  getenvInt("FIXLAB_MAX_FIX_FRAME", 8192),
		TCPIdleTimeout:    getenvDuration("FIXLAB_TCP_IDLE_TIMEOUT", 5*time.Minute),
		TCPLogonTimeout:   getenvDuration("FIXLAB_TCP_LOGON_TIMEOUT", 30*time.Second),
		TCPConnRatePerMin: getenvInt("FIXLAB_TCP_CONN_RATE_PER_MIN", 5),

		HTTPRequestsPerHourPerIP: getenvInt("FIXLAB_HTTP_RATE_PER_HOUR", 10000),
		HTTPMaxBodyBytes:         getenvInt64("FIXLAB_HTTP_MAX_BODY", 1<<20),

		InternalBindHost: getenv("FIXLAB_INTERNAL_BIND", "127.0.0.1"),

		CORSOrigin: getenv("FIXLAB_CORS_ORIGIN", "*"),

		InitiatorConnectTimeout: getenvDuration("FIXLAB_INITIATOR_CONNECT_TIMEOUT", 10*time.Second),
		InitiatorMaxOutbound:    getenvInt("FIXLAB_INITIATOR_MAX_OUTBOUND", 50),
		InitiatorDialRatePerMin: getenvInt("FIXLAB_INITIATOR_DIAL_RATE_PER_MIN", 10),
		InitiatorDNSTimeout:     getenvDuration("FIXLAB_INITIATOR_DNS_TIMEOUT", 5*time.Second),

		TLSCertFile:           getenv("FIXLAB_TLS_CERT", ""),
		TLSKeyFile:            getenv("FIXLAB_TLS_KEY", ""),
		TLSInsecureSkipVerify: getenv("FIXLAB_TLS_INSECURE_SKIP_VERIFY", "") == "true",

		MaxSessionsPerIP: getenvInt("FIXLAB_MAX_SESSIONS_PER_IP", 1),
	}
}
