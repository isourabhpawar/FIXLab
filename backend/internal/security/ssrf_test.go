package security

import (
	"net"
	"strings"
	"testing"
	"time"
)

// withPrivate toggles the test-only bypass for the duration of a test.
func withPrivate(t *testing.T, on bool) {
	t.Helper()
	v := "false"
	if on {
		v = "true"
	}
	t.Setenv("FIXLAB_TEST_ALLOW_PRIVATE", v)
}

func TestValidateBlockedLiteralIPs(t *testing.T) {
	withPrivate(t, false)
	blocked := []string{
		"127.0.0.1", "127.1.2.3",
		"10.1.2.3", "10.255.255.255",
		"172.16.0.1", "172.31.255.254",
		"192.168.1.1", "192.168.0.254",
		"169.254.169.254", // cloud metadata
		"169.254.10.20",
		"::1",
		"fc00::1", "fd12:3456::1",
		"fe80::1",
	}
	for _, ip := range blocked {
		_, err := ValidateInitiatorTarget(ip, "9878", 5*time.Second)
		if err == nil {
			t.Errorf("expected rejection for blocked IP %s, got success", ip)
			continue
		}
		if !strings.Contains(err.Error(), "blocked range") {
			t.Errorf("error for %s should name the blocked range, got: %v", ip, err)
		}
	}
}

func TestValidateBlockedHostnames(t *testing.T) {
	withPrivate(t, false)
	blocked := []string{
		"localhost", "LOCALHOST", "localhost.",
		"metadata.google.internal",
		"instance-data",
		"host.docker.internal",
		"kubernetes.default.svc.cluster.local",
		"myservice.default.svc.cluster.local",
	}
	for _, h := range blocked {
		_, err := ValidateInitiatorTarget(h, "9878", 5*time.Second)
		if err == nil {
			t.Errorf("expected rejection for blocked hostname %s, got success", h)
			continue
		}
		if !strings.Contains(err.Error(), "blocked") {
			t.Errorf("error for %s should say blocked, got: %v", h, err)
		}
	}
}

func TestValidateMetadataIP(t *testing.T) {
	withPrivate(t, false)
	_, err := ValidateInitiatorTarget("169.254.169.254", "80", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "blocked range") {
		t.Errorf("expected blocked-range rejection for 169.254.169.254, got: %v", err)
	}
}

func TestValidatePortSyntax(t *testing.T) {
	withPrivate(t, false)
	cases := []struct{ port, want string }{
		{"", "empty"},
		{"abc", "not numeric"},
		{"80x", "not numeric"},
		{"+80", "not numeric"},
		{"0", "out of range"},
		{"-1", "not numeric"},
		{"65536", "out of range"},
	}
	for _, c := range cases {
		_, err := ValidateInitiatorTarget("203.0.113.7", c.port, 5*time.Second)
		if err == nil {
			t.Errorf("port %q: expected rejection, got success", c.port)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("port %q: error should mention %q, got: %v", c.port, c.want, err)
		}
	}
}

func TestValidatePublicLiteralIP(t *testing.T) {
	withPrivate(t, false)
	// TEST-NET-3 is documentation-only and never private — safe for unit tests.
	res, err := ValidateInitiatorTarget("203.0.113.7", "9878", 5*time.Second)
	if err != nil {
		t.Fatalf("expected success for public IP, got: %v", err)
	}
	if res.DialIP != "203.0.113.7" || res.Port != 9878 {
		t.Errorf("unexpected result: %+v", res)
	}
	if len(res.ResolvedIPs) != 1 {
		t.Errorf("expected 1 resolved IP, got %v", res.ResolvedIPs)
	}
}

func TestValidatePrivateAllowedWithOverride(t *testing.T) {
	withPrivate(t, true)
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.1"} {
		res, err := ValidateInitiatorTarget(ip, "9878", 5*time.Second)
		if err != nil {
			t.Errorf("with override, expected success for %s, got: %v", ip, err)
			continue
		}
		if res.DialIP != ip {
			t.Errorf("expected DialIP %s, got %s", ip, res.DialIP)
		}
	}
}

// TestValidateAllResolvedIPsChecked substitutes a fake DNS answer with
// one public and one private IP: the whole hostname must be rejected
// (a single bad answer poisons the host).
func TestValidateAllResolvedIPsChecked(t *testing.T) {
	withPrivate(t, false)
	old := resolveHostFunc
	resolveHostFunc = func(host string, timeout time.Duration) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.7"), net.ParseIP("10.9.9.9")}, nil
	}
	defer func() { resolveHostFunc = old }()

	_, err := ValidateInitiatorTarget("some-broker.example", "9878", 5*time.Second)
	if err == nil {
		t.Fatalf("expected rejection when one resolved IP is private")
	}
	if !strings.Contains(err.Error(), "10.9.9.9") || !strings.Contains(err.Error(), "blocked range") {
		t.Errorf("error should name the bad IP and the blocked range, got: %v", err)
	}
}

// TestValidateHostnameAllPublicPinsFirstIP checks the pinning behavior:
// the dial IP is the first validated address and the full set is kept.
func TestValidateHostnameAllPublicPinsFirstIP(t *testing.T) {
	withPrivate(t, false)
	old := resolveHostFunc
	resolveHostFunc = func(host string, timeout time.Duration) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.7"), net.ParseIP("198.51.100.9")}, nil
	}
	defer func() { resolveHostFunc = old }()

	res, err := ValidateInitiatorTarget("some-broker.example", "9878", 5*time.Second)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if res.DialIP != "203.0.113.7" {
		t.Errorf("expected DialIP pinned to first IP, got %s", res.DialIP)
	}
	if len(res.ResolvedIPs) != 2 {
		t.Errorf("expected both resolved IPs kept, got %v", res.ResolvedIPs)
	}
}

func TestValidateDialIP(t *testing.T) {
	withPrivate(t, false)
	if err := ValidateDialIP("203.0.113.7"); err != nil {
		t.Errorf("expected success for public dial IP, got: %v", err)
	}
	if err := ValidateDialIP("10.0.0.5"); err == nil ||
		!strings.Contains(err.Error(), "blocked range") {
		t.Errorf("expected blocked-range error for 10.0.0.5, got: %v", err)
	}
	if err := ValidateDialIP("not-an-ip"); err == nil {
		t.Errorf("expected error for non-IP dial address")
	}
	withPrivate(t, true)
	if err := ValidateDialIP("127.0.0.1"); err != nil {
		t.Errorf("with override, expected success for 127.0.0.1, got: %v", err)
	}
}

func TestValidateEmptyHost(t *testing.T) {
	withPrivate(t, false)
	if _, err := ValidateInitiatorTarget("   ", "9878", 5*time.Second); err == nil {
		t.Errorf("expected rejection for empty host")
	}
}
