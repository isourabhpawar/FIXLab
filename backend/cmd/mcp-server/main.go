// Command mcp-server exposes FixLab as an MCP server over stdio
// (spec §49: Claude / Cursor → MCP Server → FixLab API →
// Session Manager → QuickFIX/Go).
//
// It is a THIN WRAPPER over the HTTP REST API: it never imports backend
// internals and can run against any reachable FixLab deployment. The API
// base URL comes from FIXLAB_API_URL (default http://127.0.0.1:8080).
//
// The six tools (spec §49):
//
//	sandbox_create        create a FIX sandbox (ACCEPTOR or INITIATOR)
//	sandbox_get_status    role, connection status, orders, TTL, policy state
//	sandbox_list_messages message history (spec §18 records)
//	sandbox_send_order    inject 35=D/F/G (INITIATOR sessions only)
//	sandbox_send_execution
//	                      FILL/PARTIAL_FILL/REJECT/CANCEL_*/REPLACE_* (ACCEPTOR)
//	sandbox_send_raw      power tool: send one raw FIX message; the engine
//	                      still stamps 8/9/10/34/49/52/56, so truly raw byte
//	                      injection is NOT possible.
//	sandbox_get_scenario  recorded scenario JSON (phase 2.3)
//	sandbox_replay        replay a scenario into a fresh sandbox (phase 2.3)
//
// Session tokens are bearer secrets: they are required on every tool call
// (the server is stateless) and are never written to logs.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const serverVersion = "1.0.0"

// apiBase is read once at startup from FIXLAB_API_URL.
var apiBase = "http://127.0.0.1:8080"

var httpClient = &http.Client{Timeout: 30 * time.Second}

// apiError is the {"error": "..."} envelope the REST API returns.
type apiError struct {
	Error string `json:"error"`
}

// callAPI performs one REST call and returns the decoded JSON body.
// Non-2xx responses return an error carrying the API's message.
func callAPI(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("FixLab API unreachable at %s: %w", apiBase, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var ae apiError
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &ae) == nil && ae.Error != "" {
			msg = ae.Error
		}
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("FixLab API %d: %s", resp.StatusCode, msg)
	}
	return json.RawMessage(raw), nil
}

// toolErr reports a tool-level failure inside the result content with
// IsError set (per the SDK's guidance: the LLM must see the error and
// be able to self-correct), never as a protocol-level error.
func toolErr(format string, args ...any) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}, nil, nil
}

// ok returns a successful tool result whose content is the pretty-printed
// JSON of v (and whose structured content is v itself).
func ok(v any) (*mcp.CallToolResult, any, error) {
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toolErr("encode result: %v", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(pretty)}},
	}, v, nil
}

// ---------- sandbox_create ----------

type createInput struct {
	Role             string `json:"role"` // ACCEPTOR | INITIATOR (required)
	BeginString      string `json:"beginString,omitempty"`
	SenderCompID     string `json:"senderCompId,omitempty"`
	TargetCompID     string `json:"targetCompId,omitempty"`
	TLS              bool   `json:"tls,omitempty"`
	RemoteHost       string `json:"remoteHost,omitempty"`         // INITIATOR only
	RemotePort       string `json:"remotePort,omitempty"`         // INITIATOR only
	RemoteTargetComp string `json:"remoteTargetCompId,omitempty"` // INITIATOR only
}

func handleCreate(ctx context.Context, _ *mcp.CallToolRequest, in createInput) (*mcp.CallToolResult, any, error) {
	role := strings.ToUpper(strings.TrimSpace(in.Role))
	if role != "ACCEPTOR" && role != "INITIATOR" {
		return toolErr("role must be ACCEPTOR or INITIATOR, got %q", in.Role)
	}
	body := map[string]any{"role": role}
	if in.BeginString != "" {
		body["fixVersion"] = in.BeginString
	}
	if in.TLS {
		body["tls"] = true
	}
	if role == "ACCEPTOR" {
		if in.TargetCompID != "" {
			body["targetCompId"] = in.TargetCompID
		}
		if in.SenderCompID != "" {
			body["senderCompId"] = in.SenderCompID
		}
	} else {
		if in.RemoteHost == "" || in.RemotePort == "" || in.RemoteTargetComp == "" {
			return toolErr("INITIATOR requires remoteHost, remotePort and remoteTargetCompId")
		}
		body["remoteHost"] = in.RemoteHost
		body["remotePort"] = in.RemotePort
		body["remoteCompId"] = in.RemoteTargetComp
		if in.SenderCompID != "" {
			body["localSenderCompId"] = in.SenderCompID
		}
	}
	raw, err := callAPI(ctx, http.MethodPost, "/api/v1/sessions", body)
	if err != nil {
		return toolErr("%v", err)
	}
	var created map[string]any
	if err := json.Unmarshal(raw, &created); err != nil {
		return toolErr("decode create response: %v", err)
	}
	created["connect_howto"] = connectHowto(role, created)
	// Tokens are never logged: only the role and that creation happened.
	slog.Info("mcp: sandbox created", "role", role)
	return ok(created)
}

// connectHowto renders the human-readable connection summary (spec §49:
// sandbox_create returns how to connect).
func connectHowto(role string, created map[string]any) string {
	ep, _ := created["endpoint"].(map[string]any)
	id, _ := created["identifiers"].(map[string]any)
	host, _ := ep["host"].(string)
	port := ep["port"]
	tls, _ := ep["tls"].(bool)
	transport := "TCP"
	if tls {
		transport = "TLS"
	}
	if role == "ACCEPTOR" {
		return fmt.Sprintf(
			"Point your FIX engine at %s %s:%v with BeginString=%v, SenderCompID=<yours>, TargetCompID=%v. "+
				"Watch messages live at GET /api/v1/sessions/<token>/messages or the /ws/session/<token> stream. "+
				"The token is the bearer credential — keep it secret; the sandbox expires automatically.",
			transport, host, port, id["beginString"], id["senderCompId"])
	}
	return fmt.Sprintf(
		"FixLab is dialling your FIX acceptor at %s:%v over %s (SSRF-validated, IP-pinned). "+
			"Watch the CONNECTING → TCP_CONNECTED → LOGON_SENT → LOGON_ACCEPTED chain via sandbox_get_status, "+
			"then inject orders with sandbox_send_order.",
		host, port, transport)
}

// ---------- sandbox_get_status ----------

type tokenInput struct {
	Token string `json:"token"`
}

func requireToken(in tokenInput) (string, *mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Token) == "" {
		r, o, e := toolErr("token is required (the fixlab_… bearer credential)")
		return "", r, o, e
	}
	return in.Token, nil, nil, nil
}

func handleGetStatus(ctx context.Context, _ *mcp.CallToolRequest, in tokenInput) (*mcp.CallToolResult, any, error) {
	token, r, o, e := requireToken(in)
	if r != nil || e != nil {
		return r, o, e
	}
	raw, err := callAPI(ctx, http.MethodGet, "/api/v1/sessions/"+token, nil)
	if err != nil {
		return toolErr("%v", err)
	}
	var info map[string]any
	if err := json.Unmarshal(raw, &info); err != nil {
		return toolErr("decode status: %v", err)
	}
	// Merge order counts from the blotter.
	ordersRaw, err := callAPI(ctx, http.MethodGet, "/api/v1/sessions/"+token+"/orders", nil)
	if err != nil {
		return toolErr("%v", err)
	}
	var ordersDoc struct {
		Orders []struct {
			Status string `json:"status"`
		} `json:"orders"`
	}
	if err := json.Unmarshal(ordersRaw, &ordersDoc); err != nil {
		return toolErr("decode orders: %v", err)
	}
	byStatus := map[string]int{}
	for _, od := range ordersDoc.Orders {
		byStatus[od.Status]++
	}
	// TTL remaining from expiresAt.
	ttl := ""
	if exp, ok := info["expiresAt"].(string); ok {
		if t, err := time.Parse(time.RFC3339, exp); err == nil {
			ttl = time.Until(t).Round(time.Second).String()
		}
	}
	// Stochastic summary.
	stochSummary := map[string]any{"enabled": false}
	if st, ok := info["stochastic"].(map[string]any); ok {
		if cfg, ok := st["config"].(map[string]any); ok {
			stochSummary["enabled"] = cfg["enabled"]
			stochSummary["outcomes"] = st["outcomes"]
		}
	}
	status := map[string]any{
		"role":             info["role"],
		"connectionStatus": info["status"],
		"endpoint":         info["endpoint"],
		"identifiers":      info["identifiers"],
		"ttlRemaining":     ttl,
		"appMessages":      info["appMessages"],
		"messageCount":     info["messageCount"],
		"orders":           map[string]any{"total": len(ordersDoc.Orders), "byStatus": byStatus},
		"killSwitch":       info["killSwitch"],
		"stochastic":       stochSummary,
	}
	return ok(status)
}

// ---------- sandbox_list_messages ----------

type listMessagesInput struct {
	Token     string `json:"token"`
	Limit     int    `json:"limit,omitempty"`
	Direction string `json:"direction,omitempty"` // IN | OUT | (empty = both)
}

func handleListMessages(ctx context.Context, _ *mcp.CallToolRequest, in listMessagesInput) (*mcp.CallToolResult, any, error) {
	token, r, o, e := requireToken(tokenInput{Token: in.Token})
	if r != nil || e != nil {
		return r, o, e
	}
	limit := in.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 500 {
		return toolErr("limit must be 1..500, got %d", in.Limit)
	}
	dir := strings.ToUpper(strings.TrimSpace(in.Direction))
	if dir != "" && dir != "IN" && dir != "OUT" {
		return toolErr("direction must be IN or OUT, got %q", in.Direction)
	}
	// The API returns the newest M messages in chronological
	// (oldest-first) order. Fetch extra when filtering by direction so
	// the page still fills, then reverse to newest-first.
	fetchLimit := limit
	if dir != "" {
		fetchLimit = limit * 4
		if fetchLimit > 500 {
			fetchLimit = 500
		}
	}
	raw, err := callAPI(ctx, http.MethodGet,
		fmt.Sprintf("/api/v1/sessions/%s/messages?limit=%d", token, fetchLimit), nil)
	if err != nil {
		return toolErr("%v", err)
	}
	var doc struct {
		Messages []map[string]any `json:"messages"`
		Count    int              `json:"count"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return toolErr("decode messages: %v", err)
	}
	want := map[string]bool{"INBOUND": true, "OUTBOUND": true}
	if dir == "IN" {
		want = map[string]bool{"INBOUND": true}
	} else if dir == "OUT" {
		want = map[string]bool{"OUTBOUND": true}
	}
	out := make([]map[string]any, 0, limit)
	// Walk newest-first.
	for i := len(doc.Messages) - 1; i >= 0; i-- {
		m := doc.Messages[i]
		if d, _ := m["direction"].(string); !want[d] {
			continue
		}
		out = append(out, m)
		if len(out) >= limit {
			break
		}
	}
	return ok(map[string]any{"messages": out, "returned": len(out), "total": doc.Total})
}

// ---------- sandbox_send_order ----------

type sendOrderInput struct {
	Token   string            `json:"token"`
	MsgType string            `json:"msgType"` // D | F | G
	Fields  map[string]string `json:"fields"`  // tag → value, as strings
}

func handleSendOrder(ctx context.Context, _ *mcp.CallToolRequest, in sendOrderInput) (*mcp.CallToolResult, any, error) {
	token, r, o, e := requireToken(tokenInput{Token: in.Token})
	if r != nil || e != nil {
		return r, o, e
	}
	msgType := strings.ToUpper(strings.TrimSpace(in.MsgType))
	if msgType != "D" && msgType != "F" && msgType != "G" {
		return toolErr("msgType must be D, F or G, got %q", in.MsgType)
	}
	if len(in.Fields) == 0 {
		return toolErr("fields is required (tag → value, e.g. {\"11\": \"ORD-1\", \"55\": \"TEST\"})")
	}
	raw, err := callAPI(ctx, http.MethodPost, "/api/v1/sessions/"+token+"/send-order",
		map[string]any{"msgType": msgType, "fields": in.Fields})
	if err != nil {
		return toolErr("%v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return toolErr("decode send-order response: %v", err)
	}
	return ok(doc)
}

// ---------- sandbox_send_execution ----------

type executionParams struct {
	Qty          float64 `json:"qty,omitempty"`          // FILL / PARTIAL_FILL
	Price        float64 `json:"price,omitempty"`        // FILL / PARTIAL_FILL
	Text         string  `json:"text,omitempty"`         // REJECT / CANCEL_* / REPLACE_*
	OrdRejReason string  `json:"ordRejReason,omitempty"` // REJECT (tag 103)
	CxlRejReason string  `json:"cxlRejReason,omitempty"` // CANCEL_REJECT / REPLACE_REJECT (tag 434)
}

type sendExecutionInput struct {
	Token   string          `json:"token"`
	ClOrdID string          `json:"clOrdId"`
	Action  string          `json:"action"`
	Params  executionParams `json:"params,omitempty"`
}

var validActions = map[string]bool{
	"FILL": true, "PARTIAL_FILL": true, "REJECT": true,
	"CANCEL_ACCEPT": true, "CANCEL_REJECT": true,
	"REPLACE_ACCEPT": true, "REPLACE_REJECT": true,
}

func handleSendExecution(ctx context.Context, _ *mcp.CallToolRequest, in sendExecutionInput) (*mcp.CallToolResult, any, error) {
	token, r, o, e := requireToken(tokenInput{Token: in.Token})
	if r != nil || e != nil {
		return r, o, e
	}
	if strings.TrimSpace(in.ClOrdID) == "" {
		return toolErr("clOrdId is required")
	}
	action := strings.ToUpper(strings.TrimSpace(in.Action))
	if !validActions[action] {
		return toolErr("action must be one of FILL, PARTIAL_FILL, REJECT, CANCEL_ACCEPT, CANCEL_REJECT, REPLACE_ACCEPT, REPLACE_REJECT; got %q", in.Action)
	}
	raw, err := callAPI(ctx, http.MethodPost,
		fmt.Sprintf("/api/v1/sessions/%s/orders/%s/execute", token, in.ClOrdID),
		map[string]any{
			"action":       action,
			"qty":          in.Params.Qty,
			"price":        in.Params.Price,
			"text":         in.Params.Text,
			"ordRejReason": in.Params.OrdRejReason,
			"cxlRejReason": in.Params.CxlRejReason,
		})
	if err != nil {
		return toolErr("%v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return toolErr("decode execution response: %v", err)
	}
	return ok(doc)
}

// ---------- sandbox_send_raw ----------

type sendRawInput struct {
	Token  string `json:"token"`
	RawFix string `json:"rawFix"`
}

func handleSendRaw(ctx context.Context, _ *mcp.CallToolRequest, in sendRawInput) (*mcp.CallToolResult, any, error) {
	token, r, o, e := requireToken(tokenInput{Token: in.Token})
	if r != nil || e != nil {
		return r, o, e
	}
	if strings.TrimSpace(in.RawFix) == "" {
		return toolErr("rawFix is required (a raw FIX message; SOH, | or ^A delimiters accepted)")
	}
	raw, err := callAPI(ctx, http.MethodPost, "/api/v1/sessions/"+token+"/send-raw",
		map[string]any{"rawFix": in.RawFix})
	if err != nil {
		return toolErr("%v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return toolErr("decode send-raw response: %v", err)
	}
	return ok(doc)
}

// ---------- sandbox_get_scenario ----------

func handleGetScenario(ctx context.Context, _ *mcp.CallToolRequest, in tokenInput) (*mcp.CallToolResult, any, error) {
	token, r, o, e := requireToken(in)
	if r != nil || e != nil {
		return r, o, e
	}
	raw, err := callAPI(ctx, http.MethodGet, "/api/v1/sessions/"+token+"/scenario", nil)
	if err != nil {
		return toolErr("%v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return toolErr("decode scenario: %v", err)
	}
	return ok(doc)
}

// ---------- sandbox_replay ----------

type replayInput struct {
	Token       string         `json:"token"`                 // source session token (exactly one of token/scenario)
	Scenario    map[string]any `json:"scenario,omitempty"`    // or a full scenario document
	Speed       *float64       `json:"speed,omitempty"`       // timing multiplier; 0 = apply immediately
}

func handleReplay(ctx context.Context, _ *mcp.CallToolRequest, in replayInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Token) == "" && in.Scenario == nil {
		return toolErr("either token (source session) or scenario is required")
	}
	if strings.TrimSpace(in.Token) != "" && in.Scenario != nil {
		return toolErr("pass either token or scenario, not both")
	}
	body := map[string]any{}
	if strings.TrimSpace(in.Token) != "" {
		body["sourceToken"] = strings.TrimSpace(in.Token)
	} else {
		body["scenario"] = in.Scenario
	}
	if in.Speed != nil {
		body["speed"] = *in.Speed
	}
	raw, err := callAPI(ctx, http.MethodPost, "/api/v1/sessions/replay", body)
	if err != nil {
		return toolErr("%v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return toolErr("decode replay response: %v", err)
	}
	doc["note"] = "A fresh ACCEPTOR sandbox was created and the scenario is being re-enacted in it synthetically (marked replayed:true; nothing is transmitted on the wire). Poll sandbox_get_status for replay progress."
	return ok(doc)
}

// ---------- main ----------

func main() {
	if v := os.Getenv("FIXLAB_API_URL"); v != "" {
		apiBase = strings.TrimRight(v, "/")
	}
	// Logs go to stderr only: stdout is the MCP stdio channel.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := mcp.NewServer(&mcp.Implementation{Name: "fixlab", Title: "FixLab", Version: serverVersion}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sandbox_create",
		Description: "Create a FixLab FIX sandbox. ACCEPTOR: FixLab listens and your FIX engine connects in. INITIATOR: FixLab dials your FIX acceptor (SSRF-protected). Returns the session token (bearer secret — keep it), endpoint, identifiers, expiry, and how to connect.",
	}, handleCreate)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sandbox_get_status",
		Description: "Get a sandbox's status: role, FIX connection status, endpoint, TTL remaining, message/order counts, kill-switch and stochastic-simulator state.",
	}, handleGetStatus)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sandbox_list_messages",
		Description: "List a sandbox's FIX message history: raw pipe-delimited FIX plus parsed fields (tag/name/value/enum). Filter by IN/OUT direction.",
	}, handleListMessages)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sandbox_send_order",
		Description: "Inject a 35=D/F/G order message into the remote counterparty. INITIATOR sessions only. fields is tag→value (strings); D needs 11/55/54/38/40, F needs 11/41/55/54/38, G needs 11/41/55/54/38/40. Engine-stamped tags are rejected.",
	}, handleSendOrder)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sandbox_send_execution",
		Description: "Drive a working order on an ACCEPTOR sandbox: FILL (params qty, price), PARTIAL_FILL (qty, price), REJECT (ordRejReason, text), CANCEL_ACCEPT (text), CANCEL_REJECT (cxlRejReason, text), REPLACE_ACCEPT, REPLACE_REJECT (cxlRejReason, text). Returns the ExecutionReport sent.",
	}, handleSendExecution)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sandbox_send_raw",
		Description: "Power tool: send one raw FIX message (any delimiter: SOH, |, ^A) through the live FIX session. Must start with 8= and end with 10=. LIMITATION: the engine always stamps 8/9/10/34/49/52/56 — truly raw byte injection is NOT possible.",
	}, handleSendRaw)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sandbox_get_scenario",
		Description: "Get a sandbox's recorded scenario (phase 2.3): the application-level story — logon, inbound application messages, executions, logout/disconnect — as exportable JSON. Never contains the session token.",
	}, handleGetScenario)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sandbox_replay",
		Description: "Replay a scenario into a FRESH acceptor sandbox (phase 2.3): pass token (source session) or a full scenario document, plus optional speed (timing multiplier; 0 = apply immediately). Returns the new session token; the story is re-enacted synthetically (marked replayed:true, nothing transmitted). Rules/stochastic/kill-switch do not fire during replay.",
	}, handleReplay)

	slog.Info("fixlab mcp-server starting", "version", serverVersion, "api", apiBase)
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		slog.Error("mcp server error", "err", err)
		os.Exit(1)
	}
}
