// Package security — SSRF protection for FixLab initiator sessions
// (spec §9, phase 5).
//
// Initiator mode dials OUT to a developer-supplied host:port, which is
// textbook SSRF surface. Every outbound target is validated here BEFORE
// any socket is opened, and the validation result pins the exact IP
// address the engine dials:
//
//   - literal IPs and every DNS-resolved IP are checked against the
//     blocked ranges (loopback, RFC 1918, link-local, metadata,
//     IPv6 unique-local/link-local/loopback);
//   - "localhost" and the well-known cloud metadata / container-internal
//     hostnames are rejected by name;
//   - DNS is resolved once at validation time with a timeout; the engine
//     dials the PINNED IP, never the hostname again, so DNS rebinding
//     between validation and dial (and on QuickFIX/Go reconnects) is
//     structurally impossible — there is no second resolution.
//   - StartInitiator re-validates the pinned dial IP (defense in depth),
//     so the connect path never trusts an unvalidated address.
//
// Test-only escape hatch: FIXLAB_TEST_ALLOW_PRIVATE=true bypasses the
// private-range blocking so the acceptance suite can dial a 127.0.0.1
// test counterparty. It is OFF by default and must never be enabled in
// production (the e2e script and README say so explicitly).
package security

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// blockedRanges are the IP ranges an initiator session may never dial
// (spec §9). Checked against both literal host IPs and every IP a
// hostname resolves to.
var blockedRanges = mustParseCIDRs([]string{
	"127.0.0.0/8",   // IPv4 loopback
	"10.0.0.0/8",    // RFC 1918
	"172.16.0.0/12", // RFC 1918
	"192.168.0.0/16", // RFC 1918
	"169.254.0.0/16", // link-local incl. cloud metadata
	"::1/128",        // IPv6 loopback
	"fc00::/7",       // IPv6 unique-local
	"fe80::/10",      // IPv6 link-local
})

func mustParseCIDRs(cidrs []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("security: bad blocked CIDR " + c + ": " + err.Error())
		}
		out = append(out, n)
	}
	return out
}

// blockedRangeOf returns the blocked range containing ip, or nil.
func blockedRangeOf(ip net.IP) *net.IPNet {
	for _, n := range blockedRanges {
		if n.Contains(ip) {
			return n
		}
	}
	return nil
}

// blockedHostnames are rejected by name before DNS is even attempted
// (spec §9: localhost, cloud metadata endpoints, container-internal
// names). Matching is case-insensitive; a trailing dot is tolerated.
var blockedHostnames = []string{
	"localhost",
	// AWS / Azure / GCP metadata (IP 169.254.169.254 is already covered
	// by the link-local range; the names are belt and braces).
	"instance-data",
	"instance-data.compute.internal",
	"metadata.google.internal",
	"metadata.google.com",
	// Docker / Kubernetes internals.
	"host.docker.internal",
	"kubernetes.default",
	"kubernetes.default.svc",
	"kubernetes.default.svc.cluster.local",
}

func normalizeHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

func isBlockedHostname(host string) (string, bool) {
	n := normalizeHost(host)
	for _, b := range blockedHostnames {
		if n == b {
			return b, true
		}
	}
	// Any *.svc.cluster.local name is Kubernetes-internal.
	if strings.HasSuffix(n, ".svc.cluster.local") {
		return "*.svc.cluster.local", true
	}
	return "", false
}

// TestAllowPrivate reports whether the test-only private-range bypass is
// enabled. It is read from the environment on every call (no caching) so
// tests can flip it with t.Setenv and the semantics stay obvious: the
// bypass applies at validation time only.
func TestAllowPrivate() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("FIXLAB_TEST_ALLOW_PRIVATE")))
	return v == "1" || v == "true" || v == "yes"
}

// ValidationResult is the outcome of a successful SSRF check. DialIP is
// the single pinned IP the engine must dial; ResolvedIPs is the full
// validated set (kept for diagnostics and for the connect-path
// re-validation).
type ValidationResult struct {
	// Hostname is the original host string (for display).
	Hostname string
	// Port is the validated port.
	Port int
	// DialIP is the pinned IP address to dial (string form).
	DialIP string
	// ResolvedIPs are all IPs the hostname resolved to at validation
	// time; every one of them passed the blocked-range check.
	ResolvedIPs []string
}

// ValidateInitiatorTarget performs the full SSRF check for an outbound
// initiator dial (spec §9): port syntax/range, hostname blocklist, then
// DNS resolution with every returned IP checked against the blocked
// ranges. On success it returns the pinned dial IP. Errors name the
// exact violation.
func ValidateInitiatorTarget(host, portStr string, dnsTimeout time.Duration) (*ValidationResult, error) {
	port, err := parsePort(portStr)
	if err != nil {
		return nil, err
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, fmt.Errorf("SSRF: remote host is empty")
	}

	// 1. Hostname blocklist (before DNS — no resolution of these names).
	if name, blocked := isBlockedHostname(host); blocked {
		return nil, fmt.Errorf("SSRF: hostname %q is blocked (%s)", host, name)
	}

	allowPrivate := TestAllowPrivate()

	// 2. Literal IP: check it directly, no DNS involved.
	if ip := net.ParseIP(host); ip != nil {
		if !allowPrivate {
			if n := blockedRangeOf(ip); n != nil {
				return nil, fmt.Errorf("SSRF: host %q is in blocked range %s", host, n.String())
			}
		}
		return &ValidationResult{
			Hostname:    host,
			Port:        port,
			DialIP:      ip.String(),
			ResolvedIPs: []string{ip.String()},
		}, nil
	}

	// 3. Hostname: resolve with a timeout, then check EVERY returned IP.
	// A single bad IP fails the whole validation — an attacker who
	// controls one DNS answer must not get a dial.
	ips, err := resolveHostFunc(host, dnsTimeout)
	if err != nil {
		return nil, fmt.Errorf("SSRF: DNS resolution of %q failed: %v", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("SSRF: hostname %q resolved to no IP addresses", host)
	}
	resolved := make([]string, 0, len(ips))
	for _, ip := range ips {
		if !allowPrivate {
			if n := blockedRangeOf(ip); n != nil {
				return nil, fmt.Errorf("SSRF: resolved IP %s for host %q is in blocked range %s", ip.String(), host, n.String())
			}
		}
		resolved = append(resolved, ip.String())
	}
	// Pin the first validated IP for the dial. The engine dials this IP
	// and never re-resolves the hostname, which defeats DNS rebinding
	// (including on QuickFIX/Go reconnects).
	return &ValidationResult{
		Hostname:    host,
		Port:        port,
		DialIP:      resolved[0],
		ResolvedIPs: resolved,
	}, nil
}

// ValidateDialIP re-validates an already-pinned dial IP at connect time
// (defense in depth): no DNS, just the range check. The engine calls
// this on the pinned IP from ValidateInitiatorTarget so the connect path
// never dials an address that skipped validation.
func ValidateDialIP(ipStr string) error {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return fmt.Errorf("SSRF: pinned dial address %q is not a valid IP", ipStr)
	}
	if !TestAllowPrivate() {
		if n := blockedRangeOf(ip); n != nil {
			return fmt.Errorf("SSRF: pinned dial IP %s is in blocked range %s", ip.String(), n.String())
		}
	}
	return nil
}

// parsePort validates the TCP port syntax and range (1–65535).
func parsePort(portStr string) (int, error) {
	s := strings.TrimSpace(portStr)
	if s == "" {
		return 0, fmt.Errorf("SSRF: remote port is empty")
	}
	// Reject non-numeric input explicitly (strconv.Atoi would accept
	// "+80" and surrounding spaces; be strict).
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("SSRF: remote port %q is not numeric", portStr)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("SSRF: remote port %q is not numeric", portStr)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("SSRF: remote port %d out of range (1-65535)", n)
	}
	return n, nil
}

// resolveHostFunc is the DNS lookup used by validation. It is a variable
// (not a plain function) so unit tests can substitute a fake answer and
// exercise the multi-IP validation loop without DNS control.
var resolveHostFunc = resolveHost

// resolveHost resolves a hostname to IPs with a deadline. The default
// resolver is used (it honors /etc/hosts, which the test suite relies
// on); only the timeout is enforced here.
func resolveHost(host string, timeout time.Duration) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}
