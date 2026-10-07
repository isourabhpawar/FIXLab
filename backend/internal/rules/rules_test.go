package rules

import (
	"testing"
)

func add(t *testing.T, e Engine, r Rule) Rule {
	t.Helper()
	got, err := e.AddRule(r)
	if err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	if got.ID == "" {
		t.Fatal("AddRule: empty ID")
	}
	return got
}

func TestPredicateOps(t *testing.T) {
	fields := map[int]string{55: "MSFT", 38: "1000", 54: "1", 44: "310.50", 11: "ORD1"}

	cases := []struct {
		p    Predicate
		want bool
	}{
		{Predicate{55, OpEquals, "MSFT"}, true},
		{Predicate{55, OpEquals, "msft"}, false},
		{Predicate{55, OpNotEquals, "AAPL"}, true},
		{Predicate{55, OpNotEquals, "MSFT"}, false},
		{Predicate{55, OpContains, "MS"}, true},
		{Predicate{55, OpContains, "ZZ"}, false},
		{Predicate{38, OpGreater, "999"}, true},
		{Predicate{38, OpGreater, "1000"}, false},
		{Predicate{38, OpLess, "10000"}, true},
		{Predicate{38, OpEquals, "1000"}, true},
		{Predicate{38, OpEquals, "1000.0"}, true}, // numeric eq
		{Predicate{38, OpNotEquals, "1000.00"}, false},
		{Predicate{44, OpLess, "400"}, true},
		{Predicate{54, OpEquals, "1"}, true},
		{Predicate{11, OpContains, "ORD"}, true},
		// Absent field semantics.
		{Predicate{1, OpEquals, "X"}, false},
		{Predicate{1, OpNotEquals, "X"}, true},
		{Predicate{1, OpContains, "X"}, false},
		{Predicate{1, OpGreater, "0"}, false},
	}
	for _, c := range cases {
		if got := matchPredicate(c.p, fields); got != c.want {
			t.Errorf("matchPredicate(%+v) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestPriorityOrderFirstMatchWins(t *testing.T) {
	e := NewEngine()
	// R1: lower priority number fires first even though added second.
	add(t, e, Rule{Name: "fill-msft", Priority: 10, Enabled: true,
		Predicates: []Predicate{{55, OpEquals, "MSFT"}},
		Actions:    []ActionStep{{Type: ActionFullFill}}})
	r2 := add(t, e, Rule{Name: "reject-big", Priority: 5, Enabled: true,
		Predicates: []Predicate{{38, OpGreater, "10000"}},
		Actions:    []ActionStep{{Type: ActionReject, Text: "too big"}}})

	// MSFT qty 20000 matches both: R2 (priority 5) must win.
	got := e.Evaluate("D", map[int]string{55: "MSFT", 38: "20000"})
	if got == nil || got.ID != r2.ID {
		t.Fatalf("Evaluate: got %+v, want reject-big", got)
	}
	if got.MatchedCount != 1 {
		t.Fatalf("MatchedCount = %d, want 1", got.MatchedCount)
	}
	// A second evaluation increments again.
	e.Evaluate("D", map[int]string{55: "MSFT", 38: "20000"})
	if g, _ := e.GetRule(r2.ID); g.MatchedCount != 2 {
		t.Fatalf("MatchedCount = %d, want 2", g.MatchedCount)
	}

	// MSFT qty 100 matches only R1.
	got = e.Evaluate("D", map[int]string{55: "MSFT", 38: "100"})
	if got == nil || got.Name != "fill-msft" {
		t.Fatalf("Evaluate: got %+v, want fill-msft", got)
	}

	// Nothing matches.
	if got := e.Evaluate("D", map[int]string{55: "AAPL", 38: "100"}); got != nil {
		t.Fatalf("Evaluate: got %+v, want nil", got)
	}
	// Non-D messages never fire rules.
	if got := e.Evaluate("F", map[int]string{55: "MSFT"}); got != nil {
		t.Fatalf("Evaluate(F): got %+v, want nil", got)
	}
}

func TestPriorityTieCreationOrder(t *testing.T) {
	e := NewEngine()
	first := add(t, e, Rule{Name: "first", Priority: 1, Enabled: true,
		Predicates: []Predicate{},
		Actions:    []ActionStep{{Type: ActionAckNew}}})
	add(t, e, Rule{Name: "second", Priority: 1, Enabled: true,
		Predicates: []Predicate{},
		Actions:    []ActionStep{{Type: ActionAckNew}}})
	got := e.Evaluate("D", map[int]string{})
	if got == nil || got.ID != first.ID {
		t.Fatalf("tie: got %+v, want first-created rule", got)
	}
}

func TestDisabledRuleSkipped(t *testing.T) {
	e := NewEngine()
	add(t, e, Rule{Name: "off", Priority: 1, Enabled: false,
		Predicates: []Predicate{},
		Actions:    []ActionStep{{Type: ActionAckNew}}})
	if got := e.Evaluate("D", map[int]string{}); got != nil {
		t.Fatalf("disabled rule fired: %+v", got)
	}
}

func TestZeroPredicatesMatchAll(t *testing.T) {
	e := NewEngine()
	r := add(t, e, Rule{Name: "catch-all", Priority: 1, Enabled: true,
		Actions: []ActionStep{{Type: ActionAckNew}}})
	if got := e.Evaluate("D", map[int]string{55: "ANYTHING"}); got == nil || got.ID != r.ID {
		t.Fatalf("zero-predicate rule did not match: %+v", got)
	}
}

func TestValidation(t *testing.T) {
	e := NewEngine()
	valid := Rule{Name: "ok", Priority: 1, Enabled: true,
		Predicates: []Predicate{{55, OpEquals, "MSFT"}},
		Actions:    []ActionStep{{Type: ActionFullFill}}}

	bad := []Rule{
		{Name: "no-actions", Priority: 1},
		{Name: "bad-tag", Priority: 1,
			Predicates: []Predicate{{999, OpEquals, "x"}},
			Actions:    []ActionStep{{Type: ActionAckNew}}},
		{Name: "bad-op", Priority: 1,
			Predicates: []Predicate{{55, "bogus", "x"}},
			Actions:    []ActionStep{{Type: ActionAckNew}}},
		{Name: "gt-on-string", Priority: 1,
			Predicates: []Predicate{{55, OpGreater, "x"}},
			Actions:    []ActionStep{{Type: ActionAckNew}}},
		{Name: "gt-nonnumeric", Priority: 1,
			Predicates: []Predicate{{38, OpGreater, "abc"}},
			Actions:    []ActionStep{{Type: ActionAckNew}}},
		{Name: "bad-action", Priority: 1,
			Actions: []ActionStep{{Type: "EXPLODE"}}},
		{Name: "partial-no-qty", Priority: 1,
			Actions: []ActionStep{{Type: ActionPartialFill, Price: 1}}},
		{Name: "reject-empty", Priority: 1,
			Actions: []ActionStep{{Type: ActionReject}}},
		{Name: "delay-huge", Priority: 1,
			Actions: []ActionStep{{Type: ActionDelay, DelayMs: MaxDelayMs + 1}}},
	}
	for _, r := range bad {
		if _, err := e.AddRule(r); !IsValidationError(err) {
			t.Errorf("AddRule(%s): err = %v, want ValidationError", r.Name, err)
		}
	}
	if _, err := e.AddRule(valid); err != nil {
		t.Fatalf("AddRule(valid): %v", err)
	}
}

func TestCRUD(t *testing.T) {
	e := NewEngine()
	r := add(t, e, Rule{Name: "v1", Priority: 10, Enabled: true,
		Predicates: []Predicate{{55, OpEquals, "MSFT"}},
		Actions:    []ActionStep{{Type: ActionFullFill}}})

	if _, ok := e.GetRule("nope"); ok {
		t.Fatal("GetRule(nope) found")
	}
	if err := e.RemoveRule("nope"); err != ErrRuleNotFound {
		t.Fatalf("RemoveRule(nope) = %v, want ErrRuleNotFound", err)
	}

	upd, err := e.UpdateRule(r.ID, Rule{Name: "v2", Priority: 3, Enabled: true,
		Predicates: []Predicate{{55, OpEquals, "AAPL"}},
		Actions:    []ActionStep{{Type: ActionReject, Text: "no"}}})
	if err != nil {
		t.Fatalf("UpdateRule: %v", err)
	}
	if upd.Name != "v2" || upd.Priority != 3 || upd.ID != r.ID {
		t.Fatalf("UpdateRule result: %+v", upd)
	}
	if _, err := e.UpdateRule("nope", upd); err != ErrRuleNotFound {
		t.Fatalf("UpdateRule(nope) = %v", err)
	}
	if _, err := e.UpdateRule(r.ID, Rule{Name: "bad"}); !IsValidationError(err) {
		t.Fatalf("UpdateRule(bad) = %v, want ValidationError", err)
	}

	if err := e.RemoveRule(r.ID); err != nil {
		t.Fatalf("RemoveRule: %v", err)
	}
	if err := e.RemoveRule(r.ID); err != ErrRuleNotFound {
		t.Fatalf("re-delete = %v, want ErrRuleNotFound", err)
	}
	if len(e.ListRules()) != 0 {
		t.Fatal("ListRules not empty after delete")
	}
}

func TestKillSwitch(t *testing.T) {
	e := NewEngine()
	if e.KillSwitch() {
		t.Fatal("kill switch on by default")
	}
	e.SetKillSwitch(true)
	if !e.KillSwitch() {
		t.Fatal("kill switch not set")
	}
	e.SetKillSwitch(false)
	if e.KillSwitch() {
		t.Fatal("kill switch not cleared")
	}
}
