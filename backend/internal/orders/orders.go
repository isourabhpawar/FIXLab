// Package orders defines the order model and the per-session blotter
// store (spec §23). The trading simulator (phase 3) populates it from
// inbound NewOrderSingle / OrderCancelRequest /
// OrderCancelReplaceRequest messages and from browser execution actions.
package orders

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// Status is the lifecycle state of a simulated order (spec §23).
type Status string

const (
	StatusNew             Status = "NEW"
	StatusPartiallyFilled Status = "PARTIALLY_FILLED"
	StatusFilled          Status = "FILLED"
	StatusRejected        Status = "REJECTED"
	StatusPendingCancel   Status = "PENDING_CANCEL"
	StatusCanceled        Status = "CANCELED"
	StatusReplaced        Status = "REPLACED"
)

// Working reports whether the order can still receive executions.
// REPLACED orders remain working: a replaced order can still be filled,
// cancelled, or replaced again.
func (s Status) Working() bool {
	switch s {
	case StatusNew, StatusPartiallyFilled, StatusReplaced:
		return true
	}
	return false
}

// Terminal reports whether the order's lifecycle is over.
func (s Status) Terminal() bool {
	switch s {
	case StatusFilled, StatusRejected, StatusCanceled:
		return true
	}
	return false
}

// PendingReplace carries a requested-but-unconfirmed replace (inbound
// 35=G) until the browser accepts or rejects it (spec §29).
type PendingReplace struct {
	ClOrdID  string  `json:"clOrdId"`
	OrderQty float64 `json:"orderQty"`
	Price    float64 `json:"price"`
}

// Direction marks which side originated the order: INBOUND for orders
// received from the developer's engine (acceptor mode, phase 3), OUTBOUND
// for orders FixLab injected into the remote counterparty (initiator
// mode, phase 5).
type Direction string

const (
	DirectionInbound  Direction = "INBOUND"
	DirectionOutbound Direction = "OUTBOUND"
)

// Order is one simulated order on the blotter.
type Order struct {
	// OrderID is the server-generated order identifier (tag 37).
	OrderID string `json:"orderId"`
	ClOrdID string `json:"clOrdId"`
	// Direction is INBOUND (acceptor) or OUTBOUND (initiator injection).
	Direction Direction `json:"direction,omitempty"`
	Symbol  string `json:"symbol"`
	Side    string `json:"side"` // "1" = Buy, "2" = Sell, ...
	OrderQty float64 `json:"orderQty"`
	Price    float64 `json:"price"`
	OrdType  string  `json:"ordType"`

	CumQty    float64 `json:"cumQty"`
	LeavesQty float64 `json:"leavesQty"`
	AvgPx     float64 `json:"avgPx"`

	Status Status `json:"status"`
	// PrevStatus remembers the working state while a cancel request is
	// pending (Status == PENDING_CANCEL), so a cancel-reject can restore
	// it.
	PrevStatus Status `json:"prevStatus,omitempty"`

	// CancelReqClOrdID is the ClOrdID of the inbound 35=F that put the
	// order into PENDING_CANCEL; it becomes tag 11 of a 35=9 reject.
	CancelReqClOrdID string `json:"cancelReqClOrdId,omitempty"`
	// PendingReplace is non-nil while an inbound 35=G awaits browser
	// accept/reject.
	PendingReplace *PendingReplace `json:"pendingReplace,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Replayed marks orders created by a scenario replay (phase 2.3):
	// the order was re-enacted synthetically, never sent by a client.
	Replayed bool `json:"replayed,omitempty"`
}

// Errors returned by the store.
var (
	ErrOrderExists      = errors.New("orders: duplicate ClOrdID")
	ErrOrderCapExceeded = errors.New("orders: blotter capacity exceeded")
	ErrOrderNotFound    = errors.New("orders: order not found")
)

// Store is the order blotter (spec §23). Implementations must be safe
// for concurrent use: inbound FIX callbacks and HTTP execution requests
// touch it from different goroutines.
type Store interface {
	Add(o *Order) error
	Get(clOrdID string) (*Order, bool)
	Update(o *Order) error
	List() []*Order // insertion order
	Count() int
	Clear()
}

type inMemoryStore struct {
	mu   sync.RWMutex
	m    map[string]*Order
	seq  []string // insertion order of ClOrdIDs
	cap  int
}

// NewInMemoryStore returns a Store holding at most cap orders (spec §4
// default: 1000 per session).
func NewInMemoryStore(cap int) Store {
	if cap <= 0 {
		cap = 1000
	}
	return &inMemoryStore{m: map[string]*Order{}, cap: cap}
}

func (s *inMemoryStore) Add(o *Order) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[o.ClOrdID]; ok {
		return ErrOrderExists
	}
	if len(s.m) >= s.cap {
		return ErrOrderCapExceeded
	}
	now := time.Now()
	if o.CreatedAt.IsZero() {
		o.CreatedAt = now
	}
	o.UpdatedAt = now
	// Store a copy so callers cannot mutate the blotter behind the lock.
	cp := *o
	s.m[o.ClOrdID] = &cp
	s.seq = append(s.seq, o.ClOrdID)
	return nil
}

func (s *inMemoryStore) Get(clOrdID string) (*Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.m[clOrdID]
	if !ok {
		return nil, false
	}
	cp := *o
	return &cp, true
}

func (s *inMemoryStore) Update(o *Order) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[o.ClOrdID]; !ok {
		return ErrOrderNotFound
	}
	o.UpdatedAt = time.Now()
	cp := *o
	s.m[o.ClOrdID] = &cp
	return nil
}

func (s *inMemoryStore) List() []*Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Order, 0, len(s.seq))
	for _, id := range s.seq {
		if o, ok := s.m[id]; ok {
			cp := *o
			out = append(out, &cp)
		}
	}
	// Newest first is friendlier for the blotter; keep it deterministic.
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

func (s *inMemoryStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}

func (s *inMemoryStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m = map[string]*Order{}
	s.seq = nil
}
