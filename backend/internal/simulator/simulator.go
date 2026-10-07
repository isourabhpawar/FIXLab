// Package simulator processes inbound orders and generates
// ExecutionReports (spec §12, phase 3: trading simulation).
//
// The simulator owns order state transitions and ExecutionReport
// generation. It never touches the FIX session protocol itself: all
// transmission goes through Sender.SendExecution, which routes the
// message through the QuickFIX/Go engine (header/trailer stamping) after
// the session's application-message quota check.
package simulator

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"

	"fixlab.dev/fixlab/backend/internal/common"
	"fixlab.dev/fixlab/backend/internal/messages"
	"fixlab.dev/fixlab/backend/internal/orders"
	"fixlab.dev/fixlab/backend/internal/rules"
	"fixlab.dev/fixlab/backend/internal/scenario"
)

// FIX tags used by the simulator (kept local, no generated field
// packages — same convention as the engine package).
const (
	tagMsgType          = quickfix.Tag(35)
	tagClOrdID          = quickfix.Tag(11)
	tagOrigClOrdID      = quickfix.Tag(41)
	tagOrderID          = quickfix.Tag(37)
	tagExecID           = quickfix.Tag(17)
	tagExecType         = quickfix.Tag(150)
	tagOrdStatus        = quickfix.Tag(39)
	tagSymbol           = quickfix.Tag(55)
	tagSide             = quickfix.Tag(54)
	tagOrderQty         = quickfix.Tag(38)
	tagOrdType          = quickfix.Tag(40)
	tagPrice            = quickfix.Tag(44)
	tagCumQty           = quickfix.Tag(14)
	tagLastQty          = quickfix.Tag(32)
	tagLastPx           = quickfix.Tag(31)
	tagLeavesQty        = quickfix.Tag(151)
	tagAvgPx            = quickfix.Tag(6)
	tagText             = quickfix.Tag(58)
	tagOrdRejReason     = quickfix.Tag(103)
	tagCxlRejReason     = quickfix.Tag(102)
	tagCxlRejResponseTo = quickfix.Tag(434)
)

// ExecType values (tag 150).
const (
	ExecTypeNew         = "0"
	ExecTypePartialFill = "D"
	ExecTypeFill        = "F"
	ExecTypeCanceled    = "4"
	ExecTypeReplaced    = "5"
	ExecTypeRejected    = "8"
)

// OrdStatus values (tag 39).
const (
	OrdStatusNew             = "0"
	OrdStatusPartiallyFilled = "1"
	OrdStatusFilled          = "2"
	OrdStatusCanceled        = "4"
	OrdStatusRejected        = "8"
)

// CxlRejResponseTo values (tag 434).
const (
	CxlRejResponseToCancel  = "1"
	CxlRejResponseToReplace = "2"
)

// Inbound application message types handled by the simulator.
const (
	msgNewOrderSingle             = "D"
	msgOrderCancelRequest         = "F"
	msgOrderCancelReplaceRequest  = "G"
	msgExecutionReport            = "8"
	msgOrderCancelReject          = "9"
)

// QuotaExceededError is returned when an execution would exceed the
// session's application-message quota (spec §44). The API maps it to
// HTTP 429.
type QuotaExceededError struct {
	Limit int64
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("simulator: session application-message quota exceeded (limit %d); destroy the session or wait for expiry", e.Limit)
}

// InputError marks a malformed execution request: the API maps it to
// HTTP 400.
type InputError struct{ msg string }

func (e *InputError) Error() string { return e.msg }

func inputErr(format string, args ...any) *InputError {
	return &InputError{msg: "simulator: " + fmt.Sprintf(format, args...)}
}

// StateError marks an invalid lifecycle transition (e.g. filling a
// filled order): the API maps it to HTTP 409.
type StateError struct{ msg string }

func (e *StateError) Error() string { return e.msg }

func stateErr(format string, args ...any) *StateError {
	return &StateError{msg: "simulator: " + fmt.Sprintf(format, args...)}
}

// IsNotFound reports whether err is an unknown-order error.
func IsNotFound(err error) bool { return errors.Is(err, orders.ErrOrderNotFound) }

// ExecutionSummary describes one generated execution for the
// EXECUTION_SENT event and the REST execute response.
type ExecutionSummary struct {
	MsgType   string  `json:"msgType"` // "8" or "9"
	ExecType  string  `json:"execType,omitempty"`
	OrdStatus string  `json:"ordStatus,omitempty"`
	ExecID    string  `json:"execId,omitempty"`
	OrderID   string  `json:"orderId"`
	ClOrdID   string  `json:"clOrdId"`
	CumQty    float64 `json:"cumQty"`
	LeavesQty float64 `json:"leavesQty"`
	AvgPx     float64 `json:"avgPx"`
	LastQty   float64 `json:"lastQty,omitempty"`
	LastPx    float64 `json:"lastPx,omitempty"`
	RawFix    string  `json:"rawFix"` // pipe-delimited, post engine stamping
	// Replayed marks executions produced by a scenario replay
	// (phase 2.3): the report was never transmitted on the wire.
	Replayed bool `json:"replayed,omitempty"`
}

// ExecutionResult is the outcome of one execution action.
type ExecutionResult struct {
	Order   *orders.Order     `json:"order"`
	Summary *ExecutionSummary `json:"execution"`
}

// Sender is the session-facing half of the simulator contract. It is
// implemented by *session.Session; the interface keeps the import graph
// acyclic.
type Sender interface {
	// NextExecSeq returns the next per-session sequence number used to
	// mint OrderID (tag 37) and ExecID (tag 17) values.
	NextExecSeq() int64
	// SendExecution transmits msg through the FIX engine after the
	// session's quota check. The message is validated through the FIX
	// engine (session-level stamping) before transmission (spec §25).
	SendExecution(msg *quickfix.Message) error
	// PublishOrderEvent streams ORDER_CREATED / ORDER_UPDATED.
	PublishOrderEvent(eventType string, o *orders.Order)
	// PublishExecutionSent streams EXECUTION_SENT.
	PublishExecutionSent(summary *ExecutionSummary)
}

// Simulator turns inbound NewOrderSingle / OrderCancelRequest /
// OrderCancelReplaceRequest messages into order records and
// ExecutionReports, and executes browser-driven actions on working
// orders (spec §24–§29).
type Simulator interface {
	// OnAppMessage routes one inbound application message (35=D/F/G).
	OnAppMessage(rec *messages.Record) error
	// Fill fills the entire remaining quantity at price.
	Fill(clOrdID string, qty, price float64) (*ExecutionResult, error)
	// PartialFill records a partial fill of lastQty at lastPx.
	PartialFill(clOrdID string, lastQty, lastPx float64) (*ExecutionResult, error)
	// Reject rejects the order with an optional reason code and text.
	Reject(clOrdID, ordRejReason, text string) (*ExecutionResult, error)
	// CancelAccept accepts a pending cancel request (inbound 35=F).
	CancelAccept(clOrdID, text string) (*ExecutionResult, error)
	// CancelReject rejects a pending cancel request with a 35=9.
	CancelReject(clOrdID, cxlRejReason, text string) (*ExecutionResult, error)
	// ReplaceAccept accepts a pending replace request (inbound 35=G).
	ReplaceAccept(clOrdID string) (*ExecutionResult, error)
	// ReplaceReject rejects a pending replace request with a 35=9.
	ReplaceReject(clOrdID, cxlRejReason, text string) (*ExecutionResult, error)
	// ApplyRemoteExecutionReport applies an inbound 35=8/9 from the
	// remote counterparty to an OUTBOUND-injected order (phase 5,
	// initiator mode). The remote's report is authoritative.
	ApplyRemoteExecutionReport(rec *messages.Record) error
	// Stochastic returns the session's stochastic simulation policy
	// (phase 2.1, spec §48): the outcome draws for inbound orders when
	// no deterministic rule fires.
	Stochastic() *StochasticPolicy
	// ReplayOrder re-creates a blotter record from recorded inbound 35=D
	// fields (phase 2.3). Rules, stochastic, the kill switch and the New
	// acknowledgement are all skipped: replay re-applies the recorded
	// actions, not the automation. The order is marked replayed.
	ReplayOrder(fields map[int]string) (*orders.Order, error)
	// ReplayInbound re-applies a recorded inbound 35=F/G (phase 2.3),
	// staging the pending cancel/replace exactly as the live path does.
	ReplayInbound(msgType string, fields map[int]string) error
	// ReplayExecution re-applies one recorded execution action
	// (phase 2.3) through the normal execution path.
	ReplayExecution(action string, p scenario.ExecutionPayload) (*ExecutionResult, error)
	// SetReplayMode marks the session as replaying (phase 2.3): the
	// shared scenario recorder is paused, outbound execution sends are
	// not transmitted and bypass the quota, and execution summaries are
	// marked replayed.
	SetReplayMode(on bool)
}

// KillSwitchRejectText is the 58= text of the immediate reject sent
// while the kill switch is on (spec §31). It names the halt so the
// developer's engine can distinguish it from a rule or manual reject.
const KillSwitchRejectText = "VENUE HALTED: kill switch enabled"

// Config wires a simulator.
type Config struct {
	Store  orders.Store
	Sender Sender
	Log    *slog.Logger
	// Rules is the phase-4 deterministic rule engine (nil disables
	// rule evaluation entirely — the phase-3 behavior).
	Rules rules.Engine
	// Metrics receives the rule_matches counter (spec §45); nil skips it.
	Metrics *common.Metrics
	// Recorder is the session's automatic scenario log (phase 2.3,
	// spec §48). Nil disables execution recording.
	Recorder *scenario.Recorder
}

// New returns a Simulator. Store and Sender are required.
func New(cfg Config) (Simulator, error) {
	if cfg.Store == nil {
		return nil, errors.New("simulator: order store is required")
	}
	if cfg.Sender == nil {
		return nil, errors.New("simulator: sender is required")
	}
	s := &simulator{store: cfg.Store, sender: cfg.Sender, log: cfg.Log, rules: cfg.Rules, metrics: cfg.Metrics, recorder: cfg.Recorder}
	s.stochastic = newStochasticPolicy(s, cfg.Log, cfg.Metrics)
	return s, nil
}

type simulator struct {
	store    orders.Store
	sender   Sender
	log      *slog.Logger
	rules    rules.Engine
	metrics  *common.Metrics
	recorder *scenario.Recorder
	// replayMode is set by the phase-2.3 replay player: executions are
	// not transmitted, the quota is bypassed, and summaries are marked
	// replayed. It is only read/written while holding mu (all execution
	// paths hold it) or via SetReplayMode.
	replayMode bool
	// stochastic is the phase-2.1 stochastic simulation policy: it fires
	// on inbound NewOrderSingle only when enabled AND no deterministic
	// rule fired (kill switch is evaluated before both).
	stochastic *StochasticPolicy
	// mu serializes executions: two rapid fills on the same order cannot
	// interleave their read-modify-write of CumQty/LeavesQty/AvgPx.
	mu sync.Mutex
}

// Stochastic returns the session's stochastic simulation policy.
func (s *simulator) Stochastic() *StochasticPolicy { return s.stochastic }

func (s *simulator) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return slog.Default()
}

// --- inbound routing --------------------------------------------------

// OnAppMessage routes one inbound application message to its handler.
func (s *simulator) OnAppMessage(rec *messages.Record) error {
	fields := fieldMap(rec.Fields)
	switch rec.MsgType {
	case msgNewOrderSingle:
		return s.onNewOrder(fields)
	case msgOrderCancelRequest:
		return s.onCancelRequest(fields)
	case msgOrderCancelReplaceRequest:
		return s.onReplaceRequest(fields)
	default:
		return fmt.Errorf("simulator: unsupported app message type %q", rec.MsgType)
	}
}

func fieldMap(fields []messages.Field) map[int]string {
	m := make(map[int]string, len(fields))
	for _, f := range fields {
		m[f.Tag] = f.Value
	}
	return m
}

// buildOrderRecord validates inbound 35=D fields and builds the
// blotter record. It is shared by the live path (onNewOrder) and the
// phase-2.3 replay player (ReplayOrder).
func (s *simulator) buildOrderRecord(f map[int]string) (*orders.Order, error) {
	clOrdID := strings.TrimSpace(f[int(tagClOrdID)])
	if clOrdID == "" {
		return nil, inputErr("NewOrderSingle missing ClOrdID (11)")
	}
	symbol := f[int(tagSymbol)]
	side := f[int(tagSide)]
	qty, err := parseQty(f[int(tagOrderQty)])
	if err != nil || qty <= 0 {
		return nil, inputErr("NewOrderSingle %s has invalid OrderQty (38): %q", clOrdID, f[int(tagOrderQty)])
	}
	ordType := f[int(tagOrdType)]
	price := 0.0
	if v := f[int(tagPrice)]; v != "" {
		if price, err = strconv.ParseFloat(v, 64); err != nil {
			return nil, inputErr("NewOrderSingle %s has invalid Price (44): %q", clOrdID, v)
		}
	}

	seq := s.sender.NextExecSeq()
	return &orders.Order{
		OrderID:   fmt.Sprintf("FLB%06d", seq),
		ClOrdID:   clOrdID,
		Direction: orders.DirectionInbound,
		Symbol:    symbol,
		Side:      side,
		OrderQty:  qty,
		Price:     price,
		OrdType:   ordType,
		LeavesQty: qty,
		Status:    orders.StatusNew,
	}, nil
}

// onNewOrder handles inbound 35=D: create the blotter record and publish
// ORDER_CREATED, then decide how the order is acknowledged:
//   - kill switch on -> immediate reject (150=8/39=8); rules are skipped.
//   - a rule matches -> its action chain runs asynchronously and replaces
//     the immediate ack (an explicit ACK_NEW action can send it).
//   - no rule -> the phase-3 immediate New acknowledgement
//     (ExecType=0/New, OrdStatus=0/New) so the client's order state stays
//     consistent with what FixLab reports.
func (s *simulator) onNewOrder(f map[int]string) error {
	s.mu.Lock()

	o, err := s.buildOrderRecord(f)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if err := s.store.Add(o); err != nil {
		s.mu.Unlock()
		return stateErr("NewOrderSingle %s: %v", o.ClOrdID, err)
	}
	s.sender.PublishOrderEvent("ORDER_CREATED", o)
	s.logger().Info("simulator: order created",
		"cl_ord_id", o.ClOrdID, "symbol", o.Symbol, "qty", o.OrderQty, "price", o.Price)
	re := s.rules
	s.mu.Unlock()

	// Phase 4: kill switch is evaluated before every rule (spec \u00a731).
	if re != nil && re.KillSwitch() {
		return s.killSwitchReject(o)
	}
	// Phase 4: deterministic rule evaluation - the first matching rule
	// in priority order fires. Its action chain runs on its own
	// goroutine so a DELAY never blocks the FIX message pump; the
	// simulator mutex keeps the state transitions serialized.
	if re != nil {
		if rule := re.Evaluate(msgNewOrderSingle, f); rule != nil {
			if s.metrics != nil {
				s.metrics.RuleMatches.Add(1)
			}
			go s.runRuleChain(o.ClOrdID, rule)
			return nil
		}
	}
	// Phase 2.1: stochastic simulation - fires only when enabled AND no
	// deterministic rule fired. It never overrides the kill switch or
	// rules. The outcome draw happens synchronously (deterministic RNG
	// order); the sampled-latency sleep and the execution run on a
	// goroutine so the FIX pump is never blocked.
	if s.stochastic != nil && s.stochastic.Enabled() {
		s.stochastic.Roll(o)
		return nil
	}
	// Phase-3 default: immediate New acknowledgement (spec \u00a725
	// invariant: the client must see the order as live).
	return s.sendNewAck(o)
}

// killSwitchReject rejects the order immediately with the halt text.
// Rules are not evaluated (spec \u00a731).
func (s *simulator) killSwitchReject(o *orders.Order) error {
	s.logger().Warn("rules: kill switch rejecting order", "cl_ord_id", o.ClOrdID)
	if _, err := s.Reject(o.ClOrdID, "", KillSwitchRejectText); err != nil {
		var qe *QuotaExceededError
		if errors.As(err, &qe) {
			s.logger().Warn("rules: kill-switch reject suppressed by quota",
				"cl_ord_id", o.ClOrdID, "err", err)
			return nil
		}
		return fmt.Errorf("simulator: kill-switch reject for %s: %w", o.ClOrdID, err)
	}
	return nil
}

// sendNewAck transmits the New acknowledgement (150=0/39=0).
func (s *simulator) sendNewAck(o *orders.Order) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ack := s.executionReport(o, erParams{
		clOrdID:   o.ClOrdID,
		execType:  ExecTypeNew,
		ordStatus: OrdStatusNew,
	})
	if _, err := s.send(o, ack, ExecTypeNew, OrdStatusNew, 0, 0); err != nil {
		var qe *QuotaExceededError
		if errors.As(err, &qe) {
			// Quota-exhausted: the order exists on the blotter, but no
			// ack could be sent. Log loudly; the browser still shows it.
			s.logger().Warn("simulator: order ack suppressed by quota",
				"cl_ord_id", o.ClOrdID, "err", err)
			return nil
		}
		return fmt.Errorf("simulator: send New ack for %s: %w", o.ClOrdID, err)
	}
	s.recordExecution(scenario.ExecutionPayload{Action: scenario.ActionAckNew, ClOrdID: o.ClOrdID})
	return nil
}

// sendNewAckByID sends the New acknowledgement for a working order; used
// by the ACK_NEW rule action.
func (s *simulator) sendNewAckByID(clOrdID string) error {
	s.mu.Lock()
	o, ok := s.store.Get(clOrdID)
	s.mu.Unlock()
	if !ok {
		return orders.ErrOrderNotFound
	}
	if o.Status.Terminal() {
		return stateErr("ACK_NEW for terminal order %s (%s)", clOrdID, o.Status)
	}
	return s.sendNewAck(o)
}

// --- scenario recording (phase 2.3, spec §48) -------------------------

// recordExecution appends one execution step to the session's scenario
// log. It is a no-op when recording is disabled or when the simulator is
// in replay mode (the re-enactment must not record itself).
func (s *simulator) recordExecution(p scenario.ExecutionPayload) {
	if s.replayMode || s.recorder == nil {
		return
	}
	s.recorder.Record(scenario.KindExecution, p)
}

// --- scenario replay (phase 2.3, spec §48) ----------------------------

// ReplayOrder re-creates a blotter record from recorded inbound 35=D
// fields. Rules, stochastic, the kill switch and the New acknowledgement
// are skipped: replay re-applies the recorded actions, not the
// automation. The order is marked replayed.
func (s *simulator) ReplayOrder(f map[int]string) (*orders.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, err := s.buildOrderRecord(f)
	if err != nil {
		return nil, err
	}
	o.Replayed = true
	if err := s.store.Add(o); err != nil {
		return nil, stateErr("replay NewOrderSingle %s: %v", o.ClOrdID, err)
	}
	s.sender.PublishOrderEvent("ORDER_CREATED", o)
	s.logger().Info("simulator: replay order created", "cl_ord_id", o.ClOrdID)
	return o, nil
}

// ReplayInbound re-applies a recorded inbound 35=F/G, staging the
// pending cancel/replace exactly as the live path does.
func (s *simulator) ReplayInbound(msgType string, fields map[int]string) error {
	switch msgType {
	case msgOrderCancelRequest:
		return s.onCancelRequest(fields)
	case msgOrderCancelReplaceRequest:
		return s.onReplaceRequest(fields)
	default:
		return inputErr("replay: inbound msgType %q is not replayable (want F or G)", msgType)
	}
}

// ReplayExecution re-applies one recorded execution action through the
// normal execution path (blotter bookkeeping, WS events and execution
// summaries are identical to the live path; the send itself is synthetic
// while replay mode is on).
func (s *simulator) ReplayExecution(action string, p scenario.ExecutionPayload) (*ExecutionResult, error) {
	switch action {
	case scenario.ActionAckNew:
		if err := s.sendNewAckByID(p.ClOrdID); err != nil {
			return nil, err
		}
		o, ok := s.store.Get(p.ClOrdID)
		if !ok {
			return nil, orders.ErrOrderNotFound
		}
		return &ExecutionResult{Order: o}, nil
	case scenario.ActionFill:
		return s.Fill(p.ClOrdID, p.Qty, p.Price)
	case scenario.ActionPartialFill:
		return s.PartialFill(p.ClOrdID, p.Qty, p.Price)
	case scenario.ActionReject:
		return s.Reject(p.ClOrdID, p.OrdRejReason, p.Text)
	case scenario.ActionCancelAccept:
		return s.CancelAccept(p.ClOrdID, p.Text)
	case scenario.ActionCancelReject:
		return s.CancelReject(p.ClOrdID, p.CxlRejReason, p.Text)
	case scenario.ActionReplaceAccept:
		return s.ReplaceAccept(p.ClOrdID)
	case scenario.ActionReplaceReject:
		return s.ReplaceReject(p.ClOrdID, p.CxlRejReason, p.Text)
	default:
		return nil, inputErr("replay: unknown execution action %q", action)
	}
}

// SetReplayMode marks the session as replaying: the shared scenario
// recorder is paused (the re-enactment is not recorded as new history),
// outbound execution sends are not transmitted and bypass the quota, and
// execution summaries are marked replayed.
func (s *simulator) SetReplayMode(on bool) {
	s.mu.Lock()
	s.replayMode = on
	s.mu.Unlock()
	if s.recorder != nil {
		s.recorder.SetPaused(on)
	}
}

// runRuleChain executes a rule's action chain in order (spec \u00a733).
// DELAY sleeps, then the chain continues. Any action failure stops the
// chain and leaves the order in the last consistent state - e.g. when a
// manual browser execution raced the chain and terminally filled the
// order, subsequent actions fail with a StateError instead of corrupting
// CumQty/LeavesQty/AvgPx. Rule-fired executions consume the session
// application-message quota and emit the same ORDER_UPDATED /
// EXECUTION_SENT events as manual executions.
func (s *simulator) runRuleChain(clOrdID string, rule *rules.Rule) {
	l := s.logger().With("rule_id", rule.ID, "rule_name", rule.Name, "cl_ord_id", clOrdID)
	l.Info("rules: firing rule")
	for i, a := range rule.Actions {
		var err error
		switch a.Type {
		case rules.ActionDelay:
			l.Info("rules: delay", "delay_ms", a.DelayMs)
			time.Sleep(time.Duration(a.DelayMs) * time.Millisecond)
			continue
		case rules.ActionAckNew:
			err = s.sendNewAckByID(clOrdID)
		case rules.ActionFullFill:
			err = s.ruleFullFill(clOrdID, a.Price)
		case rules.ActionPartialFill:
			_, err = s.PartialFill(clOrdID, a.Qty, a.Price)
		case rules.ActionReject:
			_, err = s.Reject(clOrdID, a.OrdRejReason, a.Text)
		default:
			err = inputErr("unknown rule action %q", a.Type)
		}
		if err != nil {
			l.Warn("rules: action chain stopped",
				"action_index", i, "action", string(a.Type), "err", err)
			return
		}
	}
	l.Info("rules: chain complete")
}

// ruleFullFill fills the order's full remaining quantity at the action's
// price override, or the order's own price when the action has none
// (market orders therefore require the price param).
func (s *simulator) ruleFullFill(clOrdID string, priceParam float64) error {
	s.mu.Lock()
	o, ok := s.store.Get(clOrdID)
	s.mu.Unlock()
	if !ok {
		return orders.ErrOrderNotFound
	}
	price := priceParam
	if price <= 0 {
		price = o.Price
	}
	if price <= 0 {
		return inputErr("FULL_FILL for %s: no price - order is not a limit order and the action sets no price", clOrdID)
	}
	if o.LeavesQty <= 0 {
		return stateErr("FULL_FILL for %s: nothing left to fill", clOrdID)
	}
	_, err := s.Fill(clOrdID, o.LeavesQty, price)
	return err
}

// onCancelRequest handles inbound 35=F: the order moves to PENDING_CANCEL
// and waits for the browser to accept or reject (spec §28).
func (s *simulator) onCancelRequest(f map[int]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cxlClOrdID := strings.TrimSpace(f[int(tagClOrdID)])
	origClOrdID := strings.TrimSpace(f[int(tagOrigClOrdID)])
	if cxlClOrdID == "" || origClOrdID == "" {
		return inputErr("OrderCancelRequest missing ClOrdID (11) or OrigClOrdID (41)")
	}
	o, ok := s.store.Get(origClOrdID)
	if !ok {
		return fmt.Errorf("simulator: OrderCancelRequest for unknown order %s: %w", origClOrdID, orders.ErrOrderNotFound)
	}
	if o.Status.Terminal() {
		return stateErr("OrderCancelRequest for terminal order %s (%s)", origClOrdID, o.Status)
	}
	if o.Status == orders.StatusPendingCancel {
		return stateErr("order %s already has a pending cancel", origClOrdID)
	}
	o.PrevStatus = o.Status
	o.Status = orders.StatusPendingCancel
	o.CancelReqClOrdID = cxlClOrdID
	if err := s.store.Update(o); err != nil {
		return err
	}
	s.sender.PublishOrderEvent("ORDER_UPDATED", o)
	s.logger().Info("simulator: cancel requested",
		"cl_ord_id", origClOrdID, "cxl_cl_ord_id", cxlClOrdID)
	return nil
}

// onReplaceRequest handles inbound 35=G: the requested quantity/price
// are staged on the order and wait for the browser to accept or reject
// (spec §29). No ExecutionReport is sent yet.
func (s *simulator) onReplaceRequest(f map[int]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rplClOrdID := strings.TrimSpace(f[int(tagClOrdID)])
	origClOrdID := strings.TrimSpace(f[int(tagOrigClOrdID)])
	if rplClOrdID == "" || origClOrdID == "" {
		return inputErr("OrderCancelReplaceRequest missing ClOrdID (11) or OrigClOrdID (41)")
	}
	o, ok := s.store.Get(origClOrdID)
	if !ok {
		return fmt.Errorf("simulator: OrderCancelReplaceRequest for unknown order %s: %w", origClOrdID, orders.ErrOrderNotFound)
	}
	if o.Status.Terminal() {
		return stateErr("OrderCancelReplaceRequest for terminal order %s (%s)", origClOrdID, o.Status)
	}
	if o.Status == orders.StatusPendingCancel {
		return stateErr("order %s has a pending cancel; replace refused", origClOrdID)
	}
	newQty, err := parseQty(f[int(tagOrderQty)])
	if err != nil || newQty <= 0 {
		return inputErr("OrderCancelReplaceRequest %s has invalid OrderQty (38): %q", rplClOrdID, f[int(tagOrderQty)])
	}
	if newQty <= o.CumQty {
		return stateErr("replace quantity %.4g must exceed filled quantity %.4g", newQty, o.CumQty)
	}
	newPrice := o.Price
	if v := f[int(tagPrice)]; v != "" {
		if newPrice, err = strconv.ParseFloat(v, 64); err != nil {
			return inputErr("OrderCancelReplaceRequest %s has invalid Price (44): %q", rplClOrdID, v)
		}
	}
	o.PendingReplace = &orders.PendingReplace{ClOrdID: rplClOrdID, OrderQty: newQty, Price: newPrice}
	if err := s.store.Update(o); err != nil {
		return err
	}
	s.sender.PublishOrderEvent("ORDER_UPDATED", o)
	s.logger().Info("simulator: replace requested",
		"cl_ord_id", origClOrdID, "new_qty", newQty, "new_price", newPrice)
	return nil
}

// --- execution actions ------------------------------------------------

// Fill fills the entire remaining quantity at price (spec §25). The
// provided qty must equal the remaining leaves quantity; use
// PARTIAL_FILL for anything smaller.
func (s *simulator) Fill(clOrdID string, qty, price float64) (*ExecutionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, err := s.workingOrder(clOrdID)
	if err != nil {
		return nil, err
	}
	if qty <= 0 || price <= 0 {
		return nil, inputErr("FILL requires qty > 0 and price > 0")
	}
	if qty != o.LeavesQty {
		return nil, inputErr("FILL quantity %.4g must equal remaining leaves quantity %.4g (use PARTIAL_FILL for a partial)", qty, o.LeavesQty)
	}
	lastQty, lastPx := o.LeavesQty, price
	o.CumQty += lastQty
	o.LeavesQty = 0
	o.AvgPx = weightedAvg(o.CumQty-lastQty, o.AvgPx, lastQty, lastPx)
	o.Status = orders.StatusFilled
	res, err := s.execute(o, erParams{
		clOrdID:     clOrdID,
		execType:    ExecTypeFill,
		ordStatus:   OrdStatusFilled,
		lastQty:     lastQty,
		lastPx:      lastPx,
		includeLast: true,
	})
	if err == nil {
		s.recordExecution(scenario.ExecutionPayload{Action: scenario.ActionFill, ClOrdID: clOrdID, Qty: qty, Price: price})
	}
	return res, err
}

// PartialFill records a partial fill of lastQty at lastPx (spec §26),
// maintaining CumQty/LeavesQty/AvgPx. A partial that consumes the
// remainder completes the order as a full fill.
func (s *simulator) PartialFill(clOrdID string, lastQty, lastPx float64) (*ExecutionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, err := s.workingOrder(clOrdID)
	if err != nil {
		return nil, err
	}
	if lastQty <= 0 || lastPx <= 0 {
		return nil, inputErr("PARTIAL_FILL requires lastQty > 0 and lastPx > 0")
	}
	if lastQty > o.LeavesQty {
		return nil, inputErr("PARTIAL_FILL lastQty %.4g exceeds leaves quantity %.4g", lastQty, o.LeavesQty)
	}
	o.CumQty += lastQty
	o.LeavesQty -= lastQty
	o.AvgPx = weightedAvg(o.CumQty-lastQty, o.AvgPx, lastQty, lastPx)
	execType, ordStatus, status := ExecTypePartialFill, OrdStatusPartiallyFilled, orders.StatusPartiallyFilled
	if o.LeavesQty == 0 {
		execType, ordStatus, status = ExecTypeFill, OrdStatusFilled, orders.StatusFilled
	}
	o.Status = status
	res, err := s.execute(o, erParams{
		clOrdID:     clOrdID,
		execType:    execType,
		ordStatus:   ordStatus,
		lastQty:     lastQty,
		lastPx:      lastPx,
		includeLast: true,
	})
	if err == nil {
		s.recordExecution(scenario.ExecutionPayload{Action: scenario.ActionPartialFill, ClOrdID: clOrdID, Qty: lastQty, Price: lastPx})
	}
	return res, err
}

// Reject rejects the order (spec §27): ExecType=8/OrdStatus=8 with an
// optional OrdRejReason (103) and Text (58). Either a reason or text is
// required.
func (s *simulator) Reject(clOrdID, ordRejReason, text string) (*ExecutionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, err := s.workingOrder(clOrdID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(ordRejReason) == "" && strings.TrimSpace(text) == "" {
		return nil, inputErr("REJECT requires ordRejReason or text")
	}
	o.LeavesQty = 0
	o.Status = orders.StatusRejected
	res, err := s.execute(o, erParams{
		clOrdID:      clOrdID,
		execType:     ExecTypeRejected,
		ordStatus:    OrdStatusRejected,
		text:         text,
		ordRejReason: ordRejReason,
	})
	if err == nil {
		s.recordExecution(scenario.ExecutionPayload{Action: scenario.ActionReject, ClOrdID: clOrdID, OrdRejReason: ordRejReason, Text: text})
	}
	return res, err
}

// CancelAccept accepts a pending cancel request: ExecType=4/OrdStatus=4
// (spec §28).
func (s *simulator) CancelAccept(clOrdID, text string) (*ExecutionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, err := s.pendingCancelOrder(clOrdID)
	if err != nil {
		return nil, err
	}
	o.LeavesQty = 0
	o.Status = orders.StatusCanceled
	o.CancelReqClOrdID = ""
	res, err := s.execute(o, erParams{
		clOrdID:   clOrdID,
		execType:  ExecTypeCanceled,
		ordStatus: OrdStatusCanceled,
		text:      text,
	})
	if err == nil {
		s.recordExecution(scenario.ExecutionPayload{Action: scenario.ActionCancelAccept, ClOrdID: clOrdID, Text: text})
	}
	return res, err
}

// CancelReject rejects a pending cancel request with a 35=9
// OrderCancelReject (spec §28). The order returns to its pre-request
// working state.
func (s *simulator) CancelReject(clOrdID, cxlRejReason, text string) (*ExecutionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, err := s.pendingCancelOrder(clOrdID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cxlRejReason) == "" && strings.TrimSpace(text) == "" {
		return nil, inputErr("CANCEL_REJECT requires cxlRejReason or text")
	}
	cxlClOrdID := o.CancelReqClOrdID
	underlying := underlyingOrdStatus(o)
	o.Status = o.PrevStatus
	o.PrevStatus = ""
	o.CancelReqClOrdID = ""
	msg := s.cancelReject(o, cxlClOrdID, CxlRejResponseToCancel, underlying, cxlRejReason, text)
	res, err := s.send9(o, msg, cxlClOrdID)
	if err == nil {
		s.recordExecution(scenario.ExecutionPayload{Action: scenario.ActionCancelReject, ClOrdID: clOrdID, CxlRejReason: cxlRejReason, Text: text})
	}
	return res, err
}

// ReplaceAccept accepts a pending replace request (spec §29): the staged
// quantity/price take effect and an ExecType=5 ExecutionReport is sent.
func (s *simulator) ReplaceAccept(clOrdID string) (*ExecutionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, err := s.pendingReplaceOrder(clOrdID)
	if err != nil {
		return nil, err
	}
	pr := o.PendingReplace
	if pr.OrderQty <= o.CumQty {
		return nil, stateErr("replace quantity %.4g must exceed filled quantity %.4g", pr.OrderQty, o.CumQty)
	}
	o.OrderQty = pr.OrderQty
	o.Price = pr.Price
	o.LeavesQty = pr.OrderQty - o.CumQty
	o.Status = orders.StatusReplaced
	rplClOrdID := pr.ClOrdID
	o.PendingReplace = nil
	ordStatus := OrdStatusNew
	if o.CumQty > 0 {
		ordStatus = OrdStatusPartiallyFilled
	}
	res, err := s.execute(o, erParams{
		clOrdID:     rplClOrdID,
		origClOrdID: clOrdID,
		execType:    ExecTypeReplaced,
		ordStatus:   ordStatus,
	})
	if err == nil {
		s.recordExecution(scenario.ExecutionPayload{Action: scenario.ActionReplaceAccept, ClOrdID: clOrdID})
	}
	return res, err
}

// ReplaceReject rejects a pending replace request with a 35=9
// OrderCancelReject (spec §29). The order keeps its current terms.
func (s *simulator) ReplaceReject(clOrdID, cxlRejReason, text string) (*ExecutionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, err := s.pendingReplaceOrder(clOrdID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cxlRejReason) == "" && strings.TrimSpace(text) == "" {
		return nil, inputErr("REPLACE_REJECT requires cxlRejReason or text")
	}
	rplClOrdID := o.PendingReplace.ClOrdID
	o.PendingReplace = nil
	underlying := underlyingOrdStatus(o)
	msg := s.cancelReject(o, rplClOrdID, CxlRejResponseToReplace, underlying, cxlRejReason, text)
	res, err := s.send9(o, msg, rplClOrdID)
	if err == nil {
		s.recordExecution(scenario.ExecutionPayload{Action: scenario.ActionReplaceReject, ClOrdID: clOrdID, CxlRejReason: cxlRejReason, Text: text})
	}
	return res, err
}

// --- helpers ----------------------------------------------------------

// workingOrder fetches an order that may still receive executions.
func (s *simulator) workingOrder(clOrdID string) (*orders.Order, error) {
	o, ok := s.store.Get(clOrdID)
	if !ok {
		return nil, orders.ErrOrderNotFound
	}
	if o.Status == orders.StatusPendingCancel {
		return nil, stateErr("order %s has a pending cancel request; resolve it first", clOrdID)
	}
	if !o.Status.Working() {
		return nil, stateErr("order %s is %s and cannot be executed", clOrdID, o.Status)
	}
	if o.PendingReplace != nil {
		return nil, stateErr("order %s has a pending replace request; resolve it first", clOrdID)
	}
	return o, nil
}

func (s *simulator) pendingCancelOrder(clOrdID string) (*orders.Order, error) {
	o, ok := s.store.Get(clOrdID)
	if !ok {
		return nil, orders.ErrOrderNotFound
	}
	if o.Status != orders.StatusPendingCancel {
		return nil, stateErr("order %s has no pending cancel request", clOrdID)
	}
	return o, nil
}

func (s *simulator) pendingReplaceOrder(clOrdID string) (*orders.Order, error) {
	o, ok := s.store.Get(clOrdID)
	if !ok {
		return nil, orders.ErrOrderNotFound
	}
	if o.PendingReplace == nil {
		return nil, stateErr("order %s has no pending replace request", clOrdID)
	}
	return o, nil
}

// underlyingOrdStatus maps the order's pre-pending state to a 39= value
// for 35=9 rejects.
func underlyingOrdStatus(o *orders.Order) string {
	st := o.Status
	if st == orders.StatusPendingCancel {
		st = o.PrevStatus
	}
	if o.CumQty > 0 {
		return OrdStatusPartiallyFilled
	}
	_ = st
	return OrdStatusNew
}

// execute persists the mutated order, transmits the report through the
// FIX engine, and publishes ORDER_UPDATED + EXECUTION_SENT. The store
// update happens before transmission so the blotter never lags a sent
// report.
func (s *simulator) execute(o *orders.Order, p erParams) (*ExecutionResult, error) {
	if err := s.store.Update(o); err != nil {
		return nil, err
	}
	msg := s.executionReport(o, p)
	sum, err := s.send(o, msg, p.execType, p.ordStatus, p.lastQty, p.lastPx)
	if err != nil {
		return nil, err
	}
	return &ExecutionResult{Order: o, Summary: sum}, nil
}

// send transmits msg through the FIX engine, then publishes
// ORDER_UPDATED and EXECUTION_SENT. The raw wire image is captured after
// the engine stamps the header/trailer (spec §25: validated through the
// FIX engine before transmission).
func (s *simulator) send(o *orders.Order, msg *quickfix.Message, execType, ordStatus string, lastQty, lastPx float64) (*ExecutionSummary, error) {
	if err := s.sender.SendExecution(msg); err != nil {
		return nil, err
	}
	raw := strings.ReplaceAll(msg.String(), "\x01", "|")
	execID, _ := msg.Body.GetString(tagExecID)
	sum := &ExecutionSummary{
		MsgType:   msgExecutionReport,
		ExecType:  execType,
		OrdStatus: ordStatus,
		ExecID:    execID,
		OrderID:   o.OrderID,
		ClOrdID:   o.ClOrdID,
		CumQty:    o.CumQty,
		LeavesQty: o.LeavesQty,
		AvgPx:     o.AvgPx,
		LastQty:   lastQty,
		LastPx:    lastPx,
		RawFix:    raw,
		Replayed:  s.replayMode,
	}
	s.sender.PublishOrderEvent("ORDER_UPDATED", o)
	s.sender.PublishExecutionSent(sum)
	return sum, nil
}

// send9 transmits a 35=9 OrderCancelReject and publishes the events.
func (s *simulator) send9(o *orders.Order, msg *quickfix.Message, reqClOrdID string) (*ExecutionResult, error) {
	if err := s.store.Update(o); err != nil {
		return nil, err
	}
	if err := s.sender.SendExecution(msg); err != nil {
		return nil, err
	}
	raw := strings.ReplaceAll(msg.String(), "\x01", "|")
	sum := &ExecutionSummary{
		MsgType:  msgOrderCancelReject,
		OrderID:  o.OrderID,
		ClOrdID:  reqClOrdID,
		RawFix:   raw,
		Replayed: s.replayMode,
	}
	s.sender.PublishOrderEvent("ORDER_UPDATED", o)
	s.sender.PublishExecutionSent(sum)
	return &ExecutionResult{Order: o, Summary: sum}, nil
}

type erParams struct {
	clOrdID      string // 11
	origClOrdID  string // 41, optional
	execType     string // 150
	ordStatus    string // 39
	lastQty      float64
	lastPx       float64
	includeLast  bool
	text         string
	ordRejReason string
}

// executionReport builds a 35=8 ExecutionReport carrying every field the
// FIX 4.4 dictionary requires (37, 17, 150, 39, 54, 151, 14, 6) plus the
// correlation fields the client's engine needs (11, 55, 38, 40, 44, 32,
// 31, 58, 103). QuickFIX/Go stamps 8/9/10/34/49/52/56 on send.
func (s *simulator) executionReport(o *orders.Order, p erParams) *quickfix.Message {
	seq := s.sender.NextExecSeq()
	msg := quickfix.NewMessage()
	msg.Header.SetString(tagMsgType, msgExecutionReport)
	// NOTE: msg.Body must be used directly — copying the FieldMap struct
	// (b := msg.Body) loses the tag ordering, which silently empties the
	// serialized body on the wire.
	b := &msg.Body
	b.SetString(tagOrderID, o.OrderID)
	b.SetString(tagExecID, fmt.Sprintf("EX%06d", seq))
	// NB: tag 20 (ExecTransType) is NOT set: it is not defined for
	// ExecutionReport in FIX 4.4, and counterparty engines session-reject
	// the report when it is present (observed: 35=3, 371=20).
	b.SetString(tagExecType, p.execType)
	b.SetString(tagOrdStatus, p.ordStatus)
	b.SetString(tagSymbol, o.Symbol)
	b.SetString(tagSide, o.Side)
	b.SetString(tagClOrdID, p.clOrdID)
	if p.origClOrdID != "" {
		b.SetString(tagOrigClOrdID, p.origClOrdID)
	}
	b.SetString(tagOrderQty, fmtQty(o.OrderQty))
	b.SetString(tagOrdType, o.OrdType)
	if o.Price > 0 {
		b.SetString(tagPrice, fmtQty(o.Price))
	}
	b.SetString(tagCumQty, fmtQty(o.CumQty))
	b.SetString(tagLeavesQty, fmtQty(o.LeavesQty))
	b.SetString(tagAvgPx, fmtQty(o.AvgPx))
	if p.includeLast {
		b.SetString(tagLastQty, fmtQty(p.lastQty))
		b.SetString(tagLastPx, fmtQty(p.lastPx))
	}
	if p.ordRejReason != "" {
		b.SetString(tagOrdRejReason, p.ordRejReason)
	}
	if p.text != "" {
		b.SetString(tagText, p.text)
	}
	return msg
}

// cancelReject builds a 35=9 OrderCancelReject with every required FIX
// 4.4 field (37, 11, 41, 39, 434).
func (s *simulator) cancelReject(o *orders.Order, reqClOrdID, responseTo, ordStatus, cxlRejReason, text string) *quickfix.Message {
	msg := quickfix.NewMessage()
	msg.Header.SetString(tagMsgType, msgOrderCancelReject)
	b := &msg.Body // see note above: never copy the FieldMap struct
	b.SetString(tagOrderID, o.OrderID)
	b.SetString(tagClOrdID, reqClOrdID)
	b.SetString(tagOrigClOrdID, o.ClOrdID)
	b.SetString(tagOrdStatus, ordStatus)
	b.SetString(tagCxlRejResponseTo, responseTo)
	if cxlRejReason != "" {
		b.SetString(tagCxlRejReason, cxlRejReason)
	}
	if text != "" {
		b.SetString(tagText, text)
	}
	return msg
}

func parseQty(v string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(v), 64)
}

func fmtQty(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// weightedAvg folds a fill of lastQty at lastPx into the running
// average: (cum*avg + lastQty*lastPx) / (cum+lastQty).
func weightedAvg(cumQty, avgPx, lastQty, lastPx float64) float64 {
	total := cumQty + lastQty
	if total == 0 {
		return 0
	}
	return (cumQty*avgPx + lastQty*lastPx) / total
}
