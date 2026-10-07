// Package engine adapts QuickFIX/Go to FixLab's session model.
//
// The FIX engine owns session state, framing, sequence numbers,
// heartbeats, TestRequest, ResendRequest, SequenceReset, Logon and Logout
// (spec §10). Nothing here reimplements the FIX session protocol: all of
// that is delegated to QuickFIX/Go. This package translates engine events
// into FixLab's engine-agnostic FIXEvent stream so the engine can be
// replaced later without touching the session layer.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix"

	"fixlab.dev/fixlab/backend/internal/common"
	"fixlab.dev/fixlab/backend/internal/messages"
)

// Well-known FIX tags (kept local so the engine stays decoupled from any
// generated field packages).
const (
	tagBeginString  = quickfix.Tag(8)
	tagMsgType      = quickfix.Tag(35)
	tagSenderCompID = quickfix.Tag(49)
	tagTargetCompID = quickfix.Tag(56)
	tagMsgSeqNum    = quickfix.Tag(34)
	tagPossDupFlag  = quickfix.Tag(43)
	tagSendingTime  = quickfix.Tag(52)
)

// FIXEvent is an engine-agnostic view of a single FIX message crossing a
// session, in either direction.
type FIXEvent struct {
	Direction    messages.Direction
	Raw          string // pipe-delimited raw FIX
	MsgType      string
	MsgName      string
	MsgSeqNum    int
	SenderCompID string
	TargetCompID string
	At           time.Time
}

// Hooks receives engine callbacks. Implementations must be safe for
// concurrent use: QuickFIX/Go invokes callbacks from session goroutines.
type Hooks interface {
	// OnEngineLogon fires when the FIX session completes logon.
	OnEngineLogon()
	// OnEngineLogout fires when the FIX session logs out or drops.
	OnEngineLogout()
	// OnMessage fires for every inbound and outbound FIX message,
	// administrative and application alike.
	OnMessage(FIXEvent)
	// OnSequenceGap fires when an unexpected sequence number is observed.
	OnSequenceGap(expected, received int, dir messages.Direction)
	// OnEngineError reports non-fatal engine problems.
	OnEngineError(err error)
}

// AcceptorConfig configures one QuickFIX/Go acceptor instance.
type AcceptorConfig struct {
	BindHost string
	Port     int // loopback port; 0 = allocate an ephemeral one
	// Session identity.
	BeginString  string
	SenderCompID string
	TargetCompID string
	// DataDictionaryPath is a filesystem path to the FIX data dictionary
	// XML (QuickFIX/Go requires a real file).
	DataDictionaryPath string
	Hooks              Hooks
	// MsgName resolves 35= values to message names (may be nil).
	MsgName func(msgType string) string
	Logger  *slog.Logger
	Metrics *common.Metrics
}

// InitiatorConfig lives in initiator.go (phase 5).

// Handle is a running engine instance.
type Handle interface {
	// BoundPort returns the loopback port the acceptor bound.
	BoundPort() int
	// SendMessage transmits an application message through the FIX
	// session. QuickFIX/Go stamps the header/trailer (sequence number,
	// sending time, body length, checksum) and validates the message
	// through its send path before transmission. It returns an error
	// when no session is logged on.
	SendMessage(msg *quickfix.Message) error
	// Stop shuts the engine down.
	Stop() error
}

// Manager owns FIX engine instances.
type Manager interface {
	StartAcceptor(ctx context.Context, cfg AcceptorConfig) (Handle, error)
	// StartInitiator arrives with phase 5 (outbound connections + SSRF
	// protection). It is part of the interface now so callers can be
	// written against it.
	StartInitiator(ctx context.Context, cfg InitiatorConfig) (Handle, error)
}

type manager struct{}

// NewManager returns the QuickFIX/Go-backed engine manager.
func NewManager() Manager { return &manager{} }

// AllocateLoopbackPort binds 127.0.0.1:0 to discover a free loopback port.
// The caller must bind it promptly; the bind performed here is only a
// best-effort availability probe.
func AllocateLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("engine: allocate loopback port: %w", err)
	}
	defer ln.Close()
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		return a.Port, nil
	}
	return 0, fmt.Errorf("engine: unexpected listener address %v", ln.Addr())
}

func (m *manager) StartAcceptor(_ context.Context, cfg AcceptorConfig) (Handle, error) {
	if cfg.Hooks == nil {
		return nil, fmt.Errorf("engine: hooks are required")
	}
	if cfg.DataDictionaryPath == "" {
		return nil, fmt.Errorf("engine: data dictionary path is required")
	}
	port := cfg.Port
	if port == 0 {
		var err error
		if port, err = AllocateLoopbackPort(); err != nil {
			return nil, err
		}
	}
	bindHost := cfg.BindHost
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}

	settingsText := fmt.Sprintf(`[DEFAULT]
ConnectionType=acceptor
SocketAcceptPort=%d
SocketAcceptHost=%s
BeginString=%s
SenderCompID=%s
TargetCompID=%s
DataDictionary=%s
ValidateUserDefinedFields=N
[SESSION]
BeginString=%s
SenderCompID=%s
TargetCompID=%s
`,
		port, bindHost,
		cfg.BeginString, cfg.SenderCompID, cfg.TargetCompID,
		cfg.DataDictionaryPath,
		cfg.BeginString, cfg.SenderCompID, cfg.TargetCompID,
	)
	settings, err := quickfix.ParseSettings(strings.NewReader(settingsText))
	if err != nil {
		return nil, fmt.Errorf("engine: parse settings: %w", err)
	}

	app := &app{
		hooks:   cfg.Hooks,
		msgName: cfg.MsgName,
		logger:  cfg.Logger,
		metrics: cfg.Metrics,
	}
	acceptor, err := quickfix.NewAcceptor(
		app,
		quickfix.NewMemoryStoreFactory(), // ephemeral by design (spec §39)
		settings,
		quickfix.NewNullLogFactory(),
	)
	if err != nil {
		return nil, fmt.Errorf("engine: create acceptor: %w", err)
	}
	if err := acceptor.Start(); err != nil {
		return nil, fmt.Errorf("engine: start acceptor: %w", err)
	}
	return &acceptorHandle{acceptor: acceptor, app: app, port: port}, nil
}

type acceptorHandle struct {
	acceptor *quickfix.Acceptor
	app      *app
	port     int
	once     sync.Once
}

func (h *acceptorHandle) BoundPort() int { return h.port }

// SendMessage transmits msg through the logged-on FIX session. It fails
// fast when the counterparty is not logged on instead of silently
// queueing the message in QuickFIX/Go's send queue.
func (h *acceptorHandle) SendMessage(msg *quickfix.Message) error {
	sid, ok := h.app.sessionID()
	if !ok || !h.app.isLoggedOn() {
		return fmt.Errorf("engine: no logged-on FIX session to send to")
	}
	return quickfix.SendToTarget(msg, sid)
}

func (h *acceptorHandle) Stop() error {
	var err error
	h.once.Do(func() {
		// Acceptor.Stop has no error return; run it and report nil.
		// A panic here would indicate a quickfix bug, so let it propagate.
		h.acceptor.Stop()
	})
	return err
}

// app implements quickfix.Application, translating engine callbacks into
// Hooks. It performs no session-protocol logic of its own.
type app struct {
	hooks   Hooks
	msgName func(msgType string) string
	logger  *slog.Logger
	metrics *common.Metrics

	// firstActivity fires once on the first recorded message (initiator
	// only): the TCP dial succeeded and the session is live on the
	// socket. Nil for acceptor sessions.
	firstActivity func()

	mu             sync.Mutex
	expectInbound  int
	expectOutbound int

	sidMu    sync.RWMutex
	sid      quickfix.SessionID
	hasSID   bool
	loggedOn atomic.Bool
}

// sessionID returns the QuickFIX session identity captured at OnCreate.
func (a *app) sessionID() (quickfix.SessionID, bool) {
	a.sidMu.RLock()
	defer a.sidMu.RUnlock()
	return a.sid, a.hasSID
}

func (a *app) isLoggedOn() bool { return a.loggedOn.Load() }

func (a *app) OnCreate(sessionID quickfix.SessionID) {
	a.mu.Lock()
	a.expectInbound, a.expectOutbound = 1, 1
	a.mu.Unlock()
	a.sidMu.Lock()
	a.sid, a.hasSID = sessionID, true
	a.sidMu.Unlock()
	if a.logger != nil {
		a.logger.Info("engine: session created", "session", sessionID.String())
	}
}

func (a *app) OnLogon(sessionID quickfix.SessionID) {
	a.loggedOn.Store(true)
	if a.logger != nil {
		a.logger.Info("engine: logon accepted", "session", sessionID.String())
	}
	a.hooks.OnEngineLogon()
}

func (a *app) OnLogout(sessionID quickfix.SessionID) {
	a.loggedOn.Store(false)
	if a.logger != nil {
		a.logger.Info("engine: logout/disconnect", "session", sessionID.String())
	}
	a.hooks.OnEngineLogout()
}

func (a *app) ToAdmin(msg *quickfix.Message, _ quickfix.SessionID) {
	a.record(msg, messages.Outbound)
}

func (a *app) FromAdmin(msg *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	a.record(msg, messages.Inbound)
	return nil
}

func (a *app) ToApp(msg *quickfix.Message, _ quickfix.SessionID) error {
	a.record(msg, messages.Outbound)
	return nil
}

func (a *app) FromApp(msg *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	a.record(msg, messages.Inbound)
	return nil
}

// record converts a QuickFIX/Go message into a FIXEvent, tracks sequence
// continuity per direction, and forwards the event to the hooks.
func (a *app) record(msg *quickfix.Message, dir messages.Direction) {
	// First FIX activity on the socket: the TCP dial succeeded and the
	// session is live (initiator only; nil for acceptor sessions).
	a.mu.Lock()
	fa := a.firstActivity
	a.firstActivity = nil
	a.mu.Unlock()
	if fa != nil {
		fa()
	}
	ev := FIXEvent{
		Direction: dir,
		Raw:       strings.ReplaceAll(msg.String(), "\x01", "|"),
		At:        time.Now(),
	}
	if v, err := msg.Header.GetString(tagMsgType); err == nil {
		ev.MsgType = v
		if a.msgName != nil {
			ev.MsgName = a.msgName(v)
		}
	}
	if v, err := msg.Header.GetString(tagMsgSeqNum); err == nil {
		if n, cerr := strconv.Atoi(v); cerr == nil {
			ev.MsgSeqNum = n
		}
	}
	if v, err := msg.Header.GetString(tagSenderCompID); err == nil {
		ev.SenderCompID = v
	}
	if v, err := msg.Header.GetString(tagTargetCompID); err == nil {
		ev.TargetCompID = v
	}

	// Sequence continuity check. PossDupFlag=Y marks legitimate replays
	// (resend responses) and is exempt.
	possDup := false
	if v, err := msg.Header.GetString(tagPossDupFlag); err == nil && v == "Y" {
		possDup = true
	}
	if !possDup && ev.MsgSeqNum > 0 {
		a.mu.Lock()
		expected := a.expectInbound
		if dir == messages.Outbound {
			expected = a.expectOutbound
		}
		if ev.MsgSeqNum != expected {
			a.mu.Unlock()
			if a.metrics != nil {
				a.metrics.SequenceErrors.Add(1)
			}
			a.hooks.OnSequenceGap(expected, ev.MsgSeqNum, dir)
			a.mu.Lock()
		}
		if dir == messages.Outbound {
			a.expectOutbound = ev.MsgSeqNum + 1
		} else {
			a.expectInbound = ev.MsgSeqNum + 1
		}
		a.mu.Unlock()
	}

	if a.metrics != nil {
		if dir == messages.Inbound {
			a.metrics.FIXMessagesIn.Add(1)
		} else {
			a.metrics.FIXMessagesOut.Add(1)
		}
	}
	a.hooks.OnMessage(ev)
}
