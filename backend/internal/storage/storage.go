// Package storage defines the metadata persistence seam for FixLab.
//
// V1 is primarily ephemeral (spec §39): live FIX state stays in memory and
// PostgreSQL is only for metadata if required. This package therefore
// exposes a Store interface backed by an in-memory implementation; a
// PostgreSQL implementation can be dropped in later without touching the
// session manager.
package storage

import (
	"sync"
	"time"
)

// SessionMeta is the persistable metadata for one sandbox session. It
// carries no live handles — only the facts a future SQL store would keep.
type SessionMeta struct {
	Token        string
	Role         string
	BeginString  string
	SenderCompID string
	TargetCompID string
	Port         int
	TLS          bool
	Status       string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// Store persists session metadata.
type Store interface {
	Put(m *SessionMeta) error
	Get(token string) (*SessionMeta, bool)
	Delete(token string) error
	List() []*SessionMeta
	Count() int
}

// MemoryStore is the in-memory Store implementation used for V1.
type MemoryStore struct {
	mu   sync.RWMutex
	data map[string]*SessionMeta
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: map[string]*SessionMeta{}}
}

func copyMeta(m *SessionMeta) *SessionMeta {
	c := *m
	return &c
}

// Put inserts or replaces metadata for a token.
func (s *MemoryStore) Put(m *SessionMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[m.Token] = copyMeta(m)
	return nil
}

// Get returns metadata for a token.
func (s *MemoryStore) Get(token string) (*SessionMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.data[token]
	if !ok {
		return nil, false
	}
	return copyMeta(m), true
}

// Delete removes metadata for a token; missing tokens are not an error.
func (s *MemoryStore) Delete(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, token)
	return nil
}

// List returns all stored metadata.
func (s *MemoryStore) List() []*SessionMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*SessionMeta, 0, len(s.data))
	for _, m := range s.data {
		out = append(out, copyMeta(m))
	}
	return out
}

// Count returns the number of stored sessions.
func (s *MemoryStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}
