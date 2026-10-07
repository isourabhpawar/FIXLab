package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"fixlab.dev/fixlab/backend/internal/messages"
)

// fakeHooks is a no-op Hooks implementation for initiator tests.
type fakeHooks struct{}

func (f *fakeHooks) OnEngineLogon()                                  {}
func (f *fakeHooks) OnEngineLogout()                                 {}
func (f *fakeHooks) OnMessage(_ FIXEvent)                            {}
func (f *fakeHooks) OnSequenceGap(_, _ int, _ messages.Direction)    {}
func (f *fakeHooks) OnEngineError(_ error)                           {}

// TestStartInitiatorRejectsBlockedDialIP verifies the connect-path
// re-validation: even if a caller skipped ValidateInitiatorTarget, a
// blocked pinned IP never reaches the dial.
func TestStartInitiatorRejectsBlockedDialIP(t *testing.T) {
	t.Setenv("FIXLAB_TEST_ALLOW_PRIVATE", "false")
	m := NewManager()
	_, err := m.StartInitiator(context.Background(), InitiatorConfig{
		RemoteHost:         "evil.example",
		DialIP:             "10.0.0.5",
		RemotePort:         9878,
		RemoteCompID:       "R",
		LocalCompID:        "L",
		DataDictionaryPath: "/tmp/FIX44.xml",
		Hooks:              &fakeHooks{},
		ConnectTimeout:     0, // unused on this path
	})
	if err == nil {
		t.Fatal("expected rejection of blocked dial IP, got success")
	}
	if !strings.Contains(err.Error(), "blocked range") {
		t.Errorf("error should name the blocked range, got: %v", err)
	}
}

// TestStartInitiatorAcceptsTLS ensures TLS dials are accepted in phase
// 7 (the TLS settings are applied; the handshake itself is proven by
// the phase-7 e2e against a real TLS counterparty).
func TestStartInitiatorAcceptsTLS(t *testing.T) {
	m := NewManager()
	h, err := m.StartInitiator(context.Background(), InitiatorConfig{
		RemoteHost:         "broker.example",
		DialIP:             "203.0.113.7",
		RemotePort:         9878,
		RemoteCompID:       "R",
		LocalCompID:        "L",
		TLS:                true,
		InsecureSkipVerify: true,
		DataDictionaryPath: "../../specs/FIX44.xml",
		Hooks:              &fakeHooks{},
		ConnectTimeout:     time.Second,
	})
	if err != nil {
		t.Fatalf("TLS initiator should be accepted in phase 7, got: %v", err)
	}
	_ = h.Stop()
}

// TestStartInitiatorRequiresPinnedIP ensures the SSRF validation must
// run first: no pinned IP, no dial.
func TestStartInitiatorRequiresPinnedIP(t *testing.T) {
	m := NewManager()
	_, err := m.StartInitiator(context.Background(), InitiatorConfig{
		RemoteHost:         "broker.example",
		RemotePort:         9878,
		RemoteCompID:       "R",
		LocalCompID:        "L",
		DataDictionaryPath: "/tmp/FIX44.xml",
		Hooks:              &fakeHooks{},
	})
	if err == nil || !strings.Contains(err.Error(), "pinned dial IP") {
		t.Errorf("expected pinned-IP requirement, got: %v", err)
	}
}
