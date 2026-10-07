package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	nhws "nhooyr.io/websocket"

	"fixlab.dev/fixlab/backend/internal/common"
	"fixlab.dev/fixlab/backend/internal/session"
	"fixlab.dev/fixlab/backend/internal/storage"
	"fixlab.dev/fixlab/backend/internal/websocket"
)

func testServer(t *testing.T) (*Server, session.Manager, websocket.Hub) {
	t.Helper()
	cfg := common.LoadConfig()
	cfg.PortPoolMin = 44600
	cfg.PortPoolMax = 44610
	cfg.SessionTTL = time.Hour
	cfg.CleanupInterval = time.Hour // no background reaping in API tests
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := websocket.NewHub()
	mgr, err := session.NewManager(cfg, log, &common.Metrics{}, storage.NewMemoryStore(), hub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Shutdown)
	return NewServer(cfg, log, &common.Metrics{}, mgr, hub), mgr, hub
}

// createTestSession creates a sandbox through the API and returns its token.
func createTestSession(t *testing.T, srv *Server) string {
	t.Helper()
	rec := do(t, srv, "POST", "/api/v1/sessions", `{"targetCompId":"WSTEST"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST status = %d", rec.Code)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	token, _ := created["sessionToken"].(string)
	if token == "" {
		t.Fatal("no sessionToken in create response")
	}
	return token
}

func do(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestAPI_CreateGetDelete(t *testing.T) {
	srv, _, _ := testServer(t)

	rec := do(t, srv, "POST", "/api/v1/sessions", `{"targetCompId":"APITEST"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	token, _ := created["sessionToken"].(string)
	if !strings.HasPrefix(token, "fixlab_") {
		t.Fatalf("sessionToken = %q", token)
	}
	ep, _ := created["endpoint"].(map[string]any)
	if ep["host"] == "" || ep["port"] == nil {
		t.Fatalf("endpoint = %v", ep)
	}
	if ep["tls"] == true {
		t.Fatal("tls should be false")
	}
	ids, _ := created["identifiers"].(map[string]any)
	if ids["beginString"] != "FIX.4.4" || ids["senderCompId"] != "FIXLAB" || ids["targetCompId"] != "APITEST" {
		t.Fatalf("identifiers = %v", ids)
	}
	if _, ok := created["expiresAt"]; !ok {
		t.Fatal("expiresAt missing")
	}

	rec = do(t, srv, "GET", "/api/v1/sessions/"+token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}

	rec = do(t, srv, "DELETE", "/api/v1/sessions/"+token, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d", rec.Code)
	}

	rec = do(t, srv, "GET", "/api/v1/sessions/"+token, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after DELETE status = %d, want 404", rec.Code)
	}
}

func TestAPI_UnknownAndMalformedTokens(t *testing.T) {
	srv, _, _ := testServer(t)
	unknown := "fixlab_" + strings.Repeat("a", 64)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v1/sessions/" + unknown},
		{"DELETE", "/api/v1/sessions/" + unknown},
		{"GET", "/api/v1/sessions/not-a-token"},
		{"DELETE", "/api/v1/sessions/not-a-token"},
	} {
		rec := do(t, srv, tc.method, tc.path, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

func TestAPI_CreateValidation(t *testing.T) {
	srv, _, _ := testServer(t)

	rec := do(t, srv, "POST", "/api/v1/sessions", `{"role":"INITIATOR"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("INITIATOR role status = %d, want 400", rec.Code)
	}
	rec = do(t, srv, "POST", "/api/v1/sessions", `not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON status = %d, want 400", rec.Code)
	}
	// Empty body = defaults: must succeed.
	rec = do(t, srv, "POST", "/api/v1/sessions", ``)
	if rec.Code != http.StatusCreated {
		t.Fatalf("empty body status = %d, want 201", rec.Code)
	}
}

func TestAPI_MetricsAndHealth(t *testing.T) {
	srv, _, _ := testServer(t)
	rec := do(t, srv, "GET", "/api/v1/metrics", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"active_sessions", "fix_messages_in", "fix_messages_out", "session_expirations"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("metric %q missing", k)
		}
	}
	rec = do(t, srv, "GET", "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d", rec.Code)
	}
}

func TestAPI_MessagesEndpoint(t *testing.T) {
	srv, _, _ := testServer(t)
	token := createTestSession(t, srv)

	rec := do(t, srv, "GET", "/api/v1/sessions/"+token+"/messages?limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("messages status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["messages"].([]any); !ok {
		t.Fatalf("messages is not an array: %v", body["messages"])
	}

	unknown := "fixlab_" + strings.Repeat("b", 64)
	rec = do(t, srv, "GET", "/api/v1/sessions/"+unknown+"/messages", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token messages status = %d, want 404", rec.Code)
	}
}

func TestAPI_WebSocketStream(t *testing.T) {
	srv, mgr, hub := testServer(t)
	token := createTestSession(t, srv)

	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/session/" + token

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := nhws.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer c.Close(nhws.StatusNormalClosure, "test done")

	readEvent := func() map[string]any {
		t.Helper()
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var ev map[string]any
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatalf("ws event json: %v", err)
		}
		return ev
	}

	// 1. The server immediately sends the current CONNECTION_STATUS.
	ev := readEvent()
	if ev["type"] != "CONNECTION_STATUS" {
		t.Fatalf("first event type = %v, want CONNECTION_STATUS", ev["type"])
	}
	if ev["sessionId"] != token {
		t.Fatalf("sessionId = %v, want token", ev["sessionId"])
	}
	payload, _ := ev["payload"].(map[string]any)
	if payload["status"] != "WAITING_FOR_CONNECTION" {
		t.Fatalf("snapshot status = %v", payload["status"])
	}

	// 2. A published FIX message event reaches the browser.
	hub.Publish(websocket.Event{
		Type:      websocket.EventFIXMsgIn,
		SessionID: token,
		Timestamp: time.Now(),
		Payload:   map[string]any{"msgType": "0", "msgName": "Heartbeat"},
	})
	ev = readEvent()
	if ev["type"] != "FIX_MSG_IN" {
		t.Fatalf("second event type = %v, want FIX_MSG_IN", ev["type"])
	}

	// 3. Destroying the session delivers SESSION_EXPIRED then closes.
	rec := do(t, srv, "DELETE", "/api/v1/sessions/"+token, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d", rec.Code)
	}
	ev = readEvent()
	if ev["type"] != "SESSION_EXPIRED" {
		t.Fatalf("third event type = %v, want SESSION_EXPIRED", ev["type"])
	}
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("expected ws close after session destroy")
	}

	// 4. Unknown tokens are rejected before the upgrade.
	badURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/session/" + "fixlab_" + strings.Repeat("c", 64)
	if _, _, err := nhws.Dial(ctx, badURL, nil); err == nil {
		t.Fatal("ws dial with unknown token succeeded, want rejection")
	}
	_ = mgr
}
