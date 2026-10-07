package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testCustomDictXML = `<fix major="4" minor="4" type="FIX">
<fields>
<field number="9001" name="MyCustomField" type="STRING">
<value enum="A" description="Alpha"/>
<value enum="B" description="Beta"/>
</field>
</fields>
<messages>
<message name="NewOrderSingle" msgtype="D" msgcat="app"/>
</messages>
</fix>`

func uploadDictionary(t *testing.T, srv *Server, token, xml, name string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"xml": xml, "name": name})
	return do(t, srv, "POST", "/api/v1/sessions/"+token+"/dictionary", string(body))
}

func TestDictionary_UploadGetDelete(t *testing.T) {
	srv, _, _ := testServer(t)
	token := createTestSession(t, srv)

	// Upload.
	rec := uploadDictionary(t, srv, token, testCustomDictXML, "acme")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var up map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if up["fields"] != float64(1) || up["messages"] != float64(1) {
		t.Fatalf("upload stats = %v", up)
	}

	// Session GET reflects the override.
	rec = do(t, srv, "GET", "/api/v1/sessions/"+token, "")
	var info map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	dictInfo, _ := info["dictionary"].(map[string]any)
	if dictInfo["custom"] != true || dictInfo["name"] != "acme" {
		t.Fatalf("session dictionary = %v", dictInfo)
	}

	// GET /dictionary.
	rec = do(t, srv, "GET", "/api/v1/sessions/"+token+"/dictionary", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["custom"] != true || got["beginString"] != "FIX.4.4" {
		t.Fatalf("get dictionary = %v", got)
	}

	// DELETE reverts.
	rec = do(t, srv, "DELETE", "/api/v1/sessions/"+token+"/dictionary", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rec.Code)
	}
	rec = do(t, srv, "GET", "/api/v1/sessions/"+token+"/dictionary", "")
	var reverted map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &reverted); err != nil {
		t.Fatal(err)
	}
	if reverted["custom"] != false {
		t.Fatalf("after delete, custom = %v", reverted["custom"])
	}
}

func TestDictionary_UploadValidation(t *testing.T) {
	srv, _, _ := testServer(t)
	token := createTestSession(t, srv)

	cases := []struct {
		name string
		xml  string
		want string
	}{
		{"empty", "", "xml is empty"},
		{"malformed", `<fix major="4"`, "malformed XML"},
		{"not a dict", `<config><x/></config>`, "not a FIX data dictionary"},
		{"no fields", `<fix major="4" minor="4"><fields></fields></fix>`, "definitions found in"},
		{"too large", strings.Repeat("x", 512*1024+1), "too large"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := uploadDictionary(t, srv, token, c.xml, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), c.want) {
				t.Fatalf("body %q does not name %q", rec.Body.String(), c.want)
			}
		})
	}

	// Unknown token → 404.
	rec := uploadDictionary(t, srv, "fixlab_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef00", testCustomDictXML, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token status = %d, want 404", rec.Code)
	}
}

// buildRawFIX renders fields with a correct 9= and 10= for decode tests.
func buildRawFIX(t *testing.T, bodyFields string) string {
	t.Helper()
	body := bodyFields
	bl := len(body)
	sum := 0
	head := fmt.Sprintf("8=FIX.4.4\x019=%d\x01", bl)
	for i := 0; i < len(head); i++ {
		sum += int(head[i])
	}
	for i := 0; i < len(body); i++ {
		sum += int(body[i])
	}
	return head + body + fmt.Sprintf("10=%03d\x01", sum%256)
}

func TestDictionary_DecodeWithToken(t *testing.T) {
	srv, _, _ := testServer(t)
	token := createTestSession(t, srv)

	raw := buildRawFIX(t, "35=D\x0149=C1\x0156=FIXLAB\x0134=2\x0152=20260107-05:30:00\x0111=O1\x0155=MSFT\x0154=1\x0138=100\x0140=2\x0144=310.50\x019001=B\x01")

	decode := func(tok string) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"rawFix": strings.ReplaceAll(raw, "\x01", "|"), "token": tok})
		rec := do(t, srv, "POST", "/api/v1/tools/decode", string(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("decode status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}

	fieldByTag := func(res map[string]any, tag float64) map[string]any {
		t.Helper()
		fields, _ := res["fields"].([]any)
		for _, f := range fields {
			m, _ := f.(map[string]any)
			if m["tag"] == tag {
				return m
			}
		}
		t.Fatalf("tag %v not in decoded fields", tag)
		return nil
	}

	// Without the custom dictionary, 9001 is Unknown.
	res := decode("")
	if f := fieldByTag(res, 9001); f["name"] != "Unknown" {
		t.Fatalf("without override, 9001 name = %v", f["name"])
	}

	// Upload, then decode with the token: the custom name resolves.
	if rec := uploadDictionary(t, srv, token, testCustomDictXML, "acme"); rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d", rec.Code)
	}
	res = decode(token)
	if f := fieldByTag(res, 9001); f["name"] != "MyCustomField" || f["enumDescription"] != "Beta" {
		t.Fatalf("with override, 9001 = %v", f)
	}

	// Unknown token → 404.
	body, _ := json.Marshal(map[string]string{"rawFix": "8=FIX.4.4|9=5|35=0|10=000|", "token": "fixlab_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef00"})
	rec := do(t, srv, "POST", "/api/v1/tools/decode", string(body))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token decode status = %d, want 404", rec.Code)
	}
}
