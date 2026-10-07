package security

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"fixlab.dev/fixlab/backend/internal/common"
)

// fixFrame builds a well-formed FIX frame (SOH-delimited) with correct
// BodyLength and CheckSum for the given ordered tag/value pairs.
func fixFrame(pairs ...[2]string) []byte {
	var b strings.Builder
	for _, p := range pairs {
		b.WriteString(p[0])
		b.WriteByte('=')
		b.WriteString(p[1])
		b.WriteByte(0x01)
	}
	body := b.String()
	head := fmt.Sprintf("8=FIX.4.4\x019=%d\x01", len(body))
	sum := 0
	for i := 0; i < len(head)+len(body); i++ {
		sum += int((head + body)[i])
	}
	return []byte(head + body + fmt.Sprintf("10=%03d\x01", sum%256))
}

func logonFrame(heartBtInt int) []byte {
	return fixFrame(
		[2]string{"35", "A"},
		[2]string{"49", "CLIENT"},
		[2]string{"56", "FIXLAB"},
		[2]string{"34", "1"},
		[2]string{"52", "20261007-00:00:00.000"},
		[2]string{"108", fmt.Sprint(heartBtInt)},
	)
}

// fakeBackend listens on loopback and either discards or echoes bytes.
func fakeBackend(t *testing.T, echo bool) (addr string, shutdown func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					select {
					case <-done:
						return
					default:
					}
					// Long deadline: the fake backend must hold the
					// connection open so the guard's own timeouts fire.
					_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
					n, err := c.Read(buf)
					if n > 0 && echo {
						_, _ = c.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String(), func() { close(done); ln.Close() }
}

func testGuardConfig(backend string, m *common.Metrics) GuardConfig {
	return GuardConfig{
		PublicPort:    0, // allocate below via listener? NewGuard binds cfg.PublicPort; 0 = ephemeral
		BackendAddr:   backend,
		SessionLabel:  "test",
		MaxFrameBytes: 8192,
		IdleTimeout:   5 * time.Second,
		LogonTimeout:  5 * time.Second,
		MinHeartBtInt: 10,
		ConnLimiter:   NewRateLimiter(1000, time.Minute),
		Metrics:       m,
	}
}

// newTestGuard binds an ephemeral public port by passing 0.
func newTestGuard(t *testing.T, cfg GuardConfig) *Guard {
	t.Helper()
	g, err := NewGuard(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

func expectClosed(t *testing.T, c net.Conn, what string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := c.Read(make([]byte, 1024))
	if err == nil {
		t.Fatalf("%s: expected connection to be closed, but read succeeded", what)
	}
}

func TestGuard_Passthrough(t *testing.T) {
	backend, stop := fakeBackend(t, true)
	defer stop()
	g := newTestGuard(t, testGuardConfig(backend, &common.Metrics{}))

	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", g.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	frame := logonFrame(30)
	if _, err := c.Write(frame); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, len(frame))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if string(got) != string(frame) {
		t.Fatal("passthrough bytes were altered by the guard")
	}
}

func TestGuard_OversizedFrameDropped(t *testing.T) {
	backend, stop := fakeBackend(t, false)
	defer stop()
	m := &common.Metrics{}
	cfg := testGuardConfig(backend, m)
	cfg.MaxFrameBytes = 64
	g := newTestGuard(t, cfg)

	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", g.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// 200 bytes with no frame terminator: exceeds the 64-byte limit.
	if _, err := c.Write(make([]byte, 200)); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, c, "oversized frame")
	if m.GuardDroppedFrames.Load() != 1 {
		t.Fatalf("GuardDroppedFrames = %d, want 1", m.GuardDroppedFrames.Load())
	}
}

func TestGuard_HeartbeatViolation(t *testing.T) {
	backend, stop := fakeBackend(t, false)
	defer stop()
	m := &common.Metrics{}
	cfg := testGuardConfig(backend, m)
	cfg.LogonTimeout = 10 * time.Second
	g := newTestGuard(t, cfg)

	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", g.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// HeartBtInt=1 is below the 10s minimum: connection must be killed.
	if _, err := c.Write(logonFrame(1)); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, c, "heartbeat violation")
	if m.AuthenticationFails.Load() != 1 {
		t.Fatalf("AuthenticationFails = %d, want 1", m.AuthenticationFails.Load())
	}
}

func TestGuard_LogonTimeout(t *testing.T) {
	backend, stop := fakeBackend(t, false)
	defer stop()
	m := &common.Metrics{}
	cfg := testGuardConfig(backend, m)
	cfg.LogonTimeout = 300 * time.Millisecond
	cfg.IdleTimeout = 10 * time.Second
	g := newTestGuard(t, cfg)

	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", g.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Send nothing: the logon grace period must close us.
	expectClosed(t, c, "logon timeout")
	if m.GuardLogonTimeouts.Load() != 1 {
		t.Fatalf("GuardLogonTimeouts = %d, want 1", m.GuardLogonTimeouts.Load())
	}
}

func TestGuard_IdleTimeout(t *testing.T) {
	backend, stop := fakeBackend(t, false)
	defer stop()
	m := &common.Metrics{}
	cfg := testGuardConfig(backend, m)
	cfg.LogonTimeout = 10 * time.Second
	cfg.IdleTimeout = 300 * time.Millisecond
	g := newTestGuard(t, cfg)

	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", g.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Log on (cancels the logon timer), then go silent.
	if _, err := c.Write(logonFrame(30)); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, c, "idle timeout")
	if m.GuardIdleTimeouts.Load() < 1 {
		t.Fatalf("GuardIdleTimeouts = %d, want >= 1", m.GuardIdleTimeouts.Load())
	}
}

func TestGuard_ConnectionRateLimit(t *testing.T) {
	backend, stop := fakeBackend(t, false)
	defer stop()
	m := &common.Metrics{}
	cfg := testGuardConfig(backend, m)
	cfg.ConnLimiter = NewRateLimiter(2, time.Minute)
	cfg.LogonTimeout = 10 * time.Second
	g := newTestGuard(t, cfg)

	addr := fmt.Sprintf("127.0.0.1:%d", g.Port())
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	// Third attempt within the minute must be refused at the guard.
	c3, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close()
	expectClosed(t, c3, "rate-limited connection")
	if m.GuardRateLimited.Load() != 1 {
		t.Fatalf("GuardRateLimited = %d, want 1", m.GuardRateLimited.Load())
	}
}
