// Package scenario implements automatic scenario recording and replay
// (phase 2.3, spec §48).
//
// Recording is automatic and always on: every session keeps a bounded
// log of its application-level story — logon, inbound application
// messages, executions, logout/disconnect. Session-level admin messages
// (heartbeats, TestRequests, …) are deliberately NOT recorded: the
// scenario is the trading story, not the wire chatter.
//
// Replay re-enacts a recorded (or hand-written) scenario in a FRESH
// session. Inbound messages are re-injected SYNTHETICALLY through the
// simulator — the FIX engine is bypassed, nothing is transmitted on the
// wire, and no quota is consumed. Replay reproduces the application
// story (blotter, WS events, execution reports as data), not the wire
// bytes. Every replayed artifact is marked replayed:true, and replayed
// raws show the recorded application fields only — they are NOT
// re-validated wire images.
package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Step kinds recorded in a scenario.
const (
	KindLogon      = "logon"
	KindInbound    = "inbound"
	KindExecution  = "execution"
	KindLogout     = "logout"
	KindDisconnect = "disconnect"
)

// MaxSteps bounds the per-session scenario log (spec §48: e.g. 500
// steps). When full, the oldest steps are dropped (ring semantics,
// matching the message history).
const MaxSteps = 500

// Version is the scenario JSON schema version.
const Version = 1

// Execution actions recorded in execution steps. They mirror the
// simulator's execution vocabulary one-to-one.
const (
	ActionAckNew       = "ACK_NEW"
	ActionFill         = "FILL"
	ActionPartialFill  = "PARTIAL_FILL"
	ActionReject       = "REJECT"
	ActionCancelAccept = "CANCEL_ACCEPT"
	ActionCancelReject = "CANCEL_REJECT"
	// ActionReplaceAccept is recorded when a pending replace is
	// accepted (ExecType=5).
	ActionReplaceAccept = "REPLACE_ACCEPT"
	ActionReplaceReject = "REPLACE_REJECT"
)

// ExecutionActions is the closed vocabulary of replayable actions.
var ExecutionActions = map[string]bool{
	ActionAckNew: true, ActionFill: true, ActionPartialFill: true,
	ActionReject: true, ActionCancelAccept: true, ActionCancelReject: true,
	ActionReplaceAccept: true, ActionReplaceReject: true,
}

// Step is one recorded story event. AtMs is the offset from session
// start in milliseconds, so a scenario can be replayed with its original
// timing (or compressed with a speed multiplier).
type Step struct {
	Seq     int            `json:"seq"`
	AtMs    int64          `json:"atMs"`
	Kind    string         `json:"kind"`
	Payload map[string]any `json:"payload"`
}

// InboundPayload is the payload of an inbound step: one application
// message from the client. Fields carries the recorded tag→value map
// (including the original 8/9/10/34/49/52/56 framing values).
type InboundPayload struct {
	MsgType string         `json:"msgType"`
	Fields  map[int]string `json:"fields"`
}

// ExecutionPayload is the payload of an execution step: one action
// applied to a working order, with the parameters needed to re-apply
// it during replay.
type ExecutionPayload struct {
	Action       string  `json:"action"`
	ClOrdID      string  `json:"clOrdId"`
	Qty          float64 `json:"qty,omitempty"`
	Price        float64 `json:"price,omitempty"`
	OrdRejReason string  `json:"ordRejReason,omitempty"`
	CxlRejReason string  `json:"cxlRejReason,omitempty"`
	Text         string  `json:"text,omitempty"`
}

// Scenario is the exportable, replayable story of a session. It never
// contains the session token or any other secret.
type Scenario struct {
	Version      int       `json:"version"`
	Role         string    `json:"role"` // ACCEPTOR (replay-supported) or INITIATOR
	BeginString  string    `json:"beginString,omitempty"`
	TargetCompID string    `json:"targetCompId,omitempty"`
	RecordedAt   time.Time `json:"recordedAt"`
	Dropped      int64     `json:"dropped,omitempty"` // steps evicted by the cap
	Steps        []Step    `json:"steps"`
}

// PayloadMap builds a JSON-friendly payload map from a typed struct.
func PayloadMap(v any) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// DecodePayload decodes a step payload map back into a typed struct.
func DecodePayload[T any](m map[string]any) (T, error) {
	var t T
	raw, err := json.Marshal(m)
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return t, fmt.Errorf("scenario: malformed step payload: %w", err)
	}
	return t, nil
}

// Validate checks a scenario for replay: non-empty, known kinds,
// replayable inbound messages, known execution actions, and an
// ACCEPTOR role (initiator replay is not supported — see the replay
// player). Steps are sorted by AtMs in place (stable).
func Validate(sc *Scenario) error {
	if sc == nil {
		return errors.New("scenario: nil scenario")
	}
	if len(sc.Steps) == 0 {
		return errors.New("scenario: no steps to replay")
	}
	if sc.Role != "" && sc.Role != "ACCEPTOR" {
		return fmt.Errorf("scenario: replay of %s-recorded scenarios is not supported (ACCEPTOR sessions only)", sc.Role)
	}
	for i := range sc.Steps {
		st := &sc.Steps[i]
		switch st.Kind {
		case KindLogon, KindLogout, KindDisconnect:
			// Narrative only; skipped by the player.
		case KindInbound:
			p, err := DecodePayload[InboundPayload](st.Payload)
			if err != nil {
				return fmt.Errorf("scenario: step %d: %w", st.Seq, err)
			}
			if p.MsgType != "D" && p.MsgType != "F" && p.MsgType != "G" {
				return fmt.Errorf("scenario: step %d: inbound msgType %q is not replayable (want D, F or G)", st.Seq, p.MsgType)
			}
		case KindExecution:
			p, err := DecodePayload[ExecutionPayload](st.Payload)
			if err != nil {
				return fmt.Errorf("scenario: step %d: %w", st.Seq, err)
			}
			if !ExecutionActions[p.Action] {
				return fmt.Errorf("scenario: step %d: unknown execution action %q", st.Seq, p.Action)
			}
			if p.ClOrdID == "" {
				return fmt.Errorf("scenario: step %d: execution action %q missing clOrdId", st.Seq, p.Action)
			}
		default:
			return fmt.Errorf("scenario: step %d: unknown kind %q", st.Seq, st.Kind)
		}
	}
	sort.SliceStable(sc.Steps, func(i, j int) bool { return sc.Steps[i].AtMs < sc.Steps[j].AtMs })
	return nil
}

// StepDelay returns how long the replay player should wait before
// applying the step at atMs when the previous step was at prevAtMs.
// speed is the timing multiplier (2 = twice as fast); speed <= 0
// applies everything immediately.
func StepDelay(prevAtMs, atMs int64, speed float64) time.Duration {
	if speed <= 0 {
		return 0
	}
	d := float64(atMs-prevAtMs) / speed
	if d < 0 {
		d = 0
	}
	return time.Duration(d * float64(time.Millisecond))
}

// Recorder is the per-session automatic scenario log. It is safe for
// concurrent use and never blocks its callers (recording a step is a
// slice append under a mutex).
type Recorder struct {
	mu      sync.Mutex
	start   time.Time
	steps   []Step
	dropped int64
	paused  bool
	seq     int
}

// NewRecorder returns a recorder whose atMs offsets are measured from
// start (the session's creation time).
func NewRecorder(start time.Time) *Recorder {
	return &Recorder{start: start}
}

// Record appends one step. When paused (during replay) or when the log
// is only inspected, nothing is appended. When the cap is reached the
// oldest step is evicted (ring semantics).
func (r *Recorder) Record(kind string, payload any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused {
		return
	}
	r.seq++
	r.steps = append(r.steps, Step{
		Seq:     r.seq,
		AtMs:    time.Since(r.start).Milliseconds(),
		Kind:    kind,
		Payload: PayloadMap(payload),
	})
	if len(r.steps) > MaxSteps {
		copy(r.steps, r.steps[1:])
		r.steps = r.steps[:len(r.steps)-1]
		r.dropped++
	}
}

// SetPaused suspends/resumes recording. The replay player pauses the
// shared recorder so the synthetic re-enactment is not recorded as new
// history.
func (r *Recorder) SetPaused(p bool) {
	r.mu.Lock()
	r.paused = p
	r.mu.Unlock()
}

// Len returns the number of recorded steps.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.steps)
}

// Snapshot returns the exportable scenario: a copy of the recorded
// steps plus metadata. Role/BeginString/TargetCompID are filled by the
// caller (the recorder does not know the session identity).
func (r *Recorder) Snapshot() Scenario {
	r.mu.Lock()
	defer r.mu.Unlock()
	steps := make([]Step, len(r.steps))
	copy(steps, r.steps)
	return Scenario{
		Version:    Version,
		RecordedAt: time.Now(),
		Dropped:    r.dropped,
		Steps:      steps,
	}
}
