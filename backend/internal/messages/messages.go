// Package messages defines the canonical FIX message representation used
// across the backend (spec §18) and the bounded in-memory message history.
package messages

import (
	"sync"
	"time"
)

// Direction indicates which way a FIX message travelled relative to FixLab.
type Direction string

const (
	// Inbound travelled client → FixLab.
	Inbound Direction = "INBOUND"
	// Outbound travelled FixLab → client.
	Outbound Direction = "OUTBOUND"
)

// Admin message types per spec §4. Session-level administrative messages
// must not consume the application-message quota.
var AdminMsgTypes = map[string]bool{
	"A": true, // Logon
	"0": true, // Heartbeat
	"1": true, // TestRequest
	"2": true, // ResendRequest
	"4": true, // SequenceReset
	"5": true, // Logout
}

// IsAdminMsgType reports whether a 35= value is a session-level
// administrative message.
func IsAdminMsgType(msgType string) bool { return AdminMsgTypes[msgType] }

// Field is one parsed FIX tag/value pair with dictionary enrichment.
type Field struct {
	Tag             int    `json:"tag"`
	Name            string `json:"name"`
	Value           string `json:"value"`
	EnumDescription string `json:"enumDescription,omitempty"`
}

// Record is the stored view of a single FIX message (spec §18).
type Record struct {
	Type         string    `json:"type"` // always "FIX_MESSAGE"
	Direction    Direction `json:"direction"`
	MsgSeqNum    int       `json:"msgSeqNum"`
	MsgType      string    `json:"msgType"`
	MsgName      string    `json:"msgName"`
	RawFIX       string    `json:"rawFix"` // pipe-delimited for readability
	SenderCompID string    `json:"senderCompId"`
	TargetCompID string    `json:"targetCompId"`
	Timestamp    time.Time `json:"timestamp"`
	Fields       []Field   `json:"fields,omitempty"`
	// Replayed marks synthetic records produced by a scenario replay
	// (phase 2.3): the message was never on the wire.
	Replayed bool `json:"replayed,omitempty"`
}

// RingBuffer is a bounded, goroutine-safe message history. When full, the
// oldest records are overwritten (spec §4: message history is bounded).
type RingBuffer struct {
	mu   sync.RWMutex
	buf  []*Record
	cap  int
	head int // index of the oldest element
	size int
}

// NewRingBuffer creates a history buffer holding at most cap records.
func NewRingBuffer(cap int) *RingBuffer {
	if cap <= 0 {
		cap = 500
	}
	return &RingBuffer{buf: make([]*Record, cap), cap: cap}
}

// Push appends a record, evicting the oldest when full.
func (r *RingBuffer) Push(rec *Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size < r.cap {
		r.buf[(r.head+r.size)%r.cap] = rec
		r.size++
		return
	}
	r.buf[r.head] = rec
	r.head = (r.head + 1) % r.cap
}

// List returns records oldest-first. If limit > 0 at most limit records
// are returned (the most recent ones).
func (r *RingBuffer) List(limit int) []*Record {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Record, 0, r.size)
	for i := 0; i < r.size; i++ {
		out = append(out, r.buf[(r.head+i)%r.cap])
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Len returns the number of records currently stored.
func (r *RingBuffer) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.size
}

// Clear drops all records.
func (r *RingBuffer) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.head, r.size = 0, 0
}
