package session

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix"

	"fixlab.dev/fixlab/backend/internal/dictionary"
	"fixlab.dev/fixlab/backend/internal/engine"
	"fixlab.dev/fixlab/backend/internal/messages"
	"fixlab.dev/fixlab/backend/internal/orders"
	"fixlab.dev/fixlab/backend/internal/rules"
	"fixlab.dev/fixlab/backend/internal/scenario"
	"fixlab.dev/fixlab/backend/internal/security"
	"fixlab.dev/fixlab/backend/internal/simulator"
	"fixlab.dev/fixlab/backend/internal/websocket"
)

// Status is the lifecycle state of a sandbox session.
//
// Acceptor sessions move through:
// WAITING_FOR_CONNECTION → TCP_CONNECTED → LOGON_RECEIVED → CONNECTED.
// Initiator sessions (phase 5) move through their own chain:
// CONNECTING → TCP_CONNECTED → LOGON_SENT → LOGON_ACCEPTED,
// with CONNECTION_FAILED as the terminal failure state.
type Status string

const (
	StatusCreated       Status = "CREATED"
	StatusWaiting       Status = "WAITING_FOR_CONNECTION"
	StatusTCPConnected  Status = "TCP_CONNECTED"
	StatusLogonReceived Status = "LOGON_RECEIVED"
	StatusConnected     Status = "CONNECTED"
	StatusDisconnected  Status = "DISCONNECTED"
	StatusExpired       Status = "EXPIRED"
	StatusDestroyed     Status = "DESTROYED"
	// Initiator-only states (phase 5, spec §8).
	StatusConnecting       Status = "CONNECTING"
	StatusLogonSent        Status = "LOGON_SENT"
	StatusLogonAccepted    Status = "LOGON_ACCEPTED"
	StatusConnectionFailed Status = "CONNECTION_FAILED"
)

// Connected reports whether the status represents a live FIX session
// (either role).
func (s Status) Connected() bool {
	return s == StatusConnected || s == StatusLogonAccepted
}

// Role is the FixLab side of the FIX session.
type Role string

const (
	// RoleAcceptor means FixLab listens; the developer connects (phase 1).
	RoleAcceptor Role = "ACCEPTOR"
	// RoleInitiator means FixLab dials out (phase 5).
	RoleInitiator Role = "INITIATOR"
)

// Session is the live sandbox: identity, engine, guard, quotas, and
// bounded state (spec §39). It implements engine.Hooks.
type Session struct {
	Token        string
	Role         Role
	BeginString  string
	SenderCompID string
	TargetCompID string
	Port         int // public FIX port (guard listener)
	InternalPort int // loopback port (QuickFIX/Go acceptor)
	TLS          bool
	// CreatorIP is the HTTP client IP that created the session, for the
	// per-IP active-session cap (spec §44). Empty when unattributed.
	CreatorIP string
	CreatedAt time.Time
	ExpiresAt time.Time

	// baseDict is the standard embedded dictionary for the session's
	// BeginString. customDict, when set, overrides it for message
	// inspection/decoding (phase 2.4: custom FIX dictionaries). It is
	// an atomic pointer so an upload never races in-flight message
	// parsing; it is ephemeral and dies with the session.
	baseDict   *dictionary.Dictionary
	customDict atomic.Pointer[customDictionary]
	history    *messages.RingBuffer
	logger     *slog.Logger // structured logger; defaults to slog.Default()

	// hub streams live events to browsers (phase 2). It may be a
	// websocket.NoopHub; never nil after the manager sets it.
	hub websocket.Hub

	mu        sync.Mutex
	tcpUp     bool
	logonSeen bool
	loggedOn  bool
	expired   bool
	destroyed bool

	// Initiator-only fields (phase 5). RemoteHost is the user-supplied
	// hostname (display only); DialIP is the pinned SSRF-validated IP
	// the engine actually dials; RemotePort is the remote acceptor's
	// port. initStatus is the initiator state machine; connFailReason
	// carries the CONNECTION_FAILED reason for the WS payload.
	RemoteHost     string
	DialIP         string
	RemotePort     int
	initStatus     Status
	connFailReason string

	// AppMsgCount counts application messages only; session-level admin
	// messages (A,0,1,2,4,5) never consume the quota (spec §4).
	AppMsgCount atomic.Int64

	// appMessageLimit is the session's application-message quota (spec
	// §44). Phase 3 enforces it on outbound executions.
	appMessageLimit int64

	// orderStore is the per-session blotter (spec §23); sim is the
	// trading simulator driving it (phase 3).
	orderStore orders.Store
	sim        simulator.Simulator

	// rulesEngine is the per-session deterministic rule engine plus the
	// kill switch (phase 4). It is consulted by the simulator for every
	// inbound NewOrderSingle.
	rulesEngine rules.Engine

	// recorder is the session's automatic scenario log (phase 2.3,
	// spec §48): logon, inbound application messages, executions,
	// logout/disconnect. Always on; bounded at scenario.MaxSteps.
	recorder *scenario.Recorder

	// replayMode marks the session as re-enacting a recorded scenario
	// (phase 2.3): outbound execution sends are synthetic (no wire
	// transmission, no quota consumption). Atomic because
	// sendExecutionLocked runs outside s.mu.
	replayMode atomic.Bool

	// replay tracks a running scenario replay for the API/WS (phase 2.3).
	replayMu sync.Mutex
	replay   ReplayStatus

	// execMu serializes quota-checked outbound execution sends so the
	// check-then-send is atomic.
	execMu sync.Mutex
	// execSeq mints OrderID (37) / ExecID (17) values; it lives on the
	// session so IDs stay unique across engine restarts.
	execSeq atomic.Int64

	engineHandle engine.Handle
	guard        *security.Guard

	onStatusChange func(sess *Session, old, new Status)
}

// customDictionary is one uploaded per-session dictionary override
// (phase 2.4): the parsed dictionary plus the display name the
// developer gave it.
type customDictionary struct {
	name string
	dict *dictionary.Dictionary
}

// Dictionary returns the session's active dictionary: the uploaded
// custom override when one is set, otherwise the standard embedded
// dictionary for the session's BeginString. Safe for concurrent use.
func (s *Session) Dictionary() *dictionary.Dictionary {
	if c := s.customDict.Load(); c != nil {
		return c.dict
	}
	return s.baseDict
}

// SetCustomDictionary installs a per-session dictionary override
// (phase 2.4). The swap is atomic: in-flight message parsing keeps the
// old dictionary, new messages see the override.
func (s *Session) SetCustomDictionary(name string, d *dictionary.Dictionary) {
	s.customDict.Store(&customDictionary{name: name, dict: d})
	s.loggerOrDefault().Info("session: custom dictionary installed",
		"token", shortToken(s.Token), "name", name,
		"fields", len(d.Fields), "messages", len(d.MsgTypeToName))
}

// ClearCustomDictionary reverts to the standard dictionary.
func (s *Session) ClearCustomDictionary() {
	s.customDict.Store(nil)
	s.loggerOrDefault().Info("session: custom dictionary cleared",
		"token", shortToken(s.Token))
}

// CustomDictionaryInfo reports the override state for the API.
func (s *Session) CustomDictionaryInfo() (name string, d *dictionary.Dictionary, ok bool) {
	if c := s.customDict.Load(); c != nil {
		return c.name, c.dict, true
	}
	return "", nil, false
}

// DictionaryInfo renders the active-dictionary view for the API
// (phase 2.4): custom override details when one is installed,
// otherwise the standard embedded dictionary's stats.
func (s *Session) DictionaryInfo() DictionaryInfo {
	d := s.Dictionary()
	info := DictionaryInfo{
		BeginString: d.BeginString,
		Fields:      len(d.Fields),
		Messages:    len(d.MsgTypeToName),
	}
	if name, _, ok := s.CustomDictionaryInfo(); ok {
		info.Custom = true
		info.Name = name
	}
	return info
}

// Rules returns the session's deterministic rule engine (nil only
// before the manager wires it).
func (s *Session) Rules() rules.Engine { return s.rulesEngine }

// SetKillSwitch toggles the venue halt (spec §31) and publishes a
// KILL_SWITCH event so browsers reflect it live. While on, every
// inbound NewOrderSingle is rejected immediately and no rule fires.
func (s *Session) SetKillSwitch(on bool) bool {
	re := s.rulesEngine
	if re == nil {
		return false
	}
	re.SetKillSwitch(on)
	s.hub.Publish(websocket.Event{
		Type:      websocket.EventKillSwitch,
		SessionID: s.Token,
		Timestamp: time.Now(),
		Payload:   map[string]bool{"enabled": on},
	})
	s.loggerOrDefault().Info("session: kill switch",
		"token", shortToken(s.Token), "enabled", on)
	return on
}

func (s *Session) loggerOrDefault() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// ReplayStatus is the API-visible state of a scenario replay
// (phase 2.3).
type ReplayStatus struct {
	InProgress bool   `json:"inProgress"`
	TotalSteps int    `json:"totalSteps"`
	DoneSteps  int    `json:"doneSteps"`
	Failed     bool   `json:"failed"`
	Error      string `json:"error,omitempty"`
}

// ReplayState returns a copy of the current replay status.
func (s *Session) ReplayState() ReplayStatus {
	s.replayMu.Lock()
	defer s.replayMu.Unlock()
	return s.replay
}

// recordStep appends one step to the session's scenario log (phase 2.3).
// It is a no-op when the session has no recorder (never in practice —
// the manager always wires one).
func (s *Session) recordStep(kind string, payload any) {
	if s.recorder == nil {
		return
	}
	s.recorder.Record(kind, payload)
}

// ScenarioSnapshot returns the exportable scenario of this session
// (phase 2.3). It never contains the session token or any secret.
func (s *Session) ScenarioSnapshot() scenario.Scenario {
	snap := s.recorder.Snapshot()
	snap.Role = string(s.Role)
	snap.BeginString = s.BeginString
	snap.TargetCompID = s.TargetCompID
	return snap
}

// ReplayScenario re-enacts sc in this session (phase 2.3). It runs
// synchronously; the manager starts it in a goroutine. Steps are applied
// at their recorded relative offsets divided by speed (speed <= 0
// applies everything immediately).
//
//   - Logon/logout/disconnect steps are narrative only and are skipped:
//     there is no live client to log on.
//   - Inbound D/F/G steps are re-injected SYNTHETICALLY through the
//     simulator (marked replayed, FIX engine bypassed).
//   - Execution steps are re-applied through the normal execution path,
//     so blotter bookkeeping and WS events are identical to the live
//     path — but nothing is transmitted and no quota is consumed.
//
// Rules, stochastic and the kill switch never fire during replay:
// replay replays actions, not automation config.
func (s *Session) ReplayScenario(sc *scenario.Scenario, speed float64) {
	if s.sim == nil {
		s.replayMu.Lock()
		s.replay = ReplayStatus{Failed: true, Error: "trading simulator not available"}
		s.replayMu.Unlock()
		return
	}
	s.replayMu.Lock()
	s.replay = ReplayStatus{InProgress: true, TotalSteps: len(sc.Steps)}
	s.replayMu.Unlock()

	s.replayMode.Store(true)
	s.sim.SetReplayMode(true)
	s.hub.Publish(websocket.Event{
		Type:      websocket.EventScenarioReplayStarted,
		SessionID: s.Token,
		Timestamp: time.Now(),
		Payload:   map[string]any{"speed": speed, "totalSteps": len(sc.Steps)},
	})
	s.loggerOrDefault().Info("scenario: replay started",
		"token", shortToken(s.Token), "steps", len(sc.Steps), "speed", speed)

	start := time.Now()
	prevAtMs := int64(0)
	done := 0
	failErr := ""
	for _, st := range sc.Steps {
		if s.isGone() {
			failErr = "session destroyed or expired during replay"
			break
		}
		if d := scenario.StepDelay(prevAtMs, st.AtMs, speed); d > 0 {
			time.Sleep(d)
		}
		prevAtMs = st.AtMs
		if err := s.applyReplayStep(st); err != nil {
			failErr = fmt.Sprintf("step %d (%s): %v", st.Seq, st.Kind, err)
			break
		}
		done++
		s.replayMu.Lock()
		s.replay.DoneSteps = done
		s.replayMu.Unlock()
	}

	s.replayMode.Store(false)
	s.sim.SetReplayMode(false)
	s.replayMu.Lock()
	s.replay.InProgress = false
	if failErr != "" {
		s.replay.Failed = true
		s.replay.Error = failErr
	}
	s.replayMu.Unlock()
	s.hub.Publish(websocket.Event{
		Type:      websocket.EventScenarioReplayFinished,
		SessionID: s.Token,
		Timestamp: time.Now(),
		Payload: map[string]any{
			"appliedSteps": done,
			"totalSteps":   len(sc.Steps),
			"failed":       failErr != "",
			"error":        failErr,
			"durationMs":   time.Since(start).Milliseconds(),
		},
	})
	s.loggerOrDefault().Info("scenario: replay finished",
		"token", shortToken(s.Token), "applied", done, "failed", failErr != "")
}

// isGone reports whether the session was destroyed or expired.
func (s *Session) isGone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.destroyed || s.expired
}

// applyReplayStep applies one validated scenario step.
func (s *Session) applyReplayStep(st scenario.Step) error {
	switch st.Kind {
	case scenario.KindLogon, scenario.KindLogout, scenario.KindDisconnect:
		// Narrative only: no live client to log on or off.
		return nil
	case scenario.KindInbound:
		p, err := scenario.DecodePayload[scenario.InboundPayload](st.Payload)
		if err != nil {
			return err
		}
		s.publishReplayedInbound(p)
		if p.MsgType == "D" {
			_, err := s.sim.ReplayOrder(p.Fields)
			return err
		}
		return s.sim.ReplayInbound(p.MsgType, p.Fields)
	case scenario.KindExecution:
		p, err := scenario.DecodePayload[scenario.ExecutionPayload](st.Payload)
		if err != nil {
			return err
		}
		_, err = s.sim.ReplayExecution(p.Action, p)
		return err
	default:
		return fmt.Errorf("scenario: unknown step kind %q", st.Kind)
	}
}

// publishReplayedInbound synthesizes the FIX_MSG_IN event for a recorded
// inbound step (phase 2.3). The record is marked replayed and its raw
// shows the recorded field values — it is NOT a re-validated wire image.
func (s *Session) publishReplayedInbound(p scenario.InboundPayload) {
	raw := replayedRaw(p.Fields)
	rec := &messages.Record{
		Type:         "FIX_MESSAGE",
		Direction:    messages.Inbound,
		MsgType:      p.MsgType,
		MsgName:      s.Dictionary().MessageName(p.MsgType),
		RawFIX:       raw,
		SenderCompID: s.TargetCompID,
		TargetCompID: s.SenderCompID,
		Timestamp:    time.Now(),
		Fields:       parseFields(raw, s.Dictionary()),
		Replayed:     true,
	}
	s.history.Push(rec)
	s.hub.Publish(websocket.Event{
		Type:      websocket.EventFIXMsgIn,
		SessionID: s.Token,
		Timestamp: rec.Timestamp,
		Payload:   rec,
	})
}

// replayedRaw renders recorded fields as pipe-delimited FIX: 8= and 35=
// first (when recorded), then the remaining tags in ascending order.
// The values are the recorded ones — including the original 9=/10=
// framing — so this is an illustrative image, not wire bytes.
func replayedRaw(fields map[int]string) string {
	var b strings.Builder
	write := func(tag int) {
		if v, ok := fields[tag]; ok {
			fmt.Fprintf(&b, "%d=%s|", tag, v)
		}
	}
	for _, tag := range []int{8, 35} {
		write(tag)
	}
	tags := make([]int, 0, len(fields))
	for tag := range fields {
		if tag == 8 || tag == 35 {
			continue
		}
		tags = append(tags, tag)
	}
	sort.Ints(tags)
	for _, tag := range tags {
		write(tag)
	}
	return b.String()
}

// Orders returns the session's blotter store.
func (s *Session) Orders() orders.Store { return s.orderStore }

// Simulator returns the session's trading simulator (nil only before
// the manager wires it).
func (s *Session) Simulator() simulator.Simulator { return s.sim }

// GetStatus derives the current lifecycle state.
func (s *Session) GetStatus() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *Session) statusLocked() Status {
	switch {
	case s.destroyed:
		return StatusDestroyed
	case s.expired:
		return StatusExpired
	case s.Role == RoleInitiator:
		return s.initStatus
	case s.loggedOn:
		return StatusConnected
	case s.logonSeen:
		return StatusLogonReceived
	case s.tcpUp:
		return StatusTCPConnected
	default:
		return StatusWaiting
	}
}

// setInitiatorStatus moves the initiator state machine (phase 5) and
// publishes the CONNECTION_STATUS event via onStatusChange. reason is
// attached to the event payload for CONNECTION_FAILED.
func (s *Session) setInitiatorStatus(st Status, reason string) {
	s.mu.Lock()
	old := s.initStatus
	if old == st && st != StatusConnectionFailed {
		s.mu.Unlock()
		return
	}
	s.initStatus = st
	if reason != "" {
		s.connFailReason = reason
	}
	cb := s.onStatusChange
	s.mu.Unlock()
	if cb != nil && old != st {
		cb(s, old, st)
	}
}

// initiatorStatus returns the current initiator state (for tests).
func (s *Session) initiatorStatus() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initStatus
}

func (s *Session) setStatusFields(tcpUp, logonSeen, loggedOn *bool) {
	s.mu.Lock()
	old := s.statusLocked()
	if tcpUp != nil {
		s.tcpUp = *tcpUp
	}
	if logonSeen != nil {
		s.logonSeen = *logonSeen
	}
	if loggedOn != nil {
		s.loggedOn = *loggedOn
	}
	st := s.statusLocked()
	cb := s.onStatusChange
	s.mu.Unlock()
	if cb != nil && old != st {
		cb(s, old, st)
	}
}

// --- engine.Hooks -----------------------------------------------------

func (s *Session) OnEngineLogon() {
	if s.Role == RoleInitiator {
		// LOGON_ACCEPTED is the initiator's CONNECTED (spec §8).
		s.setInitiatorStatus(StatusLogonAccepted, "")
		s.recordStep(scenario.KindLogon, map[string]any{})
		return
	}
	t, l := true, true
	s.setStatusFields(&t, &l, &l)
	s.recordStep(scenario.KindLogon, map[string]any{})
}

func (s *Session) OnEngineLogout() {
	if s.Role == RoleInitiator {
		// The initiator redials on drop (QuickFIX/Go ReconnectInterval),
		// so logout means "back to dialing" — unless the connect
		// watchdog already failed the session terminally.
		s.mu.Lock()
		failed := s.initStatus == StatusConnectionFailed
		s.mu.Unlock()
		if !failed {
			s.setInitiatorStatus(StatusConnecting, "")
		}
		s.recordStep(scenario.KindLogout, map[string]any{})
		return
	}
	f := false
	s.setStatusFields(&f, &f, &f)
	s.recordStep(scenario.KindLogout, map[string]any{})
}

// OnInitiatorTCPActivity fires on the first FIX message in either
// direction: the TCP dial succeeded and the session is live on the
// socket (engine callback, phase 5).
func (s *Session) OnInitiatorTCPActivity() {
	if s.Role != RoleInitiator {
		return
	}
	s.mu.Lock()
	cur := s.initStatus
	s.mu.Unlock()
	if cur == StatusConnecting {
		s.setInitiatorStatus(StatusTCPConnected, "")
	}
}

// OnInitiatorConnectFailed moves the session to CONNECTION_FAILED with
// the reason (engine watchdog callback, phase 5). The engine has
// already stopped dialing.
func (s *Session) OnInitiatorConnectFailed(reason string) {
	if s.Role != RoleInitiator {
		return
	}
	s.setInitiatorStatus(StatusConnectionFailed, reason)
	s.recordStep(scenario.KindDisconnect, map[string]any{"reason": reason})
	s.hub.Publish(websocket.Event{
		Type:      websocket.EventSessionError,
		SessionID: s.Token,
		Timestamp: time.Now(),
		Payload:   map[string]string{"error": "initiator connection failed: " + reason},
	})
}

func (s *Session) OnEngineError(err error) {
	if err == nil {
		return
	}
	s.hub.Publish(websocket.Event{
		Type:      websocket.EventSessionError,
		SessionID: s.Token,
		Timestamp: time.Now(),
		Payload:   map[string]string{"error": err.Error()},
	})
}

func (s *Session) OnSequenceGap(expected, received int, dir messages.Direction) {
	// QuickFIX/Go already performs the resend dance itself; FixLab
	// surfaces the gap to the browser for diagnostics (spec §35).
	s.hub.Publish(websocket.Event{
		Type:      websocket.EventSequenceGap,
		SessionID: s.Token,
		Timestamp: time.Now(),
		Payload: map[string]any{
			"expected":  expected,
			"received":  received,
			"direction": string(dir),
		},
	})
}

// OnMessage records every FIX message into the bounded history and
// counts application messages toward the quota. Admin messages are
// recorded but never counted (spec §4).
func (s *Session) OnMessage(ev engine.FIXEvent) {
	rec := &messages.Record{
		Type:         "FIX_MESSAGE",
		Direction:    ev.Direction,
		MsgSeqNum:    ev.MsgSeqNum,
		MsgType:      ev.MsgType,
		MsgName:      ev.MsgName,
		RawFIX:       ev.Raw,
		SenderCompID: ev.SenderCompID,
		TargetCompID: ev.TargetCompID,
		Timestamp:    ev.At,
		Fields:       parseFields(ev.Raw, s.Dictionary()),
	}
	s.history.Push(rec)
	if !messages.IsAdminMsgType(ev.MsgType) {
		s.AppMsgCount.Add(1)
	}
	// Live-stream every message to subscribed browsers (spec §19). The
	// payload is the spec §18 message representation.
	evType := websocket.EventFIXMsgOut
	if ev.Direction == messages.Inbound {
		evType = websocket.EventFIXMsgIn
	}
	s.hub.Publish(websocket.Event{
		Type:      evType,
		SessionID: s.Token,
		Timestamp: ev.At,
		Payload:   rec,
	})
	if LogFixMessages {
		l := s.logger
		if l == nil {
			l = slog.Default()
		}
		l.Info("fix message",
			"token", shortToken(s.Token),
			"dir", string(ev.Direction),
			"msg_type", ev.MsgType,
			"msg_name", ev.MsgName,
			"seq", ev.MsgSeqNum,
			"raw", ev.Raw)
	}

	// Record inbound application messages in the scenario log
	// (phase 2.3, spec §48): the trading story, not the wire chatter.
	// Admin messages (logon/heartbeat/…) are deliberately excluded.
	if s.Role == RoleAcceptor && ev.Direction == messages.Inbound &&
		(ev.MsgType == "D" || ev.MsgType == "F" || ev.MsgType == "G") {
		fields := make(map[int]string, len(rec.Fields))
		for _, f := range rec.Fields {
			fields[f.Tag] = f.Value
		}
		s.recordStep(scenario.KindInbound, scenario.InboundPayload{MsgType: ev.MsgType, Fields: fields})
	}

	// Route inbound application messages to the trading simulator
	// (phase 3: NewOrderSingle, OrderCancelRequest,
	// OrderCancelReplaceRequest). QuickFIX/Go has already validated the
	// message against the dictionary; the simulator applies FixLab's
	// order-level handling.
	if s.Role == RoleAcceptor && s.sim != nil && ev.Direction == messages.Inbound &&
		(ev.MsgType == "D" || ev.MsgType == "F" || ev.MsgType == "G") {
		if err := s.sim.OnAppMessage(rec); err != nil {
			l := s.logger
			if l == nil {
				l = slog.Default()
			}
			l.Warn("simulator: inbound app message rejected",
				"token", shortToken(s.Token),
				"msg_type", ev.MsgType,
				"err", err)
			s.hub.Publish(websocket.Event{
				Type:      websocket.EventSessionError,
				SessionID: s.Token,
				Timestamp: time.Now(),
				Payload:   map[string]string{"error": err.Error()},
			})
		}
	}

	// Initiator-side routing (phase 5):
	//   - our outbound logon (35=A) completes the LOGON_SENT step;
	//   - the remote's ExecutionReports (35=8) and OrderCancelRejects
	//     (35=9) drive the OUTBOUND blotter via the simulator.
	if s.Role == RoleInitiator {
		if ev.Direction == messages.Outbound && ev.MsgType == "A" {
			s.setInitiatorStatus(StatusLogonSent, "")
		}
		if s.sim != nil && ev.Direction == messages.Inbound &&
			(ev.MsgType == "8" || ev.MsgType == "9") {
			if err := s.sim.ApplyRemoteExecutionReport(rec); err != nil {
				l := s.loggerOrDefault()
				l.Warn("simulator: remote report not applied",
					"token", shortToken(s.Token),
					"msg_type", ev.MsgType,
					"err", err)
				s.hub.Publish(websocket.Event{
					Type:      websocket.EventSessionError,
					SessionID: s.Token,
					Timestamp: time.Now(),
					Payload:   map[string]string{"error": err.Error()},
				})
			}
		}
	}
}

// --- simulator.Sender -------------------------------------------------

// NextExecSeq implements simulator.Sender.
func (s *Session) NextExecSeq() int64 { return s.execSeq.Add(1) }

// SendExecution implements simulator.Sender: it enforces the session's
// application-message quota (spec §44) and then transmits through the
// FIX engine, which validates and stamps the message (spec §25). The
// check and the send are serialized so concurrent executions cannot
// overshoot the quota; the count itself is incremented by OnMessage when
// the engine reports the outbound message.
func (s *Session) SendExecution(msg *quickfix.Message) error {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	return s.sendExecutionLocked(msg)
}

// sendExecutionLocked is the quota-checked send; the caller must hold
// execMu (InjectOrder holds it across its whole check-mutate-send).
func (s *Session) sendExecutionLocked(msg *quickfix.Message) error {
	if s.replayMode.Load() {
		// Scenario replay (phase 2.3) re-enacts the application story
		// without touching the wire: no quota check, no transmission,
		// no quota consumption. A synthetic history record marked
		// replayed keeps /messages and the live feed consistent.
		s.pushReplayedOutbound(msg)
		return nil
	}
	if limit := s.appMessageLimit; limit > 0 && s.AppMsgCount.Load() >= limit {
		return &simulator.QuotaExceededError{Limit: limit}
	}
	eh := s.engineHandle
	if eh == nil {
		return errors.New("session: FIX engine is not running")
	}
	return eh.SendMessage(msg)
}

// pushReplayedOutbound records a synthetic outbound execution report
// during scenario replay (phase 2.3): it lands in history and streams
// as FIX_MSG_OUT, both marked replayed, but nothing is transmitted.
func (s *Session) pushReplayedOutbound(msg *quickfix.Message) {
	raw := strings.ReplaceAll(msg.String(), "\x01", "|")
	msgType, _ := msg.Header.GetString(quickfix.Tag(35))
	rec := &messages.Record{
		Type:         "FIX_MESSAGE",
		Direction:    messages.Outbound,
		MsgType:      msgType,
		MsgName:      s.Dictionary().MessageName(msgType),
		RawFIX:       raw,
		SenderCompID: s.SenderCompID,
		TargetCompID: s.TargetCompID,
		Timestamp:    time.Now(),
		Fields:       parseFields(raw, s.Dictionary()),
		Replayed:     true,
	}
	s.history.Push(rec)
	s.hub.Publish(websocket.Event{
		Type:      websocket.EventFIXMsgOut,
		SessionID: s.Token,
		Timestamp: rec.Timestamp,
		Payload:   rec,
	})
}

// PublishOrderEvent implements simulator.Sender.
func (s *Session) PublishOrderEvent(eventType string, o *orders.Order) {
	s.hub.Publish(websocket.Event{
		Type:      eventType,
		SessionID: s.Token,
		Timestamp: time.Now(),
		Payload:   o,
	})
}

// PublishExecutionSent implements simulator.Sender.
func (s *Session) PublishExecutionSent(summary *simulator.ExecutionSummary) {
	s.hub.Publish(websocket.Event{
		Type:      websocket.EventExecutionSent,
		SessionID: s.Token,
		Timestamp: time.Now(),
		Payload:   summary,
	})
}

// LogFixMessages toggles per-message logging for a session (useful for
// debugging and acceptance tests; off by default to keep logs quiet).
var LogFixMessages = false

func init() {
	if v := os.Getenv("FIXLAB_LOG_FIX_MESSAGES"); v == "1" || v == "true" {
		LogFixMessages = true
	}
}

// --- guard callbacks ---------------------------------------------------

func (s *Session) onTCPConnect() {
	t := true
	s.setStatusFields(&t, nil, nil)
}

func (s *Session) onTCPDisconnect() {
	f := false
	s.setStatusFields(&f, nil, &f)
	s.recordStep(scenario.KindDisconnect, map[string]any{})
}

func (s *Session) onInboundLogon() {
	t := true
	s.setStatusFields(nil, &t, nil)
}

// MessageHistory returns up to limit recent records (0 = all).
func (s *Session) MessageHistory(limit int) []*messages.Record {
	return s.history.List(limit)
}

// MessageCount returns the number of records currently in history.
func (s *Session) MessageCount() int {
	return s.history.Len()
}

// parseFields splits a pipe-delimited raw FIX message into dictionary-
// enriched fields for the message inspector (spec §18, §22).
func parseFields(raw string, dict *dictionary.Dictionary) []messages.Field {
	fields := []messages.Field{}
	start := 0
	for i := 0; i <= len(raw); i++ {
		if i < len(raw) && raw[i] != '|' {
			continue
		}
		part := raw[start:i]
		start = i + 1
		if part == "" {
			continue
		}
		eq := -1
		for j := 0; j < len(part); j++ {
			if part[j] == '=' {
				eq = j
				break
			}
		}
		if eq <= 0 {
			continue
		}
		var tag int
		for j := 0; j < eq; j++ {
			if part[j] < '0' || part[j] > '9' {
				tag = -1
				break
			}
			tag = tag*10 + int(part[j]-'0')
		}
		if tag < 0 {
			continue
		}
		value := part[eq+1:]
		f := messages.Field{Tag: tag, Value: value}
		if dict != nil {
			f.Name = dict.FieldName(tag)
			f.EnumDescription = dict.EnumDescription(tag, value)
		}
		fields = append(fields, f)
	}
	return fields
}
