// Initiator support (phase 5, spec §8): FixLab dials OUT to a
// developer-supplied FIX acceptor.
//
// QuickFIX/Go owns the session protocol on this side too — logon,
// heartbeats, resends, logoff. This file only wires the outbound
// connection and translates engine activity into FixLab's status chain:
//
//	CONNECTING → TCP_CONNECTED → LOGON_SENT → LOGON_ACCEPTED
//	                                          ↘ CONNECTION_FAILED (reason)
//
// SSRF design (spec §9):
//   - The manager validates the target with security.ValidateInitiatorTarget
//     BEFORE StartInitiator is called. Validation pins a single dial IP.
//   - StartInitiator dials ONLY cfg.DialIP (the pinned IP) via
//     SocketConnectHost. The raw hostname is never handed to QuickFIX/Go,
//     so there is no DNS re-resolution at dial time and none on
//     QuickFIX/Go reconnects either — DNS rebinding is structurally
//     impossible.
//   - As defense in depth, StartInitiator re-validates the pinned dial IP
//     with security.ValidateDialIP (no DNS, just the range check).
//   - TLS (phase 7, spec §15): the TCP dial still goes to the pinned IP,
//     and QuickFIX/Go wraps it with tls.Client (stdlib, TLS 1.2+,
//     system root CAs). SNI/verification uses cfg.RemoteHost — the
//     hostname the user typed — never the dial IP.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"

	"fixlab.dev/fixlab/backend/internal/common"
	"fixlab.dev/fixlab/backend/internal/security"
)

// InitiatorConfig configures one outbound QuickFIX/Go initiator.
type InitiatorConfig struct {
	// RemoteHost is the original hostname as entered by the user. It is
	// for display and logs ONLY — it is never dialed.
	RemoteHost string
	// DialIP is the pinned, SSRF-validated IP address. This is the only
	// address the initiator ever dials (spec §9 DNS-rebinding protection).
	DialIP string
	// RemotePort is the remote acceptor's TCP port.
	RemotePort int
	// RemoteCompID is the remote acceptor's CompID (our TargetCompID).
	RemoteCompID string
	// LocalCompID is FixLab's SenderCompID on this session.
	LocalCompID string
	// BeginString is the FIX version (phase 5: only "FIX.4.4").
	BeginString string
	// TLS dials the remote acceptor over TLS (phase 7, spec §15). The
	// dial still targets the pinned IP; SNI/verification uses
	// RemoteHost; verification uses the system root CAs.
	TLS bool
	// InsecureSkipVerify disables remote certificate verification.
	// TEST ONLY (FIXLAB_TLS_INSECURE_SKIP_VERIFY) — never in
	// production.
	InsecureSkipVerify bool
	// DataDictionaryPath is a filesystem path to the FIX data dictionary
	// XML (QuickFIX/Go requires a real file).
	DataDictionaryPath string
	Hooks              Hooks
	// MsgName resolves 35= values to message names (may be nil).
	MsgName func(msgType string) string
	Logger  *slog.Logger
	Metrics *common.Metrics
	// ConnectTimeout bounds dial+logon. If the session is not logged on
	// when it expires, OnConnectFailed fires once and the initiator
	// stops dialing (spec §9: connection timeout).
	ConnectTimeout time.Duration
	// OnFirstActivity fires once on the first FIX message in either
	// direction — i.e. the TCP dial succeeded and the session is live.
	// The session layer promotes CONNECTING → TCP_CONNECTED from it.
	OnFirstActivity func()
	// OnConnectFailed fires once when the connect timeout expires
	// without a logon, carrying the reason (last dial error when the
	// dial itself kept failing, otherwise a logon-timeout description).
	OnConnectFailed func(reason string)
}

func (m *manager) StartInitiator(_ context.Context, cfg InitiatorConfig) (Handle, error) {
	if cfg.Hooks == nil {
		return nil, fmt.Errorf("engine: hooks are required")
	}
	if cfg.DataDictionaryPath == "" {
		return nil, fmt.Errorf("engine: data dictionary path is required")
	}
	if cfg.BeginString != "" && cfg.BeginString != "FIX.4.4" {
		return nil, fmt.Errorf("engine: FIX version %q not supported in phase 5 (only FIX.4.4)", cfg.BeginString)
	}
	if cfg.DialIP == "" {
		return nil, fmt.Errorf("engine: pinned dial IP is required (SSRF validation must run first)")
	}
	// Defense in depth: re-validate the pinned dial IP on the connect
	// path. No DNS here — just the range check.
	if err := security.ValidateDialIP(cfg.DialIP); err != nil {
		return nil, fmt.Errorf("engine: dial IP rejected: %w", err)
	}
	if cfg.RemotePort < 1 || cfg.RemotePort > 65535 {
		return nil, fmt.Errorf("engine: remote port %d out of range (1-65535)", cfg.RemotePort)
	}
	if cfg.LocalCompID == "" || cfg.RemoteCompID == "" {
		return nil, fmt.Errorf("engine: local and remote CompIDs are required")
	}
	beginString := cfg.BeginString
	if beginString == "" {
		beginString = "FIX.4.4"
	}
	connectTimeout := cfg.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}

	// SocketConnectTimeout bounds each TCP dial; the watchdog below
	// bounds the whole dial+logon phase.
	dialTimeoutSec := int(connectTimeout.Seconds())
	if dialTimeoutSec < 1 {
		dialTimeoutSec = 1
	}
	// TLS (phase 7): QuickFIX/Go wraps the pinned-IP TCP dial with
	// tls.Client — TLS 1.2+, system root CAs, SNI/verification against
	// the user-typed hostname (SocketServerName). InsecureSkipVerify is
	// test-only (FIXLAB_TLS_INSECURE_SKIP_VERIFY).
	tlsSettings := ""
	if cfg.TLS {
		serverName := cfg.RemoteHost
		if serverName == "" {
			serverName = cfg.DialIP
		}
		tlsSettings = fmt.Sprintf("SocketUseSSL=Y\nSocketServerName=%s\n", serverName)
		if cfg.InsecureSkipVerify {
			tlsSettings += "SocketInsecureSkipVerify=Y\n"
		}
	}
	settingsText := fmt.Sprintf(`[DEFAULT]
ConnectionType=initiator
SocketConnectHost=%s
SocketConnectPort=%d
SocketConnectTimeout=%d
%sBeginString=%s
SenderCompID=%s
TargetCompID=%s
DataDictionary=%s
ValidateUserDefinedFields=N
HeartBtInt=30
ReconnectInterval=2
ResetOnLogon=Y
[SESSION]
BeginString=%s
SenderCompID=%s
TargetCompID=%s
`,
		cfg.DialIP, cfg.RemotePort, dialTimeoutSec,
		tlsSettings,
		beginString, cfg.LocalCompID, cfg.RemoteCompID,
		cfg.DataDictionaryPath,
		beginString, cfg.LocalCompID, cfg.RemoteCompID,
	)
	settings, err := quickfix.ParseSettings(strings.NewReader(settingsText))
	if err != nil {
		return nil, fmt.Errorf("engine: parse settings: %w", err)
	}

	h := &initiatorHandle{logger: cfg.Logger}
	app := &app{
		hooks:   cfg.Hooks,
		msgName: cfg.MsgName,
		logger:  cfg.Logger,
		metrics: cfg.Metrics,
	}
	// The once-wrapper is belt-and-braces: record() already nils the
	// field under the app mutex, so this fires at most once.
	if cfg.OnFirstActivity != nil {
		once := &sync.Once{}
		cb := cfg.OnFirstActivity
		app.firstActivity = func() { once.Do(cb) }
	}
	logFactory := &observingLogFactory{
		onDialError: func(err error) { h.recordDialError(err) },
	}
	initiator, err := quickfix.NewInitiator(
		app,
		quickfix.NewMemoryStoreFactory(), // ephemeral by design (spec §39)
		settings,
		logFactory,
	)
	if err != nil {
		return nil, fmt.Errorf("engine: create initiator: %w", err)
	}
	h.initiator = initiator
	h.app = app
	if err := initiator.Start(); err != nil {
		return nil, fmt.Errorf("engine: start initiator: %w", err)
	}

	// Watchdog: no logon within ConnectTimeout → CONNECTION_FAILED and
	// stop dialing. One-shot; after a successful logon, reconnects are
	// left to QuickFIX/Go (OnLogout → CONNECTING → …).
	h.timer = time.AfterFunc(connectTimeout, func() {
		if app.isLoggedOn() {
			return
		}
		reason := fmt.Sprintf("no logon within %s", connectTimeout)
		if derr := h.lastDialError(); derr != nil {
			reason = fmt.Sprintf("dial failed: %v", derr)
		}
		if cfg.Logger != nil {
			cfg.Logger.Warn("engine: initiator connect timeout",
				"remote_host", cfg.RemoteHost, "dial_ip", cfg.DialIP,
				"reason", reason)
		}
		if cfg.OnConnectFailed != nil {
			cfg.OnConnectFailed(reason)
		}
		h.Stop()
	})
	return h, nil
}

// initiatorHandle is a running outbound FIX session.
type initiatorHandle struct {
	initiator *quickfix.Initiator
	app       *app
	logger    *slog.Logger
	timer     *time.Timer
	once      sync.Once

	mu          sync.Mutex
	lastDialErr error
}

func (h *initiatorHandle) recordDialError(err error) {
	h.mu.Lock()
	h.lastDialErr = err
	h.mu.Unlock()
}

func (h *initiatorHandle) lastDialError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastDialErr
}

// BoundPort reports 0: initiators bind no local port (there is no
// listener and no guard proxy — documented in the phase-5 spec notes).
func (h *initiatorHandle) BoundPort() int { return 0 }

// SendMessage transmits msg through the logged-on FIX session. It fails
// fast when the counterparty is not logged on instead of silently
// queueing the message in QuickFIX/Go's send queue.
func (h *initiatorHandle) SendMessage(msg *quickfix.Message) error {
	sid, ok := h.app.sessionID()
	if !ok || !h.app.isLoggedOn() {
		return fmt.Errorf("engine: no logged-on FIX session to send to")
	}
	return quickfix.SendToTarget(msg, sid)
}

func (h *initiatorHandle) Stop() error {
	h.once.Do(func() {
		if h.timer != nil {
			h.timer.Stop()
		}
		h.initiator.Stop()
	})
	return nil
}

// observingLogFactory produces session logs that watch QuickFIX/Go's
// dial lifecycle. The dial outcome ("Failed to connect", "Failed to
// initiate", "Failed handshake") is the only honest signal QuickFIX/Go
// exposes about the TCP dial, and it feeds the CONNECTION_FAILED reason.
type observingLogFactory struct {
	onDialError func(err error)
}

func (f *observingLogFactory) Create() (quickfix.Log, error) {
	return &observingLog{onDialError: f.onDialError}, nil
}

func (f *observingLogFactory) CreateSessionLog(_ quickfix.SessionID) (quickfix.Log, error) {
	return &observingLog{onDialError: f.onDialError}, nil
}

type observingLog struct {
	onDialError func(err error)
}

func (l *observingLog) OnIncoming(_ []byte) {}
func (l *observingLog) OnOutgoing(_ []byte) {}
func (l *observingLog) OnEvent(_ string)    {}
func (l *observingLog) OnEventf(format string, a ...interface{}) {
	if l.onDialError == nil {
		return
	}
	// QuickFIX/Go dial-failure lines (initiator.go): "Failed to
	// connect: %v", "Failed to initiate: %v", "Failed handshake: %v".
	for _, prefix := range []string{"Failed to connect: ", "Failed to initiate: ", "Failed handshake: "} {
		if strings.HasPrefix(format, prefix) {
			msg := fmt.Sprintf(format, a...)
			l.onDialError(fmt.Errorf("%s", strings.TrimPrefix(msg, prefix)))
			return
		}
	}
}
