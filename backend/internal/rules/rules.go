// Package rules implements the deterministic rule engine and the kill
// switch (spec §31, §32, §33; phase 4).
//
// Determinism contract (the whole point of V1):
//   - Rules are evaluated in priority order (lowest priority value
//     first); rules sharing a priority are evaluated in creation order.
//   - ALL predicates of a rule must match (AND semantics); a rule with
//     zero predicates matches every NewOrderSingle.
//   - The FIRST matching rule fires; later rules are not evaluated.
//   - The kill switch is evaluated before every rule: while it is on,
//     every inbound NewOrderSingle is rejected and no rule fires.
//   - Action chains run in the order written; DELAY sleeps, then the
//     chain continues.
package rules

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PredicateOp is a rule predicate operator (spec §32). The canonical
// wire form is lowercase ("eq"); the legacy uppercase names ("EQUALS")
// are accepted as aliases and normalized on validation.
type PredicateOp string

const (
	OpEquals    PredicateOp = "eq"
	OpNotEquals PredicateOp = "ne"
	OpGreater   PredicateOp = "gt"
	OpLess      PredicateOp = "lt"
	OpContains  PredicateOp = "contains"
)

// Action is a rule action type (spec §33).
type Action string

const (
	ActionAckNew      Action = "ACK_NEW"
	ActionFullFill    Action = "FULL_FILL"
	ActionPartialFill Action = "PARTIAL_FILL"
	ActionReject      Action = "REJECT"
	ActionDelay       Action = "DELAY"
)

// Supported predicate tags (spec §32). Tags 38/40/44 compare
// numerically; the rest compare as strings.
var PredicateTags = map[int]string{
	11: "ClOrdID",
	1:  "Account",
	54: "Side",
	55: "Symbol",
	38: "OrderQty",
	40: "OrdType",
	44: "Price",
}

// numericTags are the tags whose values compare numerically.
var numericTags = map[int]bool{38: true, 40: true, 44: true}

// MaxDelayMs caps a single DELAY action: a rule that sleeps for hours is
// almost certainly a typo, and a stuck chain would pin a goroutine and
// the order's lifecycle.
const MaxDelayMs = 60000

// Predicate matches one FIX field, e.g. tag 55 == "MSFT".
type Predicate struct {
	Tag   int         `json:"tag"`
	Op    PredicateOp `json:"op"`
	Value string      `json:"value"`
}

// ActionStep is one step of a rule's action chain with its parameters:
//   - DELAY:       delayMs (0..MaxDelayMs); sleeps, then continues.
//   - ACK_NEW:     no params; sends the New acknowledgement (150=0/39=0).
//   - FULL_FILL:   price optional (fills at the order's own price when
//     absent; required for market orders which have no 44).
//   - PARTIAL_FILL: qty (LastQty/32) and price (LastPx/31), both > 0.
//   - REJECT:      ordRejReason (103) and/or text (58); one is required.
type ActionStep struct {
	Type         Action  `json:"type"`
	DelayMs      int     `json:"delayMs,omitempty"`
	Qty          float64 `json:"qty,omitempty"`
	Price        float64 `json:"price,omitempty"`
	OrdRejReason string  `json:"ordRejReason,omitempty"`
	Text         string  `json:"text,omitempty"`
}

// Rule is one deterministic simulation rule.
type Rule struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Priority   int          `json:"priority"`
	Enabled    bool         `json:"enabled"`
	Predicates []Predicate  `json:"predicates"`
	Actions    []ActionStep `json:"actions"`
	// MatchedCount is how many inbound orders this rule has fired on.
	MatchedCount int64     `json:"matchedCount"`
	CreatedAt    time.Time `json:"createdAt"`
	seq          int64     // creation order; breaks priority ties
}

// Errors returned by the engine.
var (
	ErrRuleNotFound = errors.New("rules: rule not found")
)

// ValidationError marks a malformed rule: the API maps it to HTTP 400.
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func validationErr(format string, args ...any) *ValidationError {
	return &ValidationError{msg: "rules: " + fmt.Sprintf(format, args...)}
}

// IsValidationError reports whether err is a rule validation failure.
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}

// Engine matches inbound NewOrderSingle messages against rules and
// exposes the kill switch (spec §31, §32). Implementations are safe for
// concurrent use: the FIX message pump evaluates while HTTP handlers
// mutate.
type Engine interface {
	// AddRule validates and stores a rule, assigning it a fresh ID.
	AddRule(r Rule) (Rule, error)
	// UpdateRule replaces the rule's editable fields (name, priority,
	// enabled, predicates, actions); ID, creation order and match count
	// are preserved.
	UpdateRule(id string, r Rule) (Rule, error)
	// RemoveRule deletes a rule; ErrRuleNotFound when unknown.
	RemoveRule(id string) error
	// GetRule fetches one rule by ID.
	GetRule(id string) (Rule, bool)
	// ListRules returns all rules in evaluation order (priority, then
	// creation order).
	ListRules() []Rule
	// Evaluate returns the first matching rule in evaluation order for
	// an inbound NewOrderSingle (msgType "D"), recording the match
	// (MatchedCount++). It returns nil when nothing matches — including
	// for non-D message types, which rules never fire on.
	Evaluate(msgType string, fields map[int]string) *Rule
	// SetKillSwitch enables/disables the venue halt: while on, every
	// inbound NewOrderSingle is rejected immediately and no rule is
	// evaluated.
	SetKillSwitch(on bool)
	// KillSwitch reports the halt state.
	KillSwitch() bool
}

// NewEngine returns an empty rule engine.
func NewEngine() Engine {
	return &engine{byID: map[string]*ruleState{}}
}

type ruleState struct {
	rule    Rule
	matched atomic.Int64
}

type engine struct {
	mu         sync.RWMutex
	byID       map[string]*ruleState
	seq        atomic.Int64
	killSwitch atomic.Bool
}

// --- mutation --------------------------------------------------------

func newRuleID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "R" + hex.EncodeToString(b[:]), nil
}

func (e *engine) AddRule(r Rule) (Rule, error) {
	nr, err := normalize(r)
	if err != nil {
		return Rule{}, err
	}
	if err := validate(nr); err != nil {
		return Rule{}, err
	}
	id, err := newRuleID()
	if err != nil {
		return Rule{}, fmt.Errorf("rules: generate rule ID: %w", err)
	}
	nr.ID = id
	nr.CreatedAt = time.Now()
	nr.seq = e.seq.Add(1)
	e.mu.Lock()
	e.byID[id] = &ruleState{rule: nr}
	e.mu.Unlock()
	return snapshot(e.byID[id]), nil
}

func (e *engine) UpdateRule(id string, r Rule) (Rule, error) {
	nr, err := normalize(r)
	if err != nil {
		return Rule{}, err
	}
	if err := validate(nr); err != nil {
		return Rule{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.byID[id]
	if !ok {
		return Rule{}, ErrRuleNotFound
	}
	// Preserve identity, creation order and match history.
	nr.ID = id
	nr.CreatedAt = st.rule.CreatedAt
	nr.seq = st.rule.seq
	st.rule = nr
	return snapshot(st), nil
}

func (e *engine) RemoveRule(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.byID[id]; !ok {
		return ErrRuleNotFound
	}
	delete(e.byID, id)
	return nil
}

func (e *engine) GetRule(id string) (Rule, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	st, ok := e.byID[id]
	if !ok {
		return Rule{}, false
	}
	return snapshot(st), true
}

func (e *engine) ListRules() []Rule {
	e.mu.RLock()
	states := make([]*ruleState, 0, len(e.byID))
	for _, st := range e.byID {
		states = append(states, st)
	}
	e.mu.RUnlock()
	sort.Slice(states, func(i, j int) bool {
		a, b := states[i].rule, states[j].rule
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.seq < b.seq
	})
	out := make([]Rule, 0, len(states))
	for _, st := range states {
		out = append(out, snapshot(st))
	}
	return out
}

func snapshot(st *ruleState) Rule {
	r := st.rule
	r.MatchedCount = st.matched.Load()
	return r
}

// --- evaluation ------------------------------------------------------

func (e *engine) Evaluate(msgType string, fields map[int]string) *Rule {
	// Rules fire on NewOrderSingle only (spec §32).
	if msgType != "D" {
		return nil
	}
	e.mu.RLock()
	states := make([]*ruleState, 0, len(e.byID))
	for _, st := range e.byID {
		states = append(states, st)
	}
	e.mu.RUnlock()
	sort.Slice(states, func(i, j int) bool {
		a, b := states[i].rule, states[j].rule
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.seq < b.seq
	})
	for _, st := range states {
		r := st.rule
		if !r.Enabled {
			continue
		}
		if matchAll(r.Predicates, fields) {
			st.matched.Add(1)
			snap := snapshot(st)
			return &snap
		}
	}
	return nil
}

func (e *engine) SetKillSwitch(on bool) { e.killSwitch.Store(on) }

func (e *engine) KillSwitch() bool { return e.killSwitch.Load() }

// matchAll reports whether every predicate matches (AND). An empty
// predicate list matches everything.
func matchAll(preds []Predicate, fields map[int]string) bool {
	for _, p := range preds {
		if !matchPredicate(p, fields) {
			return false
		}
	}
	return true
}

func matchPredicate(p Predicate, fields map[int]string) bool {
	v, ok := fields[p.Tag]
	switch p.Op {
	case OpEquals:
		if !ok {
			return p.Value == ""
		}
		if numericTags[p.Tag] {
			fv, err1 := strconv.ParseFloat(v, 64)
			pv, err2 := strconv.ParseFloat(p.Value, 64)
			return err1 == nil && err2 == nil && fv == pv
		}
		return v == p.Value
	case OpNotEquals:
		// Negation of eq, with the absent-field semantics inherited:
		// a missing field is "not equal" to any non-empty value.
		eq := Predicate{Tag: p.Tag, Op: OpEquals, Value: p.Value}
		return !matchPredicate(eq, fields)
	case OpContains:
		if !ok {
			return false
		}
		return strings.Contains(v, p.Value)
	case OpGreater:
		if !ok {
			return false
		}
		fv, err1 := strconv.ParseFloat(v, 64)
		pv, err2 := strconv.ParseFloat(p.Value, 64)
		return err1 == nil && err2 == nil && fv > pv
	case OpLess:
		if !ok {
			return false
		}
		fv, err1 := strconv.ParseFloat(v, 64)
		pv, err2 := strconv.ParseFloat(p.Value, 64)
		return err1 == nil && err2 == nil && fv < pv
	}
	return false
}

// --- validation ------------------------------------------------------

// normalize canonicalizes a rule as written by the API: op names become
// lowercase canonical ops, slices are non-nil for clean JSON.
func normalize(r Rule) (Rule, error) {
	for i := range r.Predicates {
		op, err := normalizeOp(string(r.Predicates[i].Op))
		if err != nil {
			return Rule{}, err
		}
		r.Predicates[i].Op = op
	}
	if r.Predicates == nil {
		r.Predicates = []Predicate{}
	}
	if r.Actions == nil {
		r.Actions = []ActionStep{}
	}
	return r, nil
}

func normalizeOp(op string) (PredicateOp, error) {
	switch strings.ToUpper(strings.TrimSpace(op)) {
	case "EQ", "EQUALS", "=":
		return OpEquals, nil
	case "NE", "NOT_EQUALS", "!=", "<>":
		return OpNotEquals, nil
	case "GT", "GREATER_THAN", ">":
		return OpGreater, nil
	case "LT", "LESS_THAN", "<":
		return OpLess, nil
	case "CONTAINS":
		return OpContains, nil
	}
	return "", validationErr("unknown predicate op %q (want eq, ne, gt, lt, contains)", op)
}

// validate enforces the V1 rule DSL (spec §32, §33).
func validate(r Rule) error {
	if len(r.Actions) == 0 {
		return validationErr("rule %q has no actions", r.Name)
	}
	for i, p := range r.Predicates {
		name, ok := PredicateTags[p.Tag]
		if !ok {
			return validationErr("predicate %d: unsupported tag %d (want one of 11, 1, 54, 55, 38, 40, 44)", i, p.Tag)
		}
		switch p.Op {
		case OpEquals, OpNotEquals, OpContains:
			// valid on any supported tag
		case OpGreater, OpLess:
			if !numericTags[p.Tag] {
				return validationErr("predicate %d: op %q requires a numeric tag (38, 40, 44), not %d (%s)",
					i, p.Op, p.Tag, name)
			}
			if _, err := strconv.ParseFloat(p.Value, 64); err != nil {
				return validationErr("predicate %d: op %q needs a numeric value, got %q", i, p.Op, p.Value)
			}
		default:
			return validationErr("predicate %d: unknown op %q", i, p.Op)
		}
	}
	for i, a := range r.Actions {
		switch a.Type {
		case ActionAckNew:
			// no params
		case ActionFullFill:
			if a.Price < 0 {
				return validationErr("action %d (FULL_FILL): price must be >= 0", i)
			}
		case ActionPartialFill:
			if a.Qty <= 0 || a.Price <= 0 {
				return validationErr("action %d (PARTIAL_FILL): qty and price must both be > 0", i)
			}
		case ActionReject:
			if strings.TrimSpace(a.OrdRejReason) == "" && strings.TrimSpace(a.Text) == "" {
				return validationErr("action %d (REJECT): ordRejReason or text is required", i)
			}
		case ActionDelay:
			if a.DelayMs < 0 || a.DelayMs > MaxDelayMs {
				return validationErr("action %d (DELAY): delayMs must be 0..%d", i, MaxDelayMs)
			}
		default:
			return validationErr("action %d: unknown action %q (want ACK_NEW, FULL_FILL, PARTIAL_FILL, REJECT, DELAY)", i, a.Type)
		}
	}
	return nil
}
