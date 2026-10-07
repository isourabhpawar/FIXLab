package security

import (
	"crypto/tls"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"fixlab.dev/fixlab/backend/internal/common"
)

// testTLSCert returns a self-signed serving certificate for tests.
func testTLSCert(t *testing.T) tls.Certificate {
	t.Helper()
	cert, _, err := GenerateSelfSigned("localhost")
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func tlsGuardConfig(backend string, m *common.Metrics, cert tls.Certificate) GuardConfig {
	cfg := testGuardConfig(backend, m)
	cfg.TLSConfig = ServerTLSConfig(cert)
	return cfg
}

func tlsDial(t *testing.T, addr string, min, max uint16) *tls.Conn {
	t.Helper()
	c, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true, // test-only: self-signed test cert
		MinVersion:         min,
		MaxVersion:         max,
	})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	return c
}

// TestGuard_TLS_Passthrough proves the edge-termination path: a TLS
// client handshakes, sends a FIX logon, and gets the decrypted bytes
// echoed back from the plaintext loopback backend.
func TestGuard_TLS_Passthrough(t *testing.T) {
	backend, stop := fakeBackend(t, true)
	defer stop()
	m := &common.Metrics{}
	g := newTestGuard(t, tlsGuardConfig(backend, m, testTLSCert(t)))
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(g.Port()))

	c := tlsDial(t, addr, tls.VersionTLS12, tls.VersionTLS13)
	defer c.Close()
	if v := c.ConnectionState().Version; v < tls.VersionTLS12 {
		t.Fatalf("negotiated TLS version %x, want >= 1.2", v)
	}
	frame := logonFrame(30)
	if _, err := c.Write(frame); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, len(frame))
	n := 0
	for n < len(frame) {
		k, err := c.Read(got[n:])
		if err != nil {
			t.Fatalf("echo read: %v", err)
		}
		n += k
	}
	if string(got) != string(frame) {
		t.Fatal("TLS passthrough bytes were altered by the guard")
	}
}

// TestGuard_TLS_OldVersionRefused proves MinVersion TLS 1.2 is
// enforced at the edge.
func TestGuard_TLS_OldVersionRefused(t *testing.T) {
	backend, stop := fakeBackend(t, false)
	defer stop()
	m := &common.Metrics{}
	g := newTestGuard(t, tlsGuardConfig(backend, m, testTLSCert(t)))
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(g.Port()))

	_, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS10,
		MaxVersion:         tls.VersionTLS11,
	})
	if err == nil {
		t.Fatal("TLS 1.0/1.1 handshake should have been refused")
	}
	// The server side records the failed handshake.
	deadline := time.Now().Add(3 * time.Second)
	for m.TLSHandshakeFails.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if m.TLSHandshakeFails.Load() == 0 {
		t.Fatal("TLSHandshakeFails was not recorded")
	}
}

// TestGuard_TLS_PlaintextRejected proves protocol confusion is
// impossible: raw FIX bytes sent to a TLS port fail the handshake and
// the connection is dropped before reaching the FIX engine.
func TestGuard_TLS_PlaintextRejected(t *testing.T) {
	backend, stop := fakeBackend(t, false)
	defer stop()
	m := &common.Metrics{}
	g := newTestGuard(t, tlsGuardConfig(backend, m, testTLSCert(t)))
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(g.Port()))

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(logonFrame(30)); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, c, "plaintext to TLS port")
	if m.TLSHandshakeFails.Load() == 0 {
		t.Fatal("TLSHandshakeFails was not recorded for plaintext input")
	}
}

// TestGuard_TLS_OversizedFrameDropped proves the guard's byte-level
// protections still apply to the DECRYPTED stream.
func TestGuard_TLS_OversizedFrameDropped(t *testing.T) {
	backend, stop := fakeBackend(t, false)
	defer stop()
	m := &common.Metrics{}
	cfg := tlsGuardConfig(backend, m, testTLSCert(t))
	cfg.MaxFrameBytes = 64
	g := newTestGuard(t, cfg)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(g.Port()))

	c := tlsDial(t, addr, tls.VersionTLS12, tls.VersionTLS13)
	defer c.Close()
	if _, err := c.Write(make([]byte, 200)); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, c, "oversized frame over TLS")
	if m.GuardDroppedFrames.Load() != 1 {
		t.Fatalf("GuardDroppedFrames = %d, want 1", m.GuardDroppedFrames.Load())
	}
}

// TestSetupServerTLS_SelfSigned covers the dev fallback: no env cert
// configured → generated self-signed identity with a fingerprint.
func TestSetupServerTLS_SelfSigned(t *testing.T) {
	setup := SetupServerTLS("", "", "127.0.0.1", nil)
	if setup.Err != nil {
		t.Fatalf("self-signed setup: %v", setup.Err)
	}
	if setup.Config == nil {
		t.Fatal("expected a TLS config")
	}
	if setup.Config.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS 1.2", setup.Config.MinVersion)
	}
	if !setup.SelfSigned {
		t.Fatal("expected SelfSigned")
	}
	if len(setup.Fingerprint) != 64 {
		t.Fatalf("fingerprint = %q, want 64 hex chars", setup.Fingerprint)
	}
}

// TestSetupServerTLS_BrokenEnv covers the 400 path: env paths set but
// unloadable → recorded error, nil config, server still starts.
func TestSetupServerTLS_BrokenEnv(t *testing.T) {
	setup := SetupServerTLS("/nonexistent/cert.pem", "/nonexistent/key.pem", "", nil)
	if setup.Err == nil {
		t.Fatal("expected an error for missing cert files")
	}
	if setup.Config != nil {
		t.Fatal("config must be nil when the cert is unusable")
	}
	if !strings.Contains(setup.Err.Error(), "load TLS key pair") {
		t.Fatalf("error should name the load problem, got: %v", setup.Err)
	}
}

// TestSetupServerTLS_EnvCert covers loading a real key pair from files.
func TestSetupServerTLS_EnvCert(t *testing.T) {
	cert, _, err := GenerateSelfSigned("localhost")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := CertToPEM(cert)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cf := dir + "/cert.pem"
	kf := dir + "/key.pem"
	if err := os.WriteFile(cf, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kf, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	setup := SetupServerTLS(cf, kf, "", nil)
	if setup.Err != nil {
		t.Fatalf("env cert setup: %v", setup.Err)
	}
	if setup.SelfSigned {
		t.Fatal("env cert must not be marked self-signed")
	}
	if len(setup.Fingerprint) != 64 {
		t.Fatalf("fingerprint = %q", setup.Fingerprint)
	}
}
