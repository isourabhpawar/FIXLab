package security

import (
	"strings"
	"testing"
)

func TestGenerateSessionToken_Format(t *testing.T) {
	tok, err := GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, TokenPrefix) {
		t.Fatalf("token %q missing prefix %q", tok, TokenPrefix)
	}
	if !IsValidToken(tok) {
		t.Fatalf("generated token %q failed format validation", tok)
	}
	if len(tok) != len(TokenPrefix)+64 {
		t.Fatalf("expected 256-bit hex token, got length %d", len(tok))
	}
}

func TestGenerateSessionToken_Unique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		tok, err := GenerateSessionToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatal("duplicate token generated")
		}
		seen[tok] = true
	}
}

func TestIsValidToken_RejectsJunk(t *testing.T) {
	bad := []string{
		"",
		"fixlab_",
		"fixlab_xyz",
		"fixlab_" + strings.Repeat("a", 63), // too short
		"fixlab_" + strings.Repeat("a", 65), // too long
		"fixlab_" + strings.Repeat("g", 64), // non-hex
		"FIXLAB_" + strings.Repeat("a", 64), // wrong prefix case
		"session-123",
		"1", // internal-style numeric ID must never validate
	}
	for _, b := range bad {
		if IsValidToken(b) {
			t.Fatalf("IsValidToken(%q) = true, want false", b)
		}
	}
}
