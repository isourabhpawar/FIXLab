package simulator

import (
	"testing"
	"time"

	"fixlab.dev/fixlab/backend/internal/orders"
)

func testOrder() *orders.Order {
	return &orders.Order{
		ClOrdID: "T1", Symbol: "MSFT", Side: "1",
		OrderQty: 1000, Price: 310.50, OrdType: "2", LeavesQty: 1000,
	}
}

func TestValidateStochasticConfig(t *testing.T) {
	valid := StochasticConfig{Enabled: true, AcceptPct: 50, RejectPct: 30, PartialFillPct: 20,
		AvgLatencyMs: 45, StdDevMs: 10, MaxDelayMs: 5000, Seed: 7}
	if _, err := ValidateStochasticConfig(valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*StochasticConfig)
	}{
		{"sum 99", func(c *StochasticConfig) { c.PartialFillPct = 19 }},
		{"sum 101", func(c *StochasticConfig) { c.PartialFillPct = 21 }},
		{"accept over 100", func(c *StochasticConfig) { c.AcceptPct = 101; c.RejectPct = 0; c.PartialFillPct = -1 }},
		{"negative pct", func(c *StochasticConfig) { c.RejectPct = -1; c.PartialFillPct = 21 }},
		{"negative avg latency", func(c *StochasticConfig) { c.AvgLatencyMs = -1 }},
		{"negative stddev", func(c *StochasticConfig) { c.StdDevMs = -1 }},
		{"negative maxDelay", func(c *StochasticConfig) { c.MaxDelayMs = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid
			tc.mut(&c)
			if _, err := ValidateStochasticConfig(c); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}

	// Zero MaxDelayMs normalizes to the default cap.
	c := valid
	c.MaxDelayMs = 0
	vc, err := ValidateStochasticConfig(c)
	if err != nil {
		t.Fatalf("zero maxDelay rejected: %v", err)
	}
	if vc.MaxDelayMs != DefaultStochasticMaxDelayMs {
		t.Fatalf("maxDelay not defaulted: got %d", vc.MaxDelayMs)
	}
}

func TestStochasticSeededDeterminism(t *testing.T) {
	cfg := StochasticConfig{Enabled: true, AcceptPct: 50, RejectPct: 30, PartialFillPct: 20,
		AvgLatencyMs: 45, StdDevMs: 10, MaxDelayMs: 5000, Seed: 12345}
	newPolicy := func() *StochasticPolicy {
		p := newStochasticPolicy(nil, nil, nil)
		if _, err := p.SetConfig(cfg); err != nil {
			t.Fatalf("SetConfig: %v", err)
		}
		return p
	}
	p1, p2 := newPolicy(), newPolicy()
	for i := 0; i < 50; i++ {
		o1, o2 := testOrder(), testOrder()
		p1.mu.Lock()
		oc1, q1, px1, d1 := p1.drawLocked(o1)
		p1.mu.Unlock()
		p2.mu.Lock()
		oc2, q2, px2, d2 := p2.drawLocked(o2)
		p2.mu.Unlock()
		if oc1 != oc2 || q1 != q2 || px1 != px2 || d1 != d2 {
			t.Fatalf("draw %d differs: (%s,%.4g,%.4g,%v) vs (%s,%.4g,%.4g,%v)",
				i, oc1, q1, px1, d1, oc2, q2, px2, d2)
		}
	}
}

func TestStochasticOutcomeMix(t *testing.T) {
	// Find the e2e seed: 50/30/20 must produce all three outcomes in 20 draws.
	const wantSeed = 7
	p := newStochasticPolicy(nil, nil, nil)
	cfg := StochasticConfig{Enabled: true, AcceptPct: 50, RejectPct: 30, PartialFillPct: 20,
		AvgLatencyMs: 0, StdDevMs: 0, Seed: wantSeed}
	if _, err := p.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	seen := map[string]bool{}
	p.mu.Lock()
	for i := 0; i < 20; i++ {
		oc, _, _, _ := p.drawLocked(testOrder())
		seen[oc] = true
	}
	p.mu.Unlock()
	for _, oc := range []string{stochOutcomeFill, stochOutcomeReject, stochOutcomePartial} {
		if !seen[oc] {
			t.Fatalf("seed %d missing outcome %q in 20 draws: %v", wantSeed, oc, seen)
		}
	}
	t.Logf("seed %d outcomes: %v", wantSeed, seen)
}

func TestStochasticLatencyClamp(t *testing.T) {
	p := newStochasticPolicy(nil, nil, nil)

	// Deterministic latency: stddev 0 pins the sample at the mean.
	cfg := StochasticConfig{Enabled: true, AcceptPct: 100, AvgLatencyMs: 300, StdDevMs: 0, MaxDelayMs: 5000}
	if _, err := p.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	p.mu.Lock()
	_, _, _, d := p.drawLocked(testOrder())
	p.mu.Unlock()
	if d != 300*time.Millisecond {
		t.Fatalf("expected exactly 300ms, got %v", d)
	}

	// Huge mean clamps to maxDelayMs.
	cfg.AvgLatencyMs = 1000000
	if _, err := p.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	p.mu.Lock()
	_, _, _, d = p.drawLocked(testOrder())
	p.mu.Unlock()
	if d != 5000*time.Millisecond {
		t.Fatalf("expected clamp to 5000ms, got %v", d)
	}

	// A 100% accept policy only ever draws fills.
	p.mu.Lock()
	for i := 0; i < 20; i++ {
		oc, _, _, _ := p.drawLocked(testOrder())
		if oc != stochOutcomeFill {
			t.Fatalf("100%% accept drew %q", oc)
		}
	}
	p.mu.Unlock()
}

func TestStochasticPartialFillBounds(t *testing.T) {
	p := newStochasticPolicy(nil, nil, nil)
	cfg := StochasticConfig{Enabled: true, PartialFillPct: 100, MaxDelayMs: 5000}
	if _, err := p.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < 50; i++ {
		o := testOrder()
		oc, qty, px, _ := p.drawLocked(o)
		if oc != stochOutcomePartial {
			t.Fatalf("100%% partial drew %q", oc)
		}
		if qty < 0.1*o.OrderQty || qty > 0.9*o.OrderQty {
			t.Fatalf("partial qty %.4g outside [10%%, 90%%] of %.4g", qty, o.OrderQty)
		}
		if px != o.Price {
			t.Fatalf("partial price %.4g != order price %.4g", px, o.Price)
		}
	}
}

func TestStochasticSetConfigResetsRNG(t *testing.T) {
	p := newStochasticPolicy(nil, nil, nil)
	cfg := StochasticConfig{Enabled: true, AcceptPct: 50, RejectPct: 30, PartialFillPct: 20, Seed: 99}
	if _, err := p.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	p.mu.Lock()
	oc1, _, _, _ := p.drawLocked(testOrder())
	p.mu.Unlock()
	// Re-setting the same config (same seed) restarts the stream.
	if _, err := p.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	p.mu.Lock()
	oc2, _, _, _ := p.drawLocked(testOrder())
	p.mu.Unlock()
	if oc1 != oc2 {
		t.Fatalf("RNG stream not reset by SetConfig: %q vs %q", oc1, oc2)
	}
}
