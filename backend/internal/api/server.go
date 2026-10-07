// Package api exposes the FixLab REST API (spec §37) and the live
// WebSocket event stream (spec §19).
//
// Routes:
//
//	POST   /api/v1/sessions                          create a sandbox
//	GET    /api/v1/sessions/{token}                  fetch a sandbox
//	DELETE /api/v1/sessions/{token}                  destroy a sandbox
//	GET    /api/v1/sessions/{token}/messages         message history (phase 2)
//	GET    /api/v1/sessions/{token}/orders            order blotter (phase 3)
//	POST   /api/v1/sessions/{token}/orders/{clOrdId}/execute  execution action (phase 3)
//	POST   /api/v1/sessions/{token}/send-order         inject 35=D/F/G (phase 5, initiator only)
//	POST   /api/v1/sessions/{token}/send-raw           send one raw FIX message (phase 2.2, MCP)
//	GET    /api/v1/sessions/{token}/stochastic   stochastic policy (phase 2.1)
//	PUT    /api/v1/sessions/{token}/stochastic   set stochastic policy (phase 2.1)
//	GET    /api/v1/sessions/{token}/scenario     recorded scenario (phase 2.3)
//	POST   /api/v1/sessions/replay               replay scenario into a fresh session (phase 2.3)
//	POST   /api/v1/tools/decode                      decode one raw FIX message (phase 6)
//	POST   /api/v1/sessions/{token}/dictionary       upload a custom FIX dictionary (phase 2.4)
//	GET    /api/v1/sessions/{token}/dictionary       active dictionary info (phase 2.4)
//	DELETE /api/v1/sessions/{token}/dictionary       revert to the standard dictionary (phase 2.4)
//	GET    /ws/session/{token}                       live event stream (phase 2)
//	GET    /api/v1/metrics                           backend counters (observability)
//	GET    /healthz                                  liveness probe
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	nhws "nhooyr.io/websocket"

	"fixlab.dev/fixlab/backend/internal/common"
	"fixlab.dev/fixlab/backend/internal/messages"
	"fixlab.dev/fixlab/backend/internal/orders"
	"fixlab.dev/fixlab/backend/internal/rules"
	"fixlab.dev/fixlab/backend/internal/scenario"
	"fixlab.dev/fixlab/backend/internal/security"
	"fixlab.dev/fixlab/backend/internal/session"
	"fixlab.dev/fixlab/backend/internal/simulator"
	fxws "fixlab.dev/fixlab/backend/internal/websocket"
)

// Server is the HTTP API fronting the session manager.
type Server struct {
	cfg     common.Config
	log     *slog.Logger
	metrics *common.Metrics
	mgr     session.Manager
	hub     fxws.Hub
	mux     *http.ServeMux
	limiter *security.RateLimiter
	srv     *http.Server
}

// NewServer wires the routes. A nil hub is replaced with a NoopHub.
func NewServer(cfg common.Config, log *slog.Logger, metrics *common.Metrics, mgr session.Manager, hub fxws.Hub) *Server {
	if hub == nil {
		hub = fxws.NoopHub{}
	}
	s := &Server{
		cfg:     cfg,
		log:     log,
		metrics: metrics,
		mgr:     mgr,
		hub:     hub,
		mux:     http.NewServeMux(),
		limiter: security.NewRateLimiter(cfg.HTTPRequestsPerHourPerIP, time.Hour),
	}
	s.mux.HandleFunc("POST /api/v1/sessions", s.handleCreate)
	s.mux.HandleFunc("GET /api/v1/sessions/{token}", s.handleGet)
	s.mux.HandleFunc("DELETE /api/v1/sessions/{token}", s.handleDelete)
	s.mux.HandleFunc("GET /api/v1/sessions/{token}/messages", s.handleMessages)
	s.mux.HandleFunc("GET /api/v1/sessions/{token}/orders", s.handleOrders)
	s.mux.HandleFunc("POST /api/v1/sessions/{token}/orders/{clOrdId}/execute", s.handleExecute)
	s.mux.HandleFunc("POST /api/v1/sessions/{token}/send-order", s.handleSendOrder)
	s.mux.HandleFunc("POST /api/v1/sessions/{token}/send-raw", s.handleSendRaw)
	s.mux.HandleFunc("GET /api/v1/sessions/{token}/rules", s.handleRulesList)
	s.mux.HandleFunc("POST /api/v1/sessions/{token}/rules", s.handleRuleCreate)
	s.mux.HandleFunc("PUT /api/v1/sessions/{token}/rules/{ruleId}", s.handleRuleUpdate)
	s.mux.HandleFunc("DELETE /api/v1/sessions/{token}/rules/{ruleId}", s.handleRuleDelete)
	s.mux.HandleFunc("POST /api/v1/sessions/{token}/killswitch", s.handleKillSwitch)
	s.mux.HandleFunc("GET /api/v1/sessions/{token}/stochastic", s.handleStochasticGet)
	s.mux.HandleFunc("PUT /api/v1/sessions/{token}/stochastic", s.handleStochasticPut)
	s.mux.HandleFunc("GET /api/v1/sessions/{token}/scenario", s.handleScenario)
	s.mux.HandleFunc("POST /api/v1/sessions/replay", s.handleReplayStart)
	s.mux.HandleFunc("POST /api/v1/tools/decode", s.handleDecode)
	s.mux.HandleFunc("POST /api/v1/sessions/{token}/dictionary", s.handleDictionaryUpload)
	s.mux.HandleFunc("GET /api/v1/sessions/{token}/dictionary", s.handleDictionaryGet)
	s.mux.HandleFunc("DELETE /api/v1/sessions/{token}/dictionary", s.handleDictionaryDelete)
	s.mux.HandleFunc("GET /ws/session/{token}", s.handleWS)
	s.mux.HandleFunc("GET /api/v1/metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.srv = &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ip := clientIP(r)

	// CORS: the browser UI (Next.js dev server or a deployed frontend)
	// calls this API cross-origin. Session tokens are bearer credentials
	// in the URL path, never cookies, so a permissive origin is safe.
	origin := s.cfg.CORSOrigin
	if origin == "" {
		origin = "*"
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Max-Age", "86400")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if !s.limiter.Allow(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.HTTPMaxBodyBytes)
	// Record the HTTP client IP in the request context so the session
	// manager can enforce the per-IP active-session cap (spec §44).
	ctx := context.WithValue(r.Context(), session.ClientIPKey, ip)
	s.mux.ServeHTTP(w, r.WithContext(ctx))
	s.log.Info("http",
		"method", r.Method,
		"path", r.URL.Path,
		"remote_ip", ip,
		"duration_ms", time.Since(start).Milliseconds())
}

func clientIP(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		return h
	}
	if a, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return a
	}
	return r.RemoteAddr
}

// Start begins serving; it blocks until the server stops.
func (s *Server) Start() error { return s.srv.ListenAndServe() }

// Shutdown drains the HTTP server gracefully.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

type createBody struct {
	FIXVersion        string `json:"fixVersion"`
	Role              string `json:"role"`
	Transport         string `json:"transport"`
	TLS               bool   `json:"tls"`
	Auth              string `json:"auth"`
	TargetCompID      string `json:"targetCompId"`
	RemoteHost        string `json:"remoteHost"`
	RemotePort        string `json:"remotePort"`
	RemoteCompID      string `json:"remoteCompId"`
	LocalSenderCompID string `json:"localSenderCompId"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body createBody
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
	}
	sess, err := s.mgr.CreateSession(r.Context(), session.CreateRequest{
		FIXVersion:        body.FIXVersion,
		Role:              body.Role,
		Transport:         body.Transport,
		TLS:               body.TLS,
		Auth:              body.Auth,
		TargetCompID:      body.TargetCompID,
		RemoteHost:        body.RemoteHost,
		RemotePort:        body.RemotePort,
		RemoteCompID:      body.RemoteCompID,
		LocalSenderCompID: body.LocalSenderCompID,
	})
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "exhausted") ||
			strings.Contains(err.Error(), "maximum outbound") {
			status = http.StatusServiceUnavailable
		}
		if strings.Contains(err.Error(), "active session limit") {
			status = http.StatusTooManyRequests
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, s.mgr.Info(sess))
}

// sendOrderBody carries one outbound order injection (phase 5, spec §8):
// msgType D/F/G plus raw FIX fields as tag-number → value.
type sendOrderBody struct {
	MsgType string            `json:"msgType"`
	Fields  map[string]string `json:"fields"`
}

// handleSendOrder injects a NewOrderSingle / OrderCancelRequest /
// OrderCancelReplaceRequest into the remote counterparty on an
// INITIATOR session (phase 5).
func (s *Server) handleSendOrder(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	var body sendOrderBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if body.Fields == nil {
		body.Fields = map[string]string{}
	}
	order, err := sess.InjectOrder(session.InjectRequest{
		MsgType: body.MsgType,
		Fields:  body.Fields,
	})
	if err != nil {
		status := http.StatusBadRequest
		var ie *session.InjectError
		if errors.As(err, &ie) {
			status = ie.Status
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": order})
}

// sendRawBody carries one raw FIX message for transmission (phase 2.2,
// MCP sandbox_send_raw). Any of the three delimiters (SOH, "|", "^A")
// is accepted; the message must start with 8= and end with 10=.
type sendRawBody struct {
	RawFix string `json:"rawFix"`
}

// handleSendRaw transmits one raw FIX message through the session's
// engine on either role. Engine-stamped tags (8/9/10/34/49/52/56/…)
// supplied by the caller are stripped and restamped by the engine, so
// this is not true byte-level injection — the framing, sequence numbers
// and checksum are always engine-owned.
func (s *Server) handleSendRaw(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	var body sendRawBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	rec, err := sess.SendRaw(body.RawFix)
	if err != nil {
		status := http.StatusBadRequest
		var ie *session.InjectError
		if errors.As(err, &ie) {
			status = ie.Status
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": rec})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if !security.IsValidToken(token) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	sess, err := s.mgr.GetSession(token)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	writeJSON(w, http.StatusOK, s.mgr.Info(sess))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if !security.IsValidToken(token) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	if err := s.mgr.DestroySession(token); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if !security.IsValidToken(token) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	sess, err := s.mgr.GetSession(token)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	limit := 100
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n >= 0 {
			limit = n
		}
	}
	msgs := sess.MessageHistory(limit)
	if msgs == nil {
		msgs = []*messages.Record{} // never encode a null array
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": msgs,
		"count":    len(msgs),
		"total":    sess.MessageCount(),
	})
}

// handleOrders returns the session's order blotter (spec §23, phase 3).
func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	list := sess.Orders().List()
	if list == nil {
		list = []*orders.Order{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"orders": list,
		"count":  len(list),
	})
}

// executeBody carries one browser execution action (spec §24).
type executeBody struct {
	Action       string  `json:"action"`
	Qty          float64 `json:"qty"`
	Price        float64 `json:"price"`
	OrdRejReason string  `json:"ordRejReason"`
	CxlRejReason string  `json:"cxlRejReason"`
	Text         string  `json:"text"`
}

// handleExecute runs one execution action against a working order
// (spec §37): FILL, PARTIAL_FILL, REJECT, CANCEL_ACCEPT, CANCEL_REJECT,
// REPLACE_ACCEPT, REPLACE_REJECT.
func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	clOrdID := r.PathValue("clOrdId")
	var body executeBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	sim := sess.Simulator()
	if sim == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "trading simulator not available"})
		return
	}
	var (
		res *simulator.ExecutionResult
		err error
	)
	switch body.Action {
	case "FILL":
		res, err = sim.Fill(clOrdID, body.Qty, body.Price)
	case "PARTIAL_FILL":
		res, err = sim.PartialFill(clOrdID, body.Qty, body.Price)
	case "REJECT":
		res, err = sim.Reject(clOrdID, body.OrdRejReason, body.Text)
	case "CANCEL_ACCEPT":
		res, err = sim.CancelAccept(clOrdID, body.Text)
	case "CANCEL_REJECT":
		res, err = sim.CancelReject(clOrdID, body.CxlRejReason, body.Text)
	case "REPLACE_ACCEPT":
		res, err = sim.ReplaceAccept(clOrdID)
	case "REPLACE_REJECT":
		res, err = sim.ReplaceReject(clOrdID, body.CxlRejReason, body.Text)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "unknown action " + strconv.Quote(body.Action) + " (want FILL, PARTIAL_FILL, REJECT, CANCEL_ACCEPT, CANCEL_REJECT, REPLACE_ACCEPT, REPLACE_REJECT)",
		})
		return
	}
	if err != nil {
		writeJSON(w, executeErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleRulesList returns the session's rules in evaluation order
// (spec §32, phase 4).
func (s *Server) handleRulesList(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	list := sess.Rules().ListRules()
	if list == nil {
		list = []rules.Rule{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rules": list,
		"count": len(list),
	})
}

// handleRuleCreate validates and stores a new rule (phase 4). Duplicate
// priorities are allowed; ordering is (priority, creation order).
func (s *Server) handleRuleCreate(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	var body rules.Rule
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	created, err := sess.Rules().AddRule(body)
	if err != nil {
		writeJSON(w, ruleErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, created)
}

// handleRuleUpdate replaces a rule's editable fields (phase 4).
func (s *Server) handleRuleUpdate(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	ruleID := r.PathValue("ruleId")
	var body rules.Rule
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	updated, err := sess.Rules().UpdateRule(ruleID, body)
	if err != nil {
		writeJSON(w, ruleErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleRuleDelete removes a rule (phase 4).
func (s *Server) handleRuleDelete(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	ruleID := r.PathValue("ruleId")
	if err := sess.Rules().RemoveRule(ruleID); err != nil {
		writeJSON(w, ruleErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": ruleID})
}

// ruleErrorStatus maps rule-engine errors to HTTP statuses.
func ruleErrorStatus(err error) int {
	if errors.Is(err, rules.ErrRuleNotFound) {
		return http.StatusNotFound
	}
	if rules.IsValidationError(err) {
		return http.StatusBadRequest
	}
	return http.StatusBadRequest
}

type killSwitchBody struct {
	Enabled bool `json:"enabled"`
}

// handleStochasticGet returns the session's stochastic simulation
// policy: the config plus the outcomes drawn so far (phase 2.1,
// spec §48).
func (s *Server) handleStochasticGet(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sess.Simulator().Stochastic().State())
}

// handleStochasticPut validates and stores the session's stochastic
// simulation policy (phase 2.1, spec §48). Percentages must sum to 100;
// validation errors name the problem (400).
func (s *Server) handleStochasticPut(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	var body simulator.StochasticConfig
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if _, err := sess.Simulator().Stochastic().SetConfig(body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sess.Simulator().Stochastic().State())
}

// handleKillSwitch toggles the session's venue halt (spec §31, phase 4).
// While on, every inbound NewOrderSingle is rejected immediately and no
// rule fires. A KILL_SWITCH event streams to browsers.
func (s *Server) handleKillSwitch(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	var body killSwitchBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	enabled := sess.SetKillSwitch(body.Enabled)
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": enabled})
}

// handleScenario returns the session's recorded scenario (phase 2.3,
// spec §48): the application-level story — logon, inbound application
// messages, executions, logout/disconnect. The export never contains
// the session token or any secret.
func (s *Server) handleScenario(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sess.ScenarioSnapshot())
}

// replayBody carries one scenario replay request (phase 2.3): exactly
// one of sourceToken / scenario, plus an optional speed multiplier
// (absent = 1 = real time; 0 = apply every step immediately).
type replayBody struct {
	SourceToken string             `json:"sourceToken"`
	Scenario    *scenario.Scenario `json:"scenario"`
	Speed       *float64           `json:"speed"`
}

// handleReplayStart replays a scenario into a FRESH session (phase 2.3,
// spec §48: "replay previous session"). It creates a new ACCEPTOR
// sandbox and re-enacts the story synthetically: inbound messages are
// re-injected through the simulator (FIX engine bypassed, marked
// replayed), executions re-applied through the normal execution path
// (never transmitted, no quota consumed). Rules, stochastic and the
// kill switch never fire during replay.
func (s *Server) handleReplayStart(w http.ResponseWriter, r *http.Request) {
	var body replayBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	speed := 1.0
	if body.Speed != nil {
		speed = *body.Speed
	}
	token, total, err := s.mgr.ReplaySession(r.Context(), session.ReplayRequest{
		SourceToken: body.SourceToken,
		Scenario:    body.Scenario,
		Speed:       speed,
	})
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"sessionToken": token,
		"totalSteps":   total,
		"speed":        speed,
	})
}

// sessionForToken authenticates the path token and loads the session,
// writing the 404 on failure.
func (s *Server) sessionForToken(w http.ResponseWriter, r *http.Request) (*session.Session, bool) {
	token := r.PathValue("token")
	if !security.IsValidToken(token) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return nil, false
	}
	sess, err := s.mgr.GetSession(token)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return nil, false
	}
	return sess, true
}

// executeErrorStatus maps simulator errors to HTTP statuses.
func executeErrorStatus(err error) int {
	var qe *simulator.QuotaExceededError
	if errors.As(err, &qe) {
		return http.StatusTooManyRequests
	}
	if simulator.IsNotFound(err) {
		return http.StatusNotFound
	}
	var ie *simulator.InputError
	if errors.As(err, &ie) {
		return http.StatusBadRequest
	}
	var se *simulator.StateError
	if errors.As(err, &se) {
		return http.StatusConflict
	}
	return http.StatusConflict
}

// handleWS upgrades to a native RFC 6455 WebSocket and streams the
// session's live events (spec §19). The session token in the path is the
// authenticator; unknown tokens are rejected before the upgrade.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if !security.IsValidToken(token) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	sess, err := s.mgr.GetSession(token)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}

	c, err := nhws.Accept(w, r, &nhws.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		s.log.Warn("ws: accept failed", "err", err)
		return
	}
	defer c.Close(nhws.StatusNormalClosure, "handler done")
	ctx := r.Context()

	s.log.Info("ws: subscribed", "token", shortToken(token))
	ch, unsub := s.hub.Subscribe(token)
	defer unsub()

	// Snapshot: tell the browser the current connection status
	// immediately, so a late joiner doesn't start at WAITING_FOR_CONNECTION.
	s.sendEvent(ctx, c, fxws.Event{
		Type:      fxws.EventConnectionStatus,
		SessionID: token,
		Timestamp: time.Now(),
		Payload:   map[string]string{"status": string(sess.GetStatus()), "previous": ""},
	})

	// Read loop: we don't expect client messages, but reading detects
	// disconnects and keeps nhooyr's ping/pong handling alive.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-readDone:
			return
		case ev, ok := <-ch:
			if !ok {
				// Session destroyed/expired: the hub closed our channel.
				_ = c.Close(nhws.StatusNormalClosure, "session closed")
				return
			}
			if !s.sendEvent(ctx, c, ev) {
				return
			}
		}
	}
}

// sendEvent marshals and writes one event with a write deadline. It
// reports false when the connection is dead.
func (s *Server) sendEvent(ctx context.Context, c *nhws.Conn, ev fxws.Event) bool {
	data, err := json.Marshal(ev)
	if err != nil {
		s.log.Warn("ws: marshal event failed", "err", err)
		return true
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.Write(wctx, nhws.MessageText, data); err != nil {
		return false
	}
	return true
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.metrics.Snapshot())
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// shortToken renders a token prefix for logs so full tokens never land
// in log files.
func shortToken(token string) string {
	if len(token) > 14 {
		return token[:14] + "…"
	}
	return token
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
