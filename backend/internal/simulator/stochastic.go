// Package simulator — stochastic simulation policy (phase 2.1, spec §48).
//
// The stochastic simulator draws a random outcome for every inbound
// NewOrderSingle (35=D) and executes it after a random latency sampled
// from a normal distribution. It is deliberately the LAST automatic
// behavior in the evaluation order:
//
//	kill switch → deterministic rules (first match fires) → stochastic
//	→ phase-3 immediate New acknowledgement.
//
// The stochastic policy never overrides the kill switch or a
// deterministic rule: it only fires when enabled AND no rule matched.
//
// Determinism: each session owns one *rand.Rand. When the config's Seed
// is nonzero the RNG is seeded with it, and every order performs its
// draws synchronously on the FIX message pump (outcome roll, then the
// latency sample, then — for partial fills — the fill-size roll) before
// the delayed execution is handed to a goroutine. The same seed and the
// same arrival order therefore reproduce the identical outcome
// sequence, which is what makes the policy testable.
package simulator

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	mrand "math/rand"
	"sync"
	"sync/atomic"
	"time"

	"fixlab.dev/fixlab/backend/internal/common"
	"fixlab.dev/fixlab/backend/internal/orders"
)

// Stochastic outcome kinds.
const (
	stochOutcomeFill    = "fill"
	stochOutcomeReject  = "reject"
	stochOutcomePartial = "partial"
)

// DefaultStochasticMaxDelayMs bounds one stochastic delay: a goroutine
// sleeps for the sampled latency, so an unbounded tail would pin
// goroutines (and the order's lifecycle) far too long.
const DefaultStochasticMaxDelayMs = 5000

// StochasticRejectReason is the 103= value of a stochastic rejection
// ("Other"); the 58= text names the simulator so the developer's engine
// can distinguish it from a rule or manual reject.
const StochasticRejectReason = "99"

// StochasticRejectText is the 58= text of a stochastic rejection.
const StochasticRejectText = "stochastic simulator: rejected by outcome roll"

// StochasticConfig is the per-session stochastic simulation policy
// (spec §48). Percentages must sum to 100. Seed == 0 seeds the RNG from
// crypto/rand; any nonzero seed makes the outcome sequence reproducible.
type StochasticConfig struct {
	Enabled        bool    `json:"enabled"`
	AcceptPct      float64 `json:"acceptPct"`
	RejectPct      float64 `json:"rejectPct"`
	PartialFillPct float64 `json:"partialFillPct"`
	AvgLatencyMs   float64 `json:"avgLatencyMs"`
	StdDevMs       float64 `json:"stdDevMs"`
	MaxDelayMs     int64   `json:"maxDelayMs"`
	Seed           int64   `json:"seed"`
}

// DefaultStochasticConfig is the disabled policy returned before the
// browser configures one.
func DefaultStochasticConfig() StochasticConfig {
	return StochasticConfig{
		Enabled:        false,
		AcceptPct:      100,
		RejectPct:      0,
		PartialFillPct: 0,
		AvgLatencyMs:   0,
		StdDevMs:       0,
		MaxDelayMs:     DefaultStochasticMaxDelayMs,
		Seed:           0,
	}
}

// ValidationError marks a malformed stochastic config: the API maps it
// to HTTP 400.
type StochasticValidationError struct{ msg string }

func (e *StochasticValidationError) Error() string { return e.msg }

func stochasticValidationErr(format string, args ...any) *StochasticValidationError {
	return &StochasticValidationError{msg: "stochastic: " + fmt.Sprintf(format, args...)}
}

// ValidateStochasticConfig enforces the V1 stochastic DSL. A zero
// MaxDelayMs is normalized to the default cap.
func ValidateStochasticConfig(c StochasticConfig) (StochasticConfig, error) {
	for name, v := range map[string]float64{
		"acceptPct": c.AcceptPct, "rejectPct": c.RejectPct, "partialFillPct": c.PartialFillPct,
	} {
		if v < 0 || v > 100 {
			return c, stochasticValidationErr("%s must be in [0, 100], got %.4g", name, v)
		}
	}
	if sum := c.AcceptPct + c.RejectPct + c.PartialFillPct; math.Abs(sum-100) > 1e-9 {
		return c, stochasticValidationErr("percentages must sum to 100, got %.4g (accept %.4g + reject %.4g + partial %.4g)",
			sum, c.AcceptPct, c.RejectPct, c.PartialFillPct)
	}
	if c.AvgLatencyMs < 0 {
		return c, stochasticValidationErr("avgLatencyMs must be >= 0, got %.4g", c.AvgLatencyMs)
	}
	if c.StdDevMs < 0 {
		return c, stochasticValidationErr("stdDevMs must be >= 0, got %.4g", c.StdDevMs)
	}
	if c.MaxDelayMs < 0 {
		return c, stochasticValidationErr("maxDelayMs must be >= 0, got %d", c.MaxDelayMs)
	}
	if c.MaxDelayMs == 0 {
		c.MaxDelayMs = DefaultStochasticMaxDelayMs
	}
	return c, nil
}

// StochasticState is the API-visible policy: the config plus the
// outcomes drawn so far this session (for the browser's readout).
type StochasticState struct {
	Config   StochasticConfig `json:"config"`
	Outcomes map[string]int64 `json:"outcomes"`
}

// StochasticPolicy owns one session's stochastic simulation: its config
// and its RNG. It is safe for concurrent use; the RNG is used only on
// the FIX message pump while config writes come from HTTP handlers.
type StochasticPolicy struct {
	mu       sync.Mutex
	cfg      StochasticConfig
	rng      *mrand.Rand
	fills    atomic.Int64
	rejects  atomic.Int64
	partials atomic.Int64

	sim     *simulator
	log     *slog.Logger
	metrics *common.Metrics
}

// newStochasticPolicy builds the policy bound to its simulator. The
// back-reference is safe (same package, no import cycle).
func newStochasticPolicy(sim *simulator, log *slog.Logger, metrics *common.Metrics) *StochasticPolicy {
	return &StochasticPolicy{
		cfg:     DefaultStochasticConfig(),
		rng:     newStochasticRNG(0),
		sim:     sim,
		log:     log,
		metrics: metrics,
	}
}

func (p *StochasticPolicy) logger() *slog.Logger {
	if p.log != nil {
		return p.log
	}
	return slog.Default()
}

// newStochasticRNG returns a session RNG: the fixed seed when nonzero
// (reproducible tests), otherwise crypto/rand (with a time fallback so
// a broken CSPRNG can never wedge session creation).
func newStochasticRNG(seed int64) *mrand.Rand {
	if seed != 0 {
		return mrand.New(mrand.NewSource(seed))
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return mrand.New(mrand.NewSource(int64(binary.LittleEndian.Uint64(b[:]))))
	}
	return mrand.New(mrand.NewSource(time.Now().UnixNano()))
}

// SetConfig validates and stores the policy. Replacing the config
// (including the seed) resets the RNG stream: draws after the change
// are reproducible from the new seed.
func (p *StochasticPolicy) SetConfig(c StochasticConfig) (StochasticConfig, error) {
	vc, err := ValidateStochasticConfig(c)
	if err != nil {
		return StochasticConfig{}, err
	}
	p.mu.Lock()
	p.cfg = vc
	p.rng = newStochasticRNG(vc.Seed)
	p.mu.Unlock()
	p.logger().Info("stochastic: config updated",
		"enabled", vc.Enabled, "accept", vc.AcceptPct, "reject", vc.RejectPct,
		"partial", vc.PartialFillPct, "avg_latency_ms", vc.AvgLatencyMs,
		"stddev_ms", vc.StdDevMs, "max_delay_ms", vc.MaxDelayMs, "seed", vc.Seed)
	return vc, nil
}

// Enabled reports whether the policy fires on inbound orders.
func (p *StochasticPolicy) Enabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.Enabled
}

// State returns the config plus per-session outcome counts.
func (p *StochasticPolicy) State() StochasticState {
	p.mu.Lock()
	cfg := p.cfg
	p.mu.Unlock()
	return StochasticState{
		Config: cfg,
		Outcomes: map[string]int64{
			stochOutcomeFill:    p.fills.Load(),
			stochOutcomeReject:  p.rejects.Load(),
			stochOutcomePartial: p.partials.Load(),
		},
	}
}

// draw is the synchronous part of Roll: one outcome roll, the latency
// sample, and — for partial fills — the fill-size roll. It runs on the
// FIX message pump so the RNG call order is deterministic for a fixed
// seed. Callers must hold p.mu.
func (p *StochasticPolicy) drawLocked(o *orders.Order) (outcome string, lastQty, lastPx float64, delay time.Duration) {
	cfg := p.cfg
	r := p.rng.Float64() * 100
	switch {
	case r < cfg.AcceptPct:
		outcome = stochOutcomeFill
	case r < cfg.AcceptPct+cfg.RejectPct:
		outcome = stochOutcomeReject
	default:
		outcome = stochOutcomePartial
	}
	// Latency: Normal(avg, stddev), clamped to [0, maxDelayMs].
	lat := p.rng.NormFloat64()*cfg.StdDevMs + cfg.AvgLatencyMs
	if lat < 0 {
		lat = 0
	}
	if max := float64(cfg.MaxDelayMs); lat > max {
		lat = max
	}
	delay = time.Duration(lat * float64(time.Millisecond))
	if outcome == stochOutcomePartial {
		// Random fill size in [10%, 90%] of the order quantity; the
		// remainder stays working for manual execution.
		lastQty = o.OrderQty * (0.1 + 0.8*p.rng.Float64())
		lastPx = o.Price
	} else {
		lastQty = o.LeavesQty
		lastPx = o.Price
	}
	return outcome, lastQty, lastPx, delay
}

// Roll draws a stochastic outcome for a freshly created order and
// schedules its execution after the sampled latency. It is called from
// onNewOrder only when the kill switch is off, no deterministic rule
// fired, and the policy is enabled. The draws happen synchronously
// (deterministic RNG order); only the sleep + execution run on a
// goroutine, so the FIX pump is never blocked.
func (p *StochasticPolicy) Roll(o *orders.Order) {
	p.mu.Lock()
	outcome, lastQty, lastPx, delay := p.drawLocked(o)
	p.mu.Unlock()

	switch outcome {
	case stochOutcomeFill:
		p.fills.Add(1)
	case stochOutcomeReject:
		p.rejects.Add(1)
	case stochOutcomePartial:
		p.partials.Add(1)
	}
	if p.metrics != nil {
		p.metrics.StochasticOutcomes.Add(outcome)
	}
	p.logger().Info("stochastic: outcome drawn",
		"cl_ord_id", o.ClOrdID, "outcome", outcome,
		"delay_ms", delay.Milliseconds())
	go p.executeAfterDelay(o.ClOrdID, outcome, lastQty, lastPx, delay)
}

// executeAfterDelay sleeps for the sampled latency, then executes the
// drawn outcome through the same Fill/Reject/PartialFill path as manual
// and rule-fired executions — so CumQty/LeavesQty/AvgPx bookkeeping,
// the blotter, WS events, quota consumption, and ExecID sequencing are
// identical. If a manual browser execution raced the delay and the order
// is no longer working, the execution fails with a StateError and the
// outcome is dropped with a warning (the same pattern as phase-4 rule
// chains): the order is left consistent, never double-executed.
func (p *StochasticPolicy) executeAfterDelay(clOrdID, outcome string, lastQty, lastPx float64, delay time.Duration) {
	if delay > 0 {
		time.Sleep(delay)
	}
	var err error
	switch outcome {
	case stochOutcomeFill:
		if lastPx <= 0 {
			p.logger().Warn("stochastic: fill dropped — order has no price (market order); left for manual execution",
				"cl_ord_id", clOrdID)
			return
		}
		_, err = p.sim.Fill(clOrdID, lastQty, lastPx)
	case stochOutcomeReject:
		_, err = p.sim.Reject(clOrdID, StochasticRejectReason, StochasticRejectText)
	case stochOutcomePartial:
		if lastPx <= 0 {
			p.logger().Warn("stochastic: partial fill dropped — order has no price (market order); left for manual execution",
				"cl_ord_id", clOrdID)
			return
		}
		_, err = p.sim.PartialFill(clOrdID, lastQty, lastPx)
	default:
		p.logger().Warn("stochastic: unknown outcome", "cl_ord_id", clOrdID, "outcome", outcome)
		return
	}
	if err != nil {
		p.logger().Warn("stochastic: outcome stopped",
			"cl_ord_id", clOrdID, "outcome", outcome, "err", err)
		return
	}
	p.logger().Info("stochastic: outcome executed",
		"cl_ord_id", clOrdID, "outcome", outcome)
}
