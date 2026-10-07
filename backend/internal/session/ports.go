package session

import (
	"fmt"
	"net"
	"sync"
)

// PortManager allocates externally reachable TCP ports from a configured
// pool (spec §13).
//
// The actual socket bind is authoritative for availability: Acquire probes
// each candidate with a real bind instead of trusting an in-memory map.
// Callers must bind the returned port promptly; Release returns it to the
// pool.
type PortManager interface {
	Acquire(sessionID string) (int, error)
	Release(port int)
}

type portManager struct {
	mu       sync.Mutex
	min, max int
	held     map[int]string // port -> sessionID
}

// NewPortManager creates a pool over the inclusive range [min, max].
func NewPortManager(min, max int) PortManager {
	return &portManager{min: min, max: max, held: map[int]string{}}
}

// bindProbe reports whether a port can actually be bound right now.
func bindProbe(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// Acquire returns a free port from the pool, recording the holder.
func (p *portManager) Acquire(sessionID string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for port := p.min; port <= p.max; port++ {
		if _, taken := p.held[port]; taken {
			continue
		}
		if !bindProbe(port) {
			// Something outside our map holds it (another process,
			// TIME_WAIT, etc.) — skip; the bind is authoritative.
			continue
		}
		p.held[port] = sessionID
		return port, nil
	}
	return 0, fmt.Errorf("session: port pool %d-%d exhausted", p.min, p.max)
}

// Release returns a port to the pool. Unknown ports are ignored.
func (p *portManager) Release(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.held, port)
}

// HeldBy returns the session holding a port, for diagnostics.
func (p *portManager) HeldBy(port int) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.held[port]
	return s, ok
}
