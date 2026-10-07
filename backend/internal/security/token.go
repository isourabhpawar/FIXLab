// Package security implements FixLab's defense layers: unguessable
// session tokens, per-IP rate limiting, and the TCP guard proxy that sits
// in front of every QuickFIX/Go acceptor (frame-size enforcement, idle
// and logon timeouts, connection rate limiting, minimum heartbeat guard).
package security

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
)

// TokenPrefix prefixes every session token (spec §6).
const TokenPrefix = "fixlab_"

// tokenEntropyBytes is 256 bits of entropy per token.
const tokenEntropyBytes = 32

var tokenPattern = regexp.MustCompile(`^fixlab_[0-9a-f]{64}$`)

// GenerateSessionToken returns a cryptographically secure, unguessable
// session token. The token is the sole bearer credential for the sandbox
// and must be treated as a secret (spec §6). No internal database IDs are
// ever exposed; the token is the only identifier.
func GenerateSessionToken() (string, error) {
	b := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("security: entropy source failed: %w", err)
	}
	return TokenPrefix + hex.EncodeToString(b), nil
}

// IsValidToken reports whether s looks like a token we issued. This is a
// cheap format gate; authorization is the store lookup itself.
func IsValidToken(s string) bool { return tokenPattern.MatchString(s) }
