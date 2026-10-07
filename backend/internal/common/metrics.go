package common

import (
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
)

// NewLogger returns the process-wide structured logger (spec §45:
// structured logs).
func NewLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

// Metrics holds the backend counters from spec §45. All counters are
// atomic so engine callbacks, HTTP handlers, and the cleanup worker can
// update them concurrently.
type Metrics struct {
	ActiveSessions      atomic.Int64
	AcceptedConnections atomic.Int64
	RejectedConnections atomic.Int64
	FIXMessagesIn       atomic.Int64
	FIXMessagesOut      atomic.Int64
	ApplicationMessages atomic.Int64
	SequenceErrors      atomic.Int64
	AuthenticationFails atomic.Int64
	SessionExpirations  atomic.Int64
	RuleMatches         atomic.Int64 // phase 4
	ExecutionReports    atomic.Int64 // phase 3
	GuardDroppedFrames  atomic.Int64 // oversized FIX frames dropped by the TCP guard
	GuardRateLimited    atomic.Int64 // connections refused by per-IP rate limiting
	GuardIdleTimeouts   atomic.Int64 // connections closed for idleness
	GuardLogonTimeouts  atomic.Int64 // connections closed for missing logon
	TLSHandshakeFails   atomic.Int64 // TLS handshakes rejected at the edge (phase 7)
	// StochasticOutcomes counts phase-2.1 stochastic outcome draws by
	// label: "fill", "reject", or "partial".
	StochasticOutcomes StochasticOutcomes
}

// StochasticOutcomes is a labeled counter for stochastic simulation
// outcome draws (phase 2.1, spec §48). The label set is fixed:
// "fill", "reject", "partial". Unknown labels are ignored so a typo
// can never grow the series.
type StochasticOutcomes struct {
	fill    atomic.Int64
	reject  atomic.Int64
	partial atomic.Int64
}

// Add increments the counter for one outcome label.
func (s *StochasticOutcomes) Add(outcome string) {
	switch outcome {
	case "fill":
		s.fill.Add(1)
	case "reject":
		s.reject.Add(1)
	case "partial":
		s.partial.Add(1)
	}
}

// Snapshot returns the label → count view.
func (s *StochasticOutcomes) Snapshot() map[string]int64 {
	return map[string]int64{
		"fill":    s.fill.Load(),
		"reject":  s.reject.Load(),
		"partial": s.partial.Load(),
	}
}

// Snapshot is a point-in-time, JSON-serializable view of Metrics.
type Snapshot struct {
	ActiveSessions      int64 `json:"active_sessions"`
	AcceptedConnections int64 `json:"accepted_connections"`
	RejectedConnections int64 `json:"rejected_connections"`
	FIXMessagesIn       int64 `json:"fix_messages_in"`
	FIXMessagesOut      int64 `json:"fix_messages_out"`
	ApplicationMessages int64 `json:"application_messages"`
	SequenceErrors      int64 `json:"sequence_errors"`
	AuthenticationFails int64 `json:"authentication_failures"`
	SessionExpirations  int64 `json:"session_expirations"`
	RuleMatches         int64 `json:"rule_matches"`
	ExecutionReports    int64 `json:"execution_reports"`
	GuardDroppedFrames  int64 `json:"guard_dropped_frames"`
	GuardRateLimited    int64 `json:"guard_rate_limited"`
	GuardIdleTimeouts   int64 `json:"guard_idle_timeouts"`
	GuardLogonTimeouts  int64 `json:"guard_logon_timeouts"`
	TLSHandshakeFails   int64 `json:"tls_handshake_failures"`
	// stochastic_outcomes is the labeled phase-2.1 counter:
	// {"fill": n, "reject": n, "partial": n}.
	StochasticOutcomes map[string]int64 `json:"stochastic_outcomes"`
}

// Snapshot copies the current counter values.
func (m *Metrics) Snapshot() Snapshot {
	return Snapshot{
		ActiveSessions:      m.ActiveSessions.Load(),
		AcceptedConnections: m.AcceptedConnections.Load(),
		RejectedConnections: m.RejectedConnections.Load(),
		FIXMessagesIn:       m.FIXMessagesIn.Load(),
		FIXMessagesOut:      m.FIXMessagesOut.Load(),
		ApplicationMessages: m.ApplicationMessages.Load(),
		SequenceErrors:      m.SequenceErrors.Load(),
		AuthenticationFails: m.AuthenticationFails.Load(),
		SessionExpirations:  m.SessionExpirations.Load(),
		RuleMatches:         m.RuleMatches.Load(),
		ExecutionReports:    m.ExecutionReports.Load(),
		GuardDroppedFrames:  m.GuardDroppedFrames.Load(),
		GuardRateLimited:    m.GuardRateLimited.Load(),
		GuardIdleTimeouts:   m.GuardIdleTimeouts.Load(),
		GuardLogonTimeouts:  m.GuardLogonTimeouts.Load(),
		TLSHandshakeFails:   m.TLSHandshakeFails.Load(),
		StochasticOutcomes:  m.StochasticOutcomes.Snapshot(),
	}
}

var (
	defaultMetrics     *Metrics
	defaultMetricsOnce sync.Once
)

// DefaultMetrics returns the process-wide metrics registry.
func DefaultMetrics() *Metrics {
	defaultMetricsOnce.Do(func() { defaultMetrics = &Metrics{} })
	return defaultMetrics
}
