package session

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"fixlab.dev/fixlab/backend/internal/common"
	"fixlab.dev/fixlab/backend/internal/engine"
	"fixlab.dev/fixlab/backend/internal/messages"
	"fixlab.dev/fixlab/backend/internal/security"
	"fixlab.dev/fixlab/backend/internal/storage"
	"fixlab.dev/fixlab/backend/internal/websocket"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig() common.Config {
	cfg := common.LoadConfig()
	cfg.PortPoolMin = 44500
	cfg.PortPoolMax = 44510
	cfg.SessionTTL = time.Hour
	cfg.CleanupInterval = 50 * time.Millisecond
	cfg.TCPLogonTimeout = 30 * time.Second
	cfg.TCPIdleTimeout = 30 * time.Second
	return cfg
}

func newTestManager(t *testing.T, cfg common.Config) (Manager, *common.Metrics) {
	t.Helper()
	m := &common.Metrics{}
	mgr, err := NewManager(cfg, testLogger(), m, storage.NewMemoryStore(), websocket.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Shutdown)
	return mgr, m
}

func TestManager_CreateGetDestroy(t *testing.T) {
	cfg := testConfig()
	mgr, m := newTestManager(t, cfg)

	s, err := mgr.CreateSession(context.Background(), CreateRequest{TargetCompID: "UT1"})
	if err != nil {
		t.Fatal(err)
	}
	if !security.IsValidToken(s.Token) {
		t.Fatalf("bad token %q", s.Token)
	}
	if m.ActiveSessions.Load() != 1 {
		t.Fatalf("ActiveSessions = %d, want 1", m.ActiveSessions.Load())
	}

	info := mgr.Info(s)
	if info.Role != "ACCEPTOR" {
		t.Fatalf("role = %q", info.Role)
	}
	if info.Endpoint.Port < 44500 || info.Endpoint.Port > 44510 {
		t.Fatalf("port %d outside test pool", info.Endpoint.Port)
	}
	if info.Endpoint.TLS {
		t.Fatal("TLS should be false in phase 1")
	}
	if info.Identifiers.BeginString != "FIX.4.4" ||
		info.Identifiers.SenderCompID != "FIXLAB" ||
		info.Identifiers.TargetCompID != "UT1" {
		t.Fatalf("identifiers = %+v", info.Identifiers)
	}
	if time.Until(info.ExpiresAt) > time.Hour || time.Until(info.ExpiresAt) < 59*time.Minute {
		t.Fatalf("expiresAt = %v", info.ExpiresAt)
	}
	if info.Status != string(StatusWaiting) {
		t.Fatalf("status = %q, want WAITING_FOR_CONNECTION", info.Status)
	}

	st, err := mgr.GetStatus(s.Token)
	if err != nil || st != StatusWaiting {
		t.Fatalf("GetStatus = %v, %v", st, err)
	}

	if _, err := mgr.GetSession("fixlab_" + strings.Repeat("0", 64)); err == nil {
		t.Fatal("expected error for unknown token")
	}

	if err := mgr.DestroySession(s.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.GetSession(s.Token); err == nil {
		t.Fatal("expected error after destroy")
	}
	if m.ActiveSessions.Load() != 0 {
		t.Fatalf("ActiveSessions = %d, want 0", m.ActiveSessions.Load())
	}
	if err := mgr.DestroySession(s.Token); err == nil {
		t.Fatal("double destroy should fail")
	}
}

func TestManager_RejectsUnsupportedOptions(t *testing.T) {
	mgr, _ := newTestManager(t, testConfig())
	ctx := context.Background()
	for _, req := range []CreateRequest{
		{Role: "INITIATOR"},
		{FIXVersion: "FIX.4.2"},
		{Transport: "SCTP"},
		{Auth: "HMAC-SHA256"},
	} {
		if _, err := mgr.CreateSession(ctx, req); err == nil {
			t.Fatalf("expected error for %+v", req)
		}
	}
}

// TestManager_TLS covers phase-7 edge TLS: tls:true creates a TLS
// session with the cert fingerprint exposed, "TLS" transport is an
// alias, and a broken certificate setup refuses tls:true with a 400
// naming the problem.
func TestManager_TLS(t *testing.T) {
	cfg := testConfig()
	mgr, _ := newTestManager(t, cfg)
	impl := mgr.(*manager)
	ctx := context.Background()

	s, err := mgr.CreateSession(ctx, CreateRequest{TLS: true, TargetCompID: "TLSUT"})
	if err != nil {
		t.Fatalf("tls:true session: %v", err)
	}
	if !s.TLS {
		t.Fatal("session TLS flag not set")
	}
	info := mgr.Info(s)
	if !info.Endpoint.TLS {
		t.Fatal("endpoint.tls not reported")
	}
	if len(info.CertFingerprint) != 64 {
		t.Fatalf("certFingerprint = %q, want 64 hex chars", info.CertFingerprint)
	}
	if !info.CertSelfSigned {
		t.Fatal("test manager should use the self-signed dev cert")
	}
	if err := mgr.DestroySession(s.Token); err != nil {
		t.Fatal(err)
	}

	// "TLS" as a transport string is an accepted alias.
	s2, err := mgr.CreateSession(ctx, CreateRequest{Transport: "TLS", TargetCompID: "TLSUT2"})
	if err != nil {
		t.Fatalf("transport TLS: %v", err)
	}
	if !s2.TLS {
		t.Fatal("transport TLS did not enable TLS")
	}
	if err := mgr.DestroySession(s2.Token); err != nil {
		t.Fatal(err)
	}

	// Broken certificate setup: tls:true is refused naming the problem.
	impl.tlsSetup = security.TLSSetup{Err: fmt.Errorf("test: cert file unreadable")}
	if _, err := mgr.CreateSession(ctx, CreateRequest{TLS: true}); err == nil ||
		!strings.Contains(err.Error(), "no usable TLS certificate") {
		t.Fatalf("expected no-usable-certificate error, got: %v", err)
	}
	// Plain TCP still works with a broken TLS setup.
	if _, err := mgr.CreateSession(ctx, CreateRequest{}); err != nil {
		t.Fatalf("plaintext session with broken TLS setup: %v", err)
	}
}

// TestManager_PerIPSessionCap covers the spec §44 anonymous default:
// one active session per client IP (429 past the limit).
func TestManager_PerIPSessionCap(t *testing.T) {
	cfg := testConfig()
	cfg.MaxSessionsPerIP = 1
	mgr, _ := newTestManager(t, cfg)
	ctxWithIP := func(ip string) context.Context {
		return context.WithValue(context.Background(), ClientIPKey, ip)
	}

	s1, err := mgr.CreateSession(ctxWithIP("10.9.9.9"), CreateRequest{TargetCompID: "IP1"})
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	if _, err := mgr.CreateSession(ctxWithIP("10.9.9.9"), CreateRequest{TargetCompID: "IP2"}); err == nil ||
		!strings.Contains(err.Error(), "active session limit") {
		t.Fatalf("expected active-session-limit error, got: %v", err)
	}
	// A different IP is unaffected.
	s2, err := mgr.CreateSession(ctxWithIP("10.9.9.10"), CreateRequest{TargetCompID: "IP3"})
	if err != nil {
		t.Fatalf("other IP: %v", err)
	}
	// After destroy, the IP may create again.
	if err := mgr.DestroySession(s1.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.CreateSession(ctxWithIP("10.9.9.9"), CreateRequest{TargetCompID: "IP4"}); err != nil {
		t.Fatalf("recreate after destroy: %v", err)
	}
	_ = mgr.DestroySession(s2.Token)
}

func TestManager_PortReleasedOnDestroy(t *testing.T) {
	mgr, _ := newTestManager(t, testConfig())
	s, err := mgr.CreateSession(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	port := s.Port
	impl := mgr.(*manager)
	if _, held := impl.ports.(*portManager).HeldBy(port); !held {
		t.Fatal("port not marked held after create")
	}
	if err := mgr.DestroySession(s.Token); err != nil {
		t.Fatal(err)
	}
	if _, held := impl.ports.(*portManager).HeldBy(port); held {
		t.Fatal("port still held after destroy")
	}
	// The released port must be acquirable again.
	if _, err := impl.ports.Acquire("reuse"); err != nil {
		t.Fatalf("released port not reusable: %v", err)
	}
}

func TestManager_ExpiryCleanup(t *testing.T) {
	cfg := testConfig()
	cfg.SessionTTL = 500 * time.Millisecond
	mgr, m := newTestManager(t, cfg)

	s, err := mgr.CreateSession(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// The production server starts this; the test must start it explicitly.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.RunCleanupLoop(ctx)

	// Wait for the reap to be fully processed: the session must be gone
	// from the registry AND DestroySession must have run to completion
	// (ActiveSessions is decremented last, after the engine unregisters
	// its session ID).
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, gerr := mgr.GetSession(s.Token)
		if gerr != nil && m.ActiveSessions.Load() == 0 && m.SessionExpirations.Load() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired session was not reaped")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestManager_AdminMessagesNotCounted(t *testing.T) {
	mgr, _ := newTestManager(t, testConfig())
	s, err := mgr.CreateSession(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the engine callback stream: admin messages must not
	// consume the application-message quota.
	for _, mt := range []string{"A", "0", "1", "2", "4", "5"} {
		s.OnMessage(engine.FIXEvent{Direction: messages.Inbound, MsgType: mt, At: time.Now()})
	}
	s.OnMessage(engine.FIXEvent{Direction: messages.Inbound, MsgType: "D", MsgName: "NewOrderSingle", At: time.Now()})
	if got := s.AppMsgCount.Load(); got != 1 {
		t.Fatalf("AppMsgCount = %d, want 1 (only the NewOrderSingle)", got)
	}
	if n := len(s.MessageHistory(0)); n != 7 {
		t.Fatalf("history holds %d records, want 7", n)
	}
	rec := s.MessageHistory(1)[0]
	if rec.MsgName != "NewOrderSingle" || rec.Type != "FIX_MESSAGE" {
		t.Fatalf("record = %+v", rec)
	}
}
