package security

import (
	"testing"
	"time"
)

func TestRateLimiter_BurstThenDeny(t *testing.T) {
	l := NewRateLimiter(5, time.Minute)
	for i := 0; i < 5; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("attempt %d denied, want allowed (burst)", i+1)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("6th attempt allowed, want denied")
	}
}

func TestRateLimiter_PerKeyIsolation(t *testing.T) {
	l := NewRateLimiter(1, time.Minute)
	if !l.Allow("a") {
		t.Fatal("first attempt for a denied")
	}
	if !l.Allow("b") {
		t.Fatal("first attempt for b denied (should be isolated)")
	}
	if l.Allow("a") {
		t.Fatal("second attempt for a allowed too soon")
	}
}

func TestRateLimiter_Refills(t *testing.T) {
	l := NewRateLimiter(2, 100*time.Millisecond)
	if !l.Allow("k") || !l.Allow("k") {
		t.Fatal("initial burst denied")
	}
	if l.Allow("k") {
		t.Fatal("over-burst allowed")
	}
	time.Sleep(150 * time.Millisecond)
	if !l.Allow("k") {
		t.Fatal("token did not refill after window")
	}
}
