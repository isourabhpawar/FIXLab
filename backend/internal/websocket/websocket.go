// Package websocket defines the live event-streaming contract and the
// in-memory fan-out hub behind GET /ws/session/{token} (spec §19).
//
// The hub is session-scoped: browsers subscribe with a session token and
// receive every event for that sandbox until they disconnect or the
// session is destroyed. Fan-out is bounded — each subscriber has a
// buffered channel and slow subscribers drop events rather than blocking
// the FIX engine goroutines that publish.
package websocket

import (
	"sync"
	"sync/atomic"
	"time"
)

// Event is one streamed occurrence (spec §19). Every event carries a
// type, session id, timestamp, and payload.
type Event struct {
	Type      string    `json:"type"`
	SessionID string    `json:"sessionId"`
	Timestamp time.Time `json:"timestamp"`
	Payload   any       `json:"payload"`
}

// Event types streamed to browsers (spec §19).
const (
	EventConnectionStatus = "CONNECTION_STATUS"
	EventFIXMsgIn         = "FIX_MSG_IN"
	EventFIXMsgOut        = "FIX_MSG_OUT"
	EventOrderCreated     = "ORDER_CREATED"  // phase 3
	EventOrderUpdated     = "ORDER_UPDATED"  // phase 3
	EventExecutionSent    = "EXECUTION_SENT" // phase 3
	EventSessionError     = "SESSION_ERROR"
	EventSequenceGap      = "SEQUENCE_GAP"
	EventSessionExpired   = "SESSION_EXPIRED"
	EventKillSwitch       = "KILL_SWITCH" // phase 4: payload {enabled: bool}
	// EventScenarioReplayStarted streams when a scenario replay begins
	// (phase 2.3): payload {speed, totalSteps}.
	EventScenarioReplayStarted = "SCENARIO_REPLAY_STARTED"
	// EventScenarioReplayFinished streams when a scenario replay ends
	// (phase 2.3): payload {appliedSteps, totalSteps, failed, error,
	// durationMs}.
	EventScenarioReplayFinished = "SCENARIO_REPLAY_FINISHED"
)

// subscriberBuffer bounds how many undelivered events a single browser
// connection may lag by before events start dropping.
const subscriberBuffer = 256

// Hub publishes session events to subscribed browsers.
type Hub interface {
	// Publish fans an event out to every subscriber of its session.
	// It never blocks the caller; slow subscribers drop events.
	Publish(ev Event)
	// Subscribe returns a channel of events for sessionID and an
	// unsubscribe function. The channel is closed when Close(sessionID)
	// is called.
	Subscribe(sessionID string) (<-chan Event, func())
	// Close terminates every subscription for a session (used when the
	// session is destroyed or expires).
	Close(sessionID string)
	// Dropped returns how many events were dropped for slow subscribers
	// of a session. It is 0 for unknown sessions.
	Dropped(sessionID string) int64
}

// NoopHub discards events; useful in tests that don't need streaming.
type NoopHub struct{}

// Publish implements Hub.
func (NoopHub) Publish(Event) {}

// Subscribe implements Hub.
func (NoopHub) Subscribe(string) (<-chan Event, func()) {
	ch := make(chan Event)
	return ch, func() {}
}

// Close implements Hub.
func (NoopHub) Close(string) {}

// Dropped implements Hub.
func (NoopHub) Dropped(string) int64 { return 0 }

type subscriber struct {
	ch      chan Event
	dropped atomic.Int64
}

type hub struct {
	mu   sync.RWMutex
	subs map[string]map[*subscriber]struct{}
}

// NewHub returns the in-memory fan-out hub.
func NewHub() Hub {
	return &hub{subs: make(map[string]map[*subscriber]struct{})}
}

func (h *hub) Publish(ev Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	// The read lock is held across the (non-blocking) sends so Close
	// cannot close a channel mid-send.
	for s := range h.subs[ev.SessionID] {
		select {
		case s.ch <- ev:
		default:
			s.dropped.Add(1)
		}
	}
}

func (h *hub) Subscribe(sessionID string) (<-chan Event, func()) {
	s := &subscriber{ch: make(chan Event, subscriberBuffer)}
	h.mu.Lock()
	set, ok := h.subs[sessionID]
	if !ok {
		set = make(map[*subscriber]struct{})
		h.subs[sessionID] = set
	}
	set[s] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	unsub := func() {
		once.Do(func() {
			h.mu.Lock()
			if set, ok := h.subs[sessionID]; ok {
				delete(set, s)
				if len(set) == 0 {
					delete(h.subs, sessionID)
				}
			}
			h.mu.Unlock()
		})
	}
	return s.ch, unsub
}

func (h *hub) Close(sessionID string) {
	h.mu.Lock()
	set := h.subs[sessionID]
	delete(h.subs, sessionID)
	h.mu.Unlock()
	// Channels are closed only after the session's entry is removed, and
	// Publish holds the read lock across sends, so no send can race a
	// close here.
	for s := range set {
		close(s.ch)
	}
}

func (h *hub) Dropped(sessionID string) int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var total int64
	for s := range h.subs[sessionID] {
		total += s.dropped.Load()
	}
	return total
}
