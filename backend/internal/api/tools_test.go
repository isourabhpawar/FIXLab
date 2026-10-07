package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// buildFIX assembles a well-formed FIX message (correct 9= and 10=) from
// body fields, SOH-delimited. The caller supplies 35/49/56/… — 8=, 9=
// and 10= are stamped here.
func buildFIX(t *testing.T, beginString string, body [][2]string) string {
	t.Helper()
	var bodyBuf strings.Builder
	for _, f := range body {
		fmt.Fprintf(&bodyBuf, "%s=%s\x01", f[0], f[1])
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "8=%s\x01", beginString)
	fmt.Fprintf(&msg, "9=%d\x01%s", bodyBuf.Len(), bodyBuf.String())
	sum := 0
	for i := 0; i < msg.Len(); i++ {
		sum += int(msg.String()[i])
	}
	fmt.Fprintf(&msg, "10=%03d\x01", sum%256)
	return msg.String()
}

func sampleNOS(t *testing.T) string {
	t.Helper()
	return buildFIX(t, "FIX.4.4", [][2]string{
		{"35", "D"}, {"49", "TESTBUY"}, {"56", "FIXLAB"}, {"34", "12"},
		{"52", "20260107-05:30:00.000"}, {"11", "ORD123"}, {"55", "MSFT"},
		{"54", "1"}, {"38", "100"}, {"40", "2"}, {"44", "310.50"},
	})
}

type decodeResp struct {
	Valid              bool   `json:"valid"`
	MsgType            string `json:"msgType"`
	MsgName            string `json:"msgName"`
	ChecksumOK         bool   `json:"checksumOk"`
	ExpectedChecksum   string `json:"expectedChecksum"`
	ActualChecksum     string `json:"actualChecksum"`
	BodyLengthOK       bool   `json:"bodyLengthOk"`
	ExpectedBodyLength int    `json:"expectedBodyLength"`
	ActualBodyLength   int    `json:"actualBodyLength"`
	Fields             []struct {
		Tag             int    `json:"tag"`
		Name            string `json:"name"`
		Value           string `json:"value"`
		EnumDescription string `json:"enumDescription"`
	} `json:"fields"`
}

func postDecode(t *testing.T, srv *Server, rawFix, beginString string) (int, []byte) {
	t.Helper()
	// json.Marshal, not %q: %q emits \x01 escapes, which are not valid
	// JSON — SOH must travel as \u0001.
	raw, err := json.Marshal(map[string]string{"rawFix": rawFix, "beginString": beginString})
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, srv, "POST", "/api/v1/tools/decode", string(raw))
	return rec.Code, rec.Body.Bytes()
}

func TestDecode_ValidNOS(t *testing.T) {
	srv, _, _ := testServer(t)
	code, data := postDecode(t, srv, sampleNOS(t), "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, data)
	}
	var res decodeResp
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Valid || !res.ChecksumOK || !res.BodyLengthOK {
		t.Fatalf("expected valid decode, got %+v", res)
	}
	if res.MsgType != "D" || res.MsgName != "NewOrderSingle" {
		t.Fatalf("msgType/msgName = %q/%q", res.MsgType, res.MsgName)
	}
	byTag := map[int]string{}
	enumByTag := map[int]string{}
	for _, f := range res.Fields {
		byTag[f.Tag] = f.Value
		enumByTag[f.Tag] = f.EnumDescription
	}
	if byTag[55] != "MSFT" {
		t.Fatalf("tag 55 = %q", byTag[55])
	}
	if enumByTag[54] != "BUY" {
		t.Fatalf("tag 54 enumDescription = %q, want BUY (dictionary verbatim)", enumByTag[54])
	}
	if enumByTag[40] == "" {
		t.Fatal("tag 40 should carry an enum description (Limit)")
	}
}

func TestDecode_DelimiterStyles(t *testing.T) {
	srv, _, _ := testServer(t)
	soh := sampleNOS(t)
	pipe := strings.ReplaceAll(soh, "\x01", "|")
	caret := strings.ReplaceAll(soh, "\x01", "^A")
	for name, raw := range map[string]string{"pipe": pipe, "caret-A": caret} {
		code, data := postDecode(t, srv, raw, "")
		if code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", name, code, data)
		}
		var res decodeResp
		if err := json.Unmarshal(data, &res); err != nil {
			t.Fatal(err)
		}
		if !res.Valid {
			t.Fatalf("%s: expected valid decode, got %+v", name, res)
		}
	}
}

func TestDecode_TamperedChecksum(t *testing.T) {
	srv, _, _ := testServer(t)
	raw := sampleNOS(t)
	// Tamper one body character (MSFT → MXFT); the 10= stays as sent.
	tampered := strings.Replace(raw, "MSFT", "MXFT", 1)
	code, data := postDecode(t, srv, tampered, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, data)
	}
	var res decodeResp
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if res.Valid {
		t.Fatal("tampered message must not be valid")
	}
	if res.ChecksumOK {
		t.Fatal("checksumOk must be false after tampering")
	}
	if res.ExpectedChecksum == res.ActualChecksum {
		t.Fatal("expected and actual checksums must differ after tampering")
	}
	if len(res.ExpectedChecksum) != 3 {
		t.Fatalf("expectedChecksum = %q, want 3 digits", res.ExpectedChecksum)
	}
	if !res.BodyLengthOK {
		t.Fatal("body length is unchanged by a value tamper; bodyLengthOk must stay true")
	}
}

func TestDecode_WrongBodyLength(t *testing.T) {
	srv, _, _ := testServer(t)
	raw := sampleNOS(t)
	// Corrupt the 9= value only (leave 10= as sent): the body-length
	// verdict must flip while the checksum verdict reflects the edit.
	fields := strings.Split(strings.TrimSuffix(raw, "\x01"), "\x01")
	fields[1] = "9=5"
	corrupt := strings.Join(fields, "\x01") + "\x01"
	code, data := postDecode(t, srv, corrupt, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, data)
	}
	var res decodeResp
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if res.BodyLengthOK {
		t.Fatal("bodyLengthOk must be false with a wrong 9=")
	}
	if res.ActualBodyLength != 5 {
		t.Fatalf("actualBodyLength = %d, want 5", res.ActualBodyLength)
	}
	if res.ExpectedBodyLength == 5 || res.ExpectedBodyLength <= 0 {
		t.Fatalf("expectedBodyLength = %d, want the real byte count", res.ExpectedBodyLength)
	}
}

func TestDecode_MissingChecksum(t *testing.T) {
	srv, _, _ := testServer(t)
	raw := sampleNOS(t)
	// Drop the trailing 10= field entirely.
	cut := raw[:strings.LastIndex(raw, "10=")]
	code, data := postDecode(t, srv, cut, "")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	var errBody map[string]string
	if err := json.Unmarshal(data, &errBody); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errBody["error"], "10=CheckSum") {
		t.Fatalf("error %q must name 10=CheckSum", errBody["error"])
	}
}

func TestDecode_MissingBeginString(t *testing.T) {
	srv, _, _ := testServer(t)
	code, data := postDecode(t, srv, "9=5|35=0|10=000|", "")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	var errBody map[string]string
	_ = json.Unmarshal(data, &errBody)
	if !strings.Contains(errBody["error"], "8=BeginString") {
		t.Fatalf("error %q must name 8=BeginString", errBody["error"])
	}
}

func TestDecode_MalformedField(t *testing.T) {
	srv, _, _ := testServer(t)
	code, data := postDecode(t, srv, "8=FIX.4.4|9=5|35|10=000|", "")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", code, data)
	}
	var errBody map[string]string
	_ = json.Unmarshal(data, &errBody)
	if !strings.Contains(errBody["error"], "tag=value") {
		t.Fatalf("error %q must mention tag=value", errBody["error"])
	}
}

func TestDecode_Empty(t *testing.T) {
	srv, _, _ := testServer(t)
	code, _ := postDecode(t, srv, "   \n ", "")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestDecode_UnknownTags(t *testing.T) {
	srv, _, _ := testServer(t)
	raw := buildFIX(t, "FIX.4.4", [][2]string{
		{"35", "0"}, {"99999", "custom"}, {"88888", "x"},
	})
	code, data := postDecode(t, srv, raw, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, data)
	}
	var res decodeResp
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Valid {
		t.Fatalf("unknown tags must not break the decode: %+v", res)
	}
	for _, f := range res.Fields {
		if f.Tag == 99999 && f.Name != "Unknown" {
			t.Fatalf("tag 99999 name = %q, want Unknown", f.Name)
		}
		if f.Tag == 88888 && f.Name != "Unknown" {
			t.Fatalf("tag 88888 name = %q, want Unknown", f.Name)
		}
	}
}

func TestDecode_BeginStringOverride(t *testing.T) {
	srv, _, _ := testServer(t)
	// A FIX.4.4 message decoded against the FIX.4.2 dictionary on request.
	code, data := postDecode(t, srv, sampleNOS(t), "FIX.4.2")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, data)
	}
	var res decodeResp
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Valid {
		t.Fatalf("override decode must stay valid: %+v", res)
	}
	if res.MsgName != "NewOrderSingle" {
		t.Fatalf("msgName = %q", res.MsgName)
	}
}

func TestDecode_UnsupportedBeginString(t *testing.T) {
	srv, _, _ := testServer(t)
	code, data := postDecode(t, srv, sampleNOS(t), "FIX.9.9")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	var errBody map[string]string
	_ = json.Unmarshal(data, &errBody)
	if !strings.Contains(errBody["error"], "unsupported BeginString") {
		t.Fatalf("error %q must name the unsupported BeginString", errBody["error"])
	}
}

func TestDecode_BadJSON(t *testing.T) {
	srv, _, _ := testServer(t)
	rec := do(t, srv, "POST", "/api/v1/tools/decode", "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDecode_WrongMethod(t *testing.T) {
	srv, _, _ := testServer(t)
	rec := do(t, srv, "GET", "/api/v1/tools/decode", "")
	if rec.Code == http.StatusOK {
		t.Fatal("GET must not decode")
	}
}
