package security

import (
	"sync"
	"time"
)

// RateLimiter is a per-key token bucket used for both TCP connection
// rate limiting and HTTP API rate limiting (spec §41, §44). Buckets are
// created lazily and evicted after they go stale, so memory stays bounded.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens added per window
	window  time.Duration
	burst   int // maximum bucket size
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// NewRateLimiter allows `rate` events per `window`, with bursts up to
// `burst` (burst should be >= 1; values < 1 are clamped to rate).
func NewRateLimiter(rate int, window time.Duration) *RateLimiter {
	burst := rate
	if burst < 1 {
		burst = 1
	}
	return &RateLimiter{
		buckets: map[string]*bucket{},
		rate:    float64(rate),
		window:  window,
		burst:   burst,
	}
}

// Allow reports whether one event for key may proceed, consuming a token.
func (l *RateLimiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(l.burst), lastSeen: now}
		l.buckets[key] = b
		// Opportunistic eviction of stale buckets (older than two windows)
		// so long-lived processes don't accumulate keys forever.
		for k, ob := range l.buckets {
			if now.Sub(ob.lastSeen) > 2*l.window {
				delete(l.buckets, k)
			}
		}
	}
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.tokens += elapsed * l.rate / l.window.Seconds()
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	b.lastSeen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Size returns the number of tracked keys (for tests).
func (l *RateLimiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
