package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"fixlab.dev/fixlab/backend/internal/common"
	"fixlab.dev/fixlab/backend/internal/dictionary"
	"fixlab.dev/fixlab/backend/internal/engine"
	"fixlab.dev/fixlab/backend/internal/messages"
	"fixlab.dev/fixlab/backend/internal/orders"
	"fixlab.dev/fixlab/backend/internal/rules"
	"fixlab.dev/fixlab/backend/internal/scenario"
	"fixlab.dev/fixlab/backend/internal/security"
	"fixlab.dev/fixlab/backend/internal/simulator"
	"fixlab.dev/fixlab/backend/internal/storage"
	"fixlab.dev/fixlab/backend/internal/websocket"
)

// CreateRequest carries the sandbox creation parameters (spec §5).
type CreateRequest struct {
	FIXVersion   string // phase 1+: only "FIX.4.4"
	Role         string // "ACCEPTOR" (phase 1) or "INITIATOR" (phase 5)
	Transport    string // "TCP" (default); "TLS" is an alias for TLS=true (phase 7)
	TLS          bool   // phase 7: terminate TLS at the edge (acceptor) / dial TLS (initiator)
	Auth         string // phase 1+: only "NONE"
	TargetCompID string // acceptor: the developer's SenderCompID; defaults to "CLIENT"
	// Initiator-only fields (phase 5, spec §8).
	RemoteHost        string // remote acceptor hostname/IP (SSRF-validated)
	RemotePort        string // remote acceptor port (string: validated numeric)
	RemoteCompID      string // remote acceptor's CompID (our TargetCompID)
	LocalSenderCompID string // our SenderCompID; defaults to "FIXLAB"
}

// ClientIPKey carries the HTTP client IP into CreateSession (via
// context) so the per-IP active-session cap (spec §44) can be enforced.
type ctxKey string

const ClientIPKey ctxKey = "fixlab.client_ip"

// ClientIPFromCtx extracts the creator IP the API layer recorded, or ""
// when the caller did not provide one (the cap is skipped then).
func ClientIPFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if ip, ok := ctx.Value(ClientIPKey).(string); ok {
		return ip
	}
	return ""
}

// SessionInfo is the JSON view of a session (spec §38).
type SessionInfo struct {
	SessionToken string    `json:"sessionToken"`
	Role         string    `json:"role"`
	Endpoint     Endpoint  `json:"endpoint"`
	Identifiers  Ident     `json:"identifiers"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"createdAt"`
	ExpiresAt    time.Time `json:"expiresAt"`
	// Live counters for the browser diagnostics view (phase 2).
	AppMessages  int64 `json:"appMessages"`
	MessageCount int   `json:"messageCount"`
	// Kill switch state for the workspace banner/toggle (phase 4).
	KillSwitch bool `json:"killSwitch"`
	// Stochastic is the phase-2.1 stochastic simulation policy (config
	// + outcomes drawn this session). Always present; the policy is
	// disabled until the browser configures it.
	Stochastic simulator.StochasticState `json:"stochastic"`
	// TLS certificate identity (phase 7): SHA-256 fingerprint of the
	// serving certificate and whether it is the dev self-signed one.
	// Present on TLS sessions only.
	CertFingerprint string `json:"certFingerprint,omitempty"`
	CertSelfSigned  bool   `json:"certSelfSigned,omitempty"`
	// RemoteIP is the pinned SSRF-validated dial IP (initiator sessions
	// only, phase 5). For initiator sessions Endpoint describes the
	// REMOTE target (host as entered, remote port); for acceptor
	// sessions it describes FixLab's listener.
	RemoteIP string `json:"remoteIp,omitempty"`
	// Replay is the phase-2.3 scenario replay status (present while a
	// replay is running or after one finished/failed).
	Replay *ReplayStatus `json:"replay,omitempty"`
	// Dictionary describes the session's active FIX dictionary
	// (phase 2.4): the standard embedded one, or an uploaded custom
	// override.
	Dictionary DictionaryInfo `json:"dictionary"`
}

// DictionaryInfo is the API view of a session's active dictionary.
type DictionaryInfo struct {
	Custom      bool   `json:"custom"`
	Name        string `json:"name,omitempty"`
	BeginString string `json:"beginString"`
	Fields      int    `json:"fields"`
	Messages    int    `json:"messages"`
}

// Endpoint describes how the developer's FIX engine connects.
type Endpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	TLS  bool   `json:"tls"`
}

// Ident carries the FIX session identity.
type Ident struct {
	BeginString  string `json:"beginString"`
	SenderCompID string `json:"senderCompId"`
	TargetCompID string `json:"targetCompId"`
}

// Manager orchestrates sandbox sessions (spec §12).
type Manager interface {
	CreateSession(ctx context.Context, req CreateRequest) (*Session, error)
	StartSession(token string) error
	StopSession(token string) error
	DestroySession(token string) error
	GetSession(token string) (*Session, error)
	GetStatus(token string) (Status, error)
	// Info renders the public session view for the API.
	Info(s *Session) SessionInfo
	// RunCleanupLoop destroys expired sessions on cfg.CleanupInterval
	// until ctx is done (spec §40).
	RunCleanupLoop(ctx context.Context)
	// Shutdown stops all sessions and releases resources.
	Shutdown()
	// ReplaySession creates a fresh ACCEPTOR session and re-enacts a
	// recorded scenario in it (phase 2.3, spec §48). The scenario comes
	// from a live session (SourceToken) or is supplied directly
	// (Scenario). It returns the new session's token and the step count.
	ReplaySession(ctx context.Context, req ReplayRequest) (newToken string, totalSteps int, err error)
}

// ReplayRequest carries a scenario replay (phase 2.3): exactly one of
// SourceToken / Scenario must be set. Speed is the timing multiplier
// (2 = twice as fast); <= 0 applies every step immediately.
type ReplayRequest struct {
	SourceToken string
	Scenario    *scenario.Scenario
	Speed       float64
}

type manager struct {
	cfg     common.Config
	log     *slog.Logger
	metrics *common.Metrics
	store   storage.Store
	ports   PortManager
	engines engine.Manager
	dict    *dictionary.Dictionary
	specDir string // temp dir holding materialized FIX dictionaries

	mu       sync.RWMutex
	sessions map[string]*Session
	hub      websocket.Hub
	// dialLimiter rate-limits outbound initiator dials per destination
	// IP (spec §9); initiatorCount caps concurrent initiator sessions
	// per server.
	dialLimiter    *security.RateLimiter
	initiatorCount atomic.Int64
	// tlsSetup is the edge TLS identity resolved at startup (phase 7).
	// A nil Config means no usable certificate: tls:true sessions are
	// refused with a 400 naming the problem.
	tlsSetup security.TLSSetup
}

// NewManager wires a session manager. It loads the FIX 4.4 dictionary
// once at startup (spec §17). hub streams live events to browsers; a nil
// hub is replaced with a NoopHub.
func NewManager(cfg common.Config, log *slog.Logger, metrics *common.Metrics, store storage.Store, hub websocket.Hub) (Manager, error) {
	if log == nil {
		log = common.NewLogger()
	}
	if metrics == nil {
		metrics = common.DefaultMetrics()
	}
	if store == nil {
		store = storage.NewMemoryStore()
	}
	if hub == nil {
		hub = websocket.NoopHub{}
	}
	dict, err := dictionary.Load("FIX44.xml")
	if err != nil {
		return nil, fmt.Errorf("session: load FIX44 dictionary: %w", err)
	}
	specDir, err := dictionary.MaterializeTemp()
	if err != nil {
		return nil, fmt.Errorf("session: materialize dictionaries: %w", err)
	}
	dialRate := cfg.InitiatorDialRatePerMin
	if dialRate <= 0 {
		dialRate = 10
	}
	// Resolve the edge TLS identity once at startup (phase 7, spec §15):
	// env-provided cert wins, otherwise a self-signed dev cert is
	// generated (logged loudly). A broken env configuration does not
	// brick the server — it is recorded and tls:true sessions are
	// refused with a 400 naming the problem.
	tlsSetup := security.SetupServerTLS(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.PublicHost, log)
	if tlsSetup.Err != nil {
		log.Error("tls: no usable serving certificate — tls:true sessions will be refused",
			"err", tlsSetup.Err)
	}
	return &manager{
		cfg:         cfg,
		log:         log,
		metrics:     metrics,
		store:       store,
		ports:       NewPortManager(cfg.PortPoolMin, cfg.PortPoolMax),
		engines:     engine.NewManager(),
		dict:        dict,
		specDir:     specDir,
		sessions:    map[string]*Session{},
		hub:         hub,
		dialLimiter: security.NewRateLimiter(dialRate, time.Minute),
		tlsSetup:    tlsSetup,
	}, nil
}

func (m *manager) validate(req CreateRequest) error {
	if req.FIXVersion != "" && req.FIXVersion != "FIX.4.4" {
		return fmt.Errorf("session: FIX version %q not supported (only FIX.4.4)", req.FIXVersion)
	}
	role := req.Role
	if role == "" {
		role = string(RoleAcceptor)
	}
	if role != string(RoleAcceptor) && role != string(RoleInitiator) {
		return fmt.Errorf("session: role %q not supported (ACCEPTOR or INITIATOR)", req.Role)
	}
	if req.Transport != "" && req.Transport != "TCP" && req.Transport != "TLS" {
		return fmt.Errorf("session: transport %q not supported (TCP or TLS)", req.Transport)
	}
	// "TLS" as a transport is an alias for the tls:true flag (spec §5).
	if req.TLS || req.Transport == "TLS" {
		if m.tlsSetup.Config == nil {
			problem := "no certificate configured"
			if m.tlsSetup.Err != nil {
				problem = m.tlsSetup.Err.Error()
			}
			return fmt.Errorf("session: TLS requested but no usable TLS certificate: %s", problem)
		}
	}
	if req.Auth != "" && req.Auth != "NONE" {
		return fmt.Errorf("session: auth mode %q not supported (only NONE)", req.Auth)
	}
	if role == string(RoleInitiator) {
		if req.RemoteCompID == "" {
			return fmt.Errorf("session: initiator sessions require remoteCompId (the remote acceptor's CompID)")
		}
	}
	return nil
}

// CreateSession registers a sandbox and starts its FIX engine. For
// ACCEPTOR sessions it allocates a port, starts the FIX acceptor on
// loopback and fronts it with the TCP guard; for INITIATOR sessions it
// SSRF-validates the remote target and starts the outbound initiator
// (no port allocation, no listener — spec §8).
func (m *manager) CreateSession(ctx context.Context, req CreateRequest) (*Session, error) {
	if err := m.validate(req); err != nil {
		return nil, err
	}
	// Anonymous per-IP active-session cap (spec §44, default 1). The API
	// layer records the HTTP client IP in the context; without it the
	// cap cannot be attributed and is skipped.
	if ip := ClientIPFromCtx(ctx); ip != "" {
		max := m.cfg.MaxSessionsPerIP
		if max <= 0 {
			max = 1
		}
		m.mu.RLock()
		n := 0
		for _, s := range m.sessions {
			if s.CreatorIP == ip {
				n++
			}
		}
		m.mu.RUnlock()
		if n >= max {
			return nil, fmt.Errorf("session: active session limit reached for this IP (limit %d)", max)
		}
	}
	role := req.Role
	if role == "" {
		role = string(RoleAcceptor)
	}
	if role == string(RoleInitiator) {
		return m.createInitiatorSession(ctx, req)
	}
	return m.createAcceptorSession(ctx, req)
}

// newGuard builds the edge guard for an acceptor session. With TLS on,
// the public port terminates TLS at the edge (phase 7); the guard's
// byte-level protections (frame cap, rate limits, timeouts) apply to
// the decrypted stream unchanged.
func (m *manager) newGuard(s *Session, port, internalPort int) (*security.Guard, error) {
	cfg := security.GuardConfig{
		PublicPort:     port,
		BackendAddr:    fmt.Sprintf("127.0.0.1:%d", internalPort),
		SessionLabel:   shortToken(s.Token),
		MaxFrameBytes:  m.cfg.MaxFIXFrameBytes,
		IdleTimeout:    m.cfg.TCPIdleTimeout,
		LogonTimeout:   m.cfg.TCPLogonTimeout,
		MinHeartBtInt:  m.cfg.MinHeartBtInt,
		ConnLimiter:    security.NewRateLimiter(m.cfg.TCPConnRatePerMin, time.Minute),
		Metrics:        m.metrics,
		Logger:         m.log,
		OnConnect:      s.onTCPConnect,
		OnDisconnect:   s.onTCPDisconnect,
		OnInboundLogon: s.onInboundLogon,
	}
	if s.TLS {
		cfg.TLSConfig = m.tlsSetup.Config
	}
	return security.NewGuard(cfg)
}

// createAcceptorSession allocates a port, starts the FIX acceptor on
// loopback, fronts it with the TCP guard, and registers the sandbox.
// With tls:true the public port terminates TLS at the edge (phase 7).
func (m *manager) createAcceptorSession(ctx context.Context, req CreateRequest) (*Session, error) {
	targetCompID := req.TargetCompID
	if targetCompID == "" {
		targetCompID = "CLIENT"
	}
	tlsOn := req.TLS || req.Transport == "TLS"

	token, err := security.GenerateSessionToken()
	if err != nil {
		return nil, err
	}

	port, err := m.ports.Acquire(token)
	if err != nil {
		return nil, err
	}
	// From here on, every failure path must release the port.
	var engineHandle engine.Handle
	var guard *security.Guard
	cleanup := func() {
		if guard != nil {
			guard.Close()
		}
		if engineHandle != nil {
			engineHandle.Stop()
		}
		m.ports.Release(port)
	}

	internalPort, err := engine.AllocateLoopbackPort()
	if err != nil {
		cleanup()
		return nil, err
	}

	now := time.Now()
	s := &Session{
		Token:           token,
		Role:            RoleAcceptor,
		BeginString:     "FIX.4.4",
		SenderCompID:    "FIXLAB",
		TargetCompID:    targetCompID,
		Port:            port,
		InternalPort:    internalPort,
		TLS:             tlsOn,
		CreatorIP:       ClientIPFromCtx(ctx),
		CreatedAt:       now,
		ExpiresAt:       now.Add(m.cfg.SessionTTL),
		baseDict:        m.dict,
		history:         messages.NewRingBuffer(m.cfg.MessageHistoryCap),
		appMessageLimit: m.cfg.AppMessageLimit,
		orderStore:      orders.NewInMemoryStore(m.cfg.OrderCap),
		recorder:        scenario.NewRecorder(now),
		logger:          m.log,
		hub:             m.hub,
	}
	s.onStatusChange = func(sess *Session, old, st Status) {
		m.log.Info("session status",
			"token", shortToken(sess.Token),
			"previous", string(old),
			"status", string(st))
		payload := map[string]any{
			"status":   string(st),
			"previous": string(old),
		}
		if st == StatusConnectionFailed {
			sess.mu.Lock()
			reason := sess.connFailReason
			sess.mu.Unlock()
			payload["reason"] = reason
		}
		m.hub.Publish(websocket.Event{
			Type:      websocket.EventConnectionStatus,
			SessionID: sess.Token,
			Timestamp: time.Now(),
			Payload:   payload,
		})
	}

	engineHandle, err = m.engines.StartAcceptor(context.Background(), engine.AcceptorConfig{
		BindHost:           m.cfg.InternalBindHost,
		Port:               internalPort,
		BeginString:        s.BeginString,
		SenderCompID:       s.SenderCompID,
		TargetCompID:       s.TargetCompID,
		DataDictionaryPath: m.specDir + "/FIX44.xml",
		Hooks:              s,
		MsgName:            m.dict.MessageName,
		Logger:             m.log,
		Metrics:            m.metrics,
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("session: start FIX engine: %w", err)
	}
	s.engineHandle = engineHandle

	guard, err = m.newGuard(s, port, internalPort)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("session: start TCP guard: %w", err)
	}
	s.guard = guard

	// Wire the trading simulator (phase 3): inbound 35=D/F/G messages
	// and browser execution actions flow through it. The phase-4 rule
	// engine is consulted by the simulator for every inbound 35=D.
	// The phase-2.3 scenario recorder captures every execution.
	s.rulesEngine = rules.NewEngine()
	sim, err := simulator.New(simulator.Config{
		Store:    s.orderStore,
		Sender:   s,
		Log:      m.log,
		Rules:    s.rulesEngine,
		Metrics:  m.metrics,
		Recorder: s.recorder,
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("session: create simulator: %w", err)
	}
	s.sim = sim

	m.mu.Lock()
	m.sessions[token] = s
	m.mu.Unlock()
	m.metrics.ActiveSessions.Add(1)
	m.persistMeta(s)

	m.log.Info("session created",
		"token", shortToken(token),
		"port", port,
		"target_comp_id", targetCompID,
		"expires_at", s.ExpiresAt.Format(time.RFC3339))
	return s, nil
}

// createInitiatorSession SSRF-validates the remote target, rate-limits
// the dial, and starts the outbound FIX initiator (phase 5, spec §8/§9).
// No port is allocated and no listener is started: there is nothing to
// connect TO on FixLab's side.
func (m *manager) createInitiatorSession(ctx context.Context, req CreateRequest) (*Session, error) {
	// SSRF validation FIRST: no socket, no session, no state before the
	// target is proven safe. Errors name the exact violation (400).
	vr, err := security.ValidateInitiatorTarget(req.RemoteHost, req.RemotePort, m.cfg.InitiatorDNSTimeout)
	if err != nil {
		return nil, err
	}
	// Per-destination-IP dial rate limit (spec §9).
	if !m.dialLimiter.Allow(vr.DialIP) {
		return nil, fmt.Errorf("session: outbound dial rate limit exceeded for %s", vr.DialIP)
	}
	// Per-server cap on concurrent initiator sessions (spec §9).
	maxOut := m.cfg.InitiatorMaxOutbound
	if maxOut <= 0 {
		maxOut = 50
	}
	if m.initiatorCount.Load() >= int64(maxOut) {
		return nil, fmt.Errorf("session: maximum outbound initiator sessions reached (%d)", maxOut)
	}

	localCompID := req.LocalSenderCompID
	if localCompID == "" {
		localCompID = "FIXLAB"
	}
	token, err := security.GenerateSessionToken()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	s := &Session{
		Token:           token,
		Role:            RoleInitiator,
		BeginString:     "FIX.4.4",
		SenderCompID:    localCompID,
		TargetCompID:    req.RemoteCompID,
		RemoteHost:      vr.Hostname,
		DialIP:          vr.DialIP,
		RemotePort:      vr.Port,
		TLS:             req.TLS || req.Transport == "TLS",
		CreatorIP:       ClientIPFromCtx(ctx),
		CreatedAt:       now,
		ExpiresAt:       now.Add(m.cfg.SessionTTL),
		baseDict:        m.dict,
		history:         messages.NewRingBuffer(m.cfg.MessageHistoryCap),
		appMessageLimit: m.cfg.AppMessageLimit,
		orderStore:      orders.NewInMemoryStore(m.cfg.OrderCap),
		recorder:        scenario.NewRecorder(now),
		logger:          m.log,
		hub:             m.hub,
		initStatus:      StatusConnecting,
	}
	s.onStatusChange = func(sess *Session, old, st Status) {
		m.log.Info("session status",
			"token", shortToken(sess.Token),
			"previous", string(old),
			"status", string(st))
		payload := map[string]any{
			"status":   string(st),
			"previous": string(old),
		}
		if st == StatusConnectionFailed {
			sess.mu.Lock()
			reason := sess.connFailReason
			sess.mu.Unlock()
			payload["reason"] = reason
		}
		m.hub.Publish(websocket.Event{
			Type:      websocket.EventConnectionStatus,
			SessionID: sess.Token,
			Timestamp: time.Now(),
			Payload:   payload,
		})
	}

	engineHandle, err := m.engines.StartInitiator(context.Background(), engine.InitiatorConfig{
		RemoteHost:         vr.Hostname,
		DialIP:             vr.DialIP,
		RemotePort:         vr.Port,
		RemoteCompID:       req.RemoteCompID,
		LocalCompID:        localCompID,
		BeginString:        s.BeginString,
		TLS:                s.TLS,
		InsecureSkipVerify: m.cfg.TLSInsecureSkipVerify,
		DataDictionaryPath: m.specDir + "/FIX44.xml",
		Hooks:              s,
		MsgName:            m.dict.MessageName,
		Logger:             m.log,
		Metrics:            m.metrics,
		ConnectTimeout:     m.cfg.InitiatorConnectTimeout,
		OnFirstActivity:    s.OnInitiatorTCPActivity,
		OnConnectFailed:    s.OnInitiatorConnectFailed,
	})
	if err != nil {
		return nil, fmt.Errorf("session: start FIX initiator: %w", err)
	}
	s.engineHandle = engineHandle

	// The simulator is wired for its order store + remote-report
	// application; rules/kill-switch never fire for OUTBOUND orders.
	s.rulesEngine = rules.NewEngine()
	sim, err := simulator.New(simulator.Config{
		Store:    s.orderStore,
		Sender:   s,
		Log:      m.log,
		Rules:    s.rulesEngine,
		Metrics:  m.metrics,
		Recorder: s.recorder,
	})
	if err != nil {
		engineHandle.Stop()
		return nil, fmt.Errorf("session: create simulator: %w", err)
	}
	s.sim = sim

	m.mu.Lock()
	m.sessions[token] = s
	m.mu.Unlock()
	m.initiatorCount.Add(1)
	m.metrics.ActiveSessions.Add(1)
	m.persistMeta(s)

	m.log.Info("initiator session created",
		"token", shortToken(token),
		"remote_host", vr.Hostname,
		"dial_ip", vr.DialIP,
		"remote_port", vr.Port,
		"expires_at", s.ExpiresAt.Format(time.RFC3339))
	return s, nil
}

// StartSession (re)starts a stopped session's engine and guard. Sessions
// are started automatically on creation; this exists for explicit
// restarts and the future start endpoint.
func (m *manager) StartSession(token string) error {
	s, err := m.GetSession(token)
	if err != nil {
		return err
	}
	s.mu.Lock()
	running := s.engineHandle != nil || s.guard != nil
	role := s.Role
	s.mu.Unlock()
	if running {
		return fmt.Errorf("session: already running")
	}
	if role == RoleInitiator {
		return m.startInitiatorEngine(s)
	}
	port, err := m.ports.Acquire(token)
	if err != nil {
		return err
	}
	internalPort, err := engine.AllocateLoopbackPort()
	if err != nil {
		m.ports.Release(port)
		return err
	}
	eh, err := m.engines.StartAcceptor(context.Background(), engine.AcceptorConfig{
		BindHost:           m.cfg.InternalBindHost,
		Port:               internalPort,
		BeginString:        s.BeginString,
		SenderCompID:       s.SenderCompID,
		TargetCompID:       s.TargetCompID,
		DataDictionaryPath: m.specDir + "/FIX44.xml",
		Hooks:              s,
		MsgName:            m.dict.MessageName,
		Logger:             m.log,
		Metrics:            m.metrics,
	})
	if err != nil {
		m.ports.Release(port)
		return err
	}
	g, err := m.newGuard(s, port, internalPort)
	if err != nil {
		eh.Stop()
		m.ports.Release(port)
		return err
	}
	s.mu.Lock()
	s.Port, s.InternalPort = port, internalPort
	s.engineHandle, s.guard = eh, g
	s.mu.Unlock()
	m.persistMeta(s)
	return nil
}

// startInitiatorEngine (re)starts the outbound initiator for a stopped
// initiator session. The SSRF validation runs AGAIN here with a fresh
// DNS resolution — every (re)connect re-validates the target (spec §9),
// so a hostname that turned malicious after creation cannot be dialed.
func (m *manager) startInitiatorEngine(s *Session) error {
	vr, err := security.ValidateInitiatorTarget(s.RemoteHost, strconv.Itoa(s.RemotePort), m.cfg.InitiatorDNSTimeout)
	if err != nil {
		return fmt.Errorf("session: restart SSRF validation failed: %w", err)
	}
	if !m.dialLimiter.Allow(vr.DialIP) {
		return fmt.Errorf("session: outbound dial rate limit exceeded for %s", vr.DialIP)
	}
	eh, err := m.engines.StartInitiator(context.Background(), engine.InitiatorConfig{
		RemoteHost:         vr.Hostname,
		DialIP:             vr.DialIP,
		RemotePort:         vr.Port,
		RemoteCompID:       s.TargetCompID,
		LocalCompID:        s.SenderCompID,
		BeginString:        s.BeginString,
		TLS:                s.TLS,
		InsecureSkipVerify: m.cfg.TLSInsecureSkipVerify,
		DataDictionaryPath: m.specDir + "/FIX44.xml",
		Hooks:              s,
		MsgName:            m.dict.MessageName,
		Logger:             m.log,
		Metrics:            m.metrics,
		ConnectTimeout:     m.cfg.InitiatorConnectTimeout,
		OnFirstActivity:    s.OnInitiatorTCPActivity,
		OnConnectFailed:    s.OnInitiatorConnectFailed,
	})
	if err != nil {
		return fmt.Errorf("session: start FIX initiator: %w", err)
	}
	s.mu.Lock()
	s.DialIP = vr.DialIP
	s.engineHandle = eh
	s.initStatus = StatusConnecting
	s.connFailReason = ""
	s.mu.Unlock()
	m.persistMeta(s)
	return nil
}

// ReplaySession creates a fresh ACCEPTOR session and re-enacts a
// recorded scenario in it (phase 2.3, spec §48: "replay previous
// session"). The scenario is taken from a live session (SourceToken) or
// supplied directly (Scenario); exactly one must be set. Speed is the
// timing multiplier (2 = twice as fast); <= 0 applies every step
// immediately.
//
// The new session is a real sandbox (port, guard, engine, TTL): the
// replay is synthetic — inbound messages are re-injected through the
// simulator with the FIX engine bypassed, executions re-applied through
// the normal execution path but never transmitted, and no quota is
// consumed. Rules, stochastic and the kill switch never fire during
// replay: replay replays actions, not automation config.
//
// Only ACCEPTOR-recorded scenarios can be replayed: initiator sessions
// are driven by a remote counterparty that cannot be re-enacted.
func (m *manager) ReplaySession(ctx context.Context, req ReplayRequest) (string, int, error) {
	var sc *scenario.Scenario
	targetCompID := "CLIENT"
	if req.SourceToken != "" && req.Scenario != nil {
		return "", 0, fmt.Errorf("session: replay takes either sourceToken or scenario, not both")
	}
	switch {
	case req.SourceToken != "":
		src, err := m.GetSession(req.SourceToken)
		if err != nil {
			return "", 0, fmt.Errorf("session: replay source not found")
		}
		if src.Role != RoleAcceptor {
			return "", 0, fmt.Errorf("session: replay of %s-recorded scenarios is not supported (ACCEPTOR sessions only)", src.Role)
		}
		snap := src.ScenarioSnapshot()
		sc = &snap
		targetCompID = src.TargetCompID
	case req.Scenario != nil:
		sc = req.Scenario
		if sc.TargetCompID != "" {
			targetCompID = sc.TargetCompID
		}
	default:
		return "", 0, fmt.Errorf("session: replay needs a sourceToken or a scenario")
	}
	if req.Speed < 0 {
		return "", 0, fmt.Errorf("session: replay speed must be >= 0 (0 = apply immediately), got %v", req.Speed)
	}
	if err := scenario.Validate(sc); err != nil {
		return "", 0, err
	}

	// The replay target is a real session: port, guard, engine, TTL and
	// the per-IP cap all apply. Its FIX engine will simply never see a
	// client — the player re-enacts the story synthetically.
	//
	// The target needs a CompID pair distinct from the source's:
	// QuickFIX/Go keeps a process-wide session registry, so two live
	// sessions cannot share an identity (the phase-1 limitation). The
	// recorded TargetCompID stays in the scenario metadata; the replay
	// target gets "<source>-replay-<rand>".
	replayTargetCompID := targetCompID + "-replay-" + shortRandHex(4)
	news, err := m.CreateSession(ctx, CreateRequest{
		Role:         string(RoleAcceptor),
		TargetCompID: replayTargetCompID,
	})
	if err != nil {
		return "", 0, fmt.Errorf("session: replay target creation failed: %w", err)
	}
	// Speed is resolved by the caller (the API defaults an absent speed
	// to 1 = real time); <= 0 applies every step immediately.
	go news.ReplayScenario(sc, req.Speed)
	m.log.Info("session: replay started",
		"source", shortToken(req.SourceToken),
		"target", shortToken(news.Token),
		"steps", len(sc.Steps), "speed", req.Speed)
	return news.Token, len(sc.Steps), nil
}

// StopSession halts the FIX engine and guard but keeps the session
// registered; StartSession can bring it back (possibly on a new port).
func (m *manager) StopSession(token string) error {
	s, err := m.GetSession(token)
	if err != nil {
		return err
	}
	s.mu.Lock()
	eh, g := s.engineHandle, s.guard
	s.engineHandle, s.guard = nil, nil
	port := s.Port
	s.tcpUp, s.loggedOn, s.logonSeen = false, false, false
	if s.Role == RoleInitiator {
		s.initStatus = StatusConnecting
	}
	s.mu.Unlock()
	if g != nil {
		g.Close()
	}
	if eh != nil {
		eh.Stop()
	}
	if s.Role == RoleAcceptor {
		m.ports.Release(port)
	}
	m.persistMeta(s)
	return nil
}

// DestroySession tears a session down completely: stop the FIX session,
// close the listener, release the port, clear state, remove the session
// (spec §40).
func (m *manager) DestroySession(token string) error {
	m.mu.Lock()
	s, ok := m.sessions[token]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session: not found")
	}
	delete(m.sessions, token)
	m.mu.Unlock()

	s.mu.Lock()
	s.destroyed = true
	eh, g := s.engineHandle, s.guard
	s.engineHandle, s.guard = nil, nil
	port := s.Port
	s.mu.Unlock()

	if g != nil {
		g.Close()
	}
	if eh != nil {
		eh.Stop()
	}
	if s.Role == RoleAcceptor {
		m.ports.Release(port)
	} else {
		m.initiatorCount.Add(-1)
	}
	s.history.Clear()
	s.orderStore.Clear()
	_ = m.store.Delete(token)
	m.metrics.ActiveSessions.Add(-1)

	// Notify browsers before dropping their subscriptions: the event is
	// published first, then Close terminates the WebSocket streams (the
	// hub closes subscriber channels only after removing the session).
	reason := "destroyed"
	if s.expired {
		reason = "ttl_expired"
	}
	m.hub.Publish(websocket.Event{
		Type:      websocket.EventSessionExpired,
		SessionID: token,
		Timestamp: time.Now(),
		Payload:   map[string]string{"reason": reason},
	})
	m.hub.Close(token)

	m.log.Info("session destroyed", "token", shortToken(token), "port", port, "reason", reason)
	return nil
}

// GetSession returns the live session for a token, or an error if the
// token is unknown. The token is the sole credential (spec §6).
func (m *manager) GetSession(token string) (*Session, error) {
	m.mu.RLock()
	s, ok := m.sessions[token]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session: not found")
	}
	return s, nil
}

// GetStatus returns the derived lifecycle status for a token.
func (m *manager) GetStatus(token string) (Status, error) {
	s, err := m.GetSession(token)
	if err != nil {
		return "", err
	}
	return s.GetStatus(), nil
}

// Info renders the public session view (spec §38). The internal token is
// the only identifier exposed; no database IDs exist.
func (m *manager) Info(s *Session) SessionInfo {
	info := SessionInfo{
		SessionToken: s.Token,
		Role:         string(s.Role),
		Endpoint: Endpoint{
			Host: m.cfg.PublicHost,
			Port: s.Port,
			TLS:  s.TLS,
		},
		Identifiers: Ident{
			BeginString:  s.BeginString,
			SenderCompID: s.SenderCompID,
			TargetCompID: s.TargetCompID,
		},
		Status:       string(s.GetStatus()),
		CreatedAt:    s.CreatedAt,
		ExpiresAt:    s.ExpiresAt,
		AppMessages:  s.AppMsgCount.Load(),
		MessageCount: s.MessageCount(),
		KillSwitch:   s.rulesEngine != nil && s.rulesEngine.KillSwitch(),
		Stochastic:   s.sim.Stochastic().State(),
	}
	if rs := s.ReplayState(); rs.InProgress || rs.Failed || rs.DoneSteps > 0 {
		rsCopy := rs
		info.Replay = &rsCopy
	}
	if s.TLS {
		info.CertFingerprint = m.tlsSetup.Fingerprint
		info.CertSelfSigned = m.tlsSetup.SelfSigned
	}
	info.Dictionary = s.DictionaryInfo()
	if s.Role == RoleInitiator {
		// For initiator sessions the endpoint IS the remote target
		// (host as entered, remote port); RemoteIP is the pinned
		// SSRF-validated dial address.
		info.Endpoint = Endpoint{
			Host: s.RemoteHost,
			Port: s.RemotePort,
			TLS:  s.TLS,
		}
		info.RemoteIP = s.DialIP
	}
	return info
}

// RunCleanupLoop destroys expired sessions on the configured interval
// (spec §40).
func (m *manager) RunCleanupLoop(ctx context.Context) {
	t := time.NewTicker(m.cfg.CleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reapExpired()
		}
	}
}

func (m *manager) reapExpired() {
	now := time.Now()
	var expired []*Session
	m.mu.RLock()
	for _, s := range m.sessions {
		if now.After(s.ExpiresAt) {
			expired = append(expired, s)
		}
	}
	m.mu.RUnlock()
	for _, s := range expired {
		s.mu.Lock()
		s.expired = true
		s.mu.Unlock()
		m.log.Info("session expired", "token", shortToken(s.Token))
		m.metrics.SessionExpirations.Add(1)
		// DestroySession publishes SESSION_EXPIRED to browsers first.
		if err := m.DestroySession(s.Token); err != nil {
			m.log.Error("cleanup: destroy expired session failed",
				"token", shortToken(s.Token), "err", err)
		}
	}
}

// Shutdown stops every session and releases shared resources.
func (m *manager) Shutdown() {
	m.mu.RLock()
	tokens := make([]string, 0, len(m.sessions))
	for t := range m.sessions {
		tokens = append(tokens, t)
	}
	m.mu.RUnlock()
	for _, t := range tokens {
		_ = m.DestroySession(t)
	}
	// The materialized dictionaries are only needed while engines run.
	_ = os.RemoveAll(m.specDir)
}

func (m *manager) persistMeta(s *Session) {
	_ = m.store.Put(&storage.SessionMeta{
		Token:        s.Token,
		Role:         string(s.Role),
		BeginString:  s.BeginString,
		SenderCompID: s.SenderCompID,
		TargetCompID: s.TargetCompID,
		Port:         s.Port,
		TLS:          s.TLS,
		Status:       string(s.GetStatus()),
		CreatedAt:    s.CreatedAt,
		ExpiresAt:    s.ExpiresAt,
	})
}

// shortRandHex returns n bytes of crypto/rand as hex (best-effort:
// on the impossible CSPRNG failure it falls back to a timestamp).
func shortRandHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// shortToken renders a token prefix for logs so full tokens never land
// in log files.
func shortToken(token string) string {
	if len(token) > 14 {
		return token[:14] + "…"
	}
	return token
}
