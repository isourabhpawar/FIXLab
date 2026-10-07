package session

import "testing"

func TestParseRawFixValid(t *testing.T) {
	raw := "8=FIX.4.4|9=60|35=0|49=FIXLAB|56=C|34=5|52=20260107-00:00:00|112=PING|10=000|"
	msgType, body, err := parseRawFix(raw)
	if err != nil {
		t.Fatalf("parseRawFix: %v", err)
	}
	if msgType != "0" {
		t.Fatalf("msgType = %q, want 0", msgType)
	}
	if body[112] != "PING" {
		t.Fatalf("body[112] = %q, want PING", body[112])
	}
	// Engine-stamped tags are stripped, never echoed back.
	for _, tag := range []int{8, 9, 10, 34, 49, 52, 56} {
		if _, ok := body[tag]; ok {
			t.Fatalf("stamped tag %d was not stripped", tag)
		}
	}
	if _, ok := body[35]; ok {
		t.Fatalf("tag 35 must not appear in the body map")
	}
}

func TestParseRawFixDelimiters(t *testing.T) {
	soh := "8=FIX.4.4\x019=5\x0135=0\x01112=X\x0110=000\x01"
	caret := "8=FIX.4.4^A9=5^A35=0^A112=X^A10=000^A"
	for name, raw := range map[string]string{"soh": soh, "caret": caret} {
		msgType, body, err := parseRawFix(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if msgType != "0" || body[112] != "X" {
			t.Fatalf("%s: got type %q body %v", name, msgType, body)
		}
	}
}

func TestParseRawFixMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"no-8":        "9=5|35=0|112=X|10=000|",
		"no-10":       "8=FIX.4.4|9=5|35=0|112=X|",
		"no-35":       "8=FIX.4.4|9=5|112=X|10=000|",
		"too-short":   "8=FIX.4.4|10=000|",
		"bad-tag":     "8=FIX.4.4|9=5|35=0|ABC=X|10=000|",
		"bad-kv":      "8=FIX.4.4|9=5|35=0|112|10=000|",
		"10-not-last": "8=FIX.4.4|9=5|35=0|10=000|112=X|",
	}
	for name, raw := range cases {
		if _, _, err := parseRawFix(raw); err == nil {
			t.Fatalf("%s: expected error, got nil", name)
		}
	}
}
