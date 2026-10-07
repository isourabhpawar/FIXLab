// Outbound order injection (phase 5, spec §8).
//
// On initiator sessions the browser can inject NewOrderSingle (35=D),
// OrderCancelRequest (35=F) and OrderCancelReplaceRequest (35=G) into
// the remote FIX acceptor. Injected orders are tracked in the blotter
// with Direction=OUTBOUND; their lifecycle is driven by the remote's
// ExecutionReports (see simulator/remote.go), never by rules or browser
// executions.
package session

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/quickfixgo/quickfix"

	"fixlab.dev/fixlab/backend/internal/orders"
	"fixlab.dev/fixlab/backend/internal/simulator"
)

// InjectRequest is one outbound order injection (spec §37, phase 5).
type InjectRequest struct {
	MsgType string            // "D", "F" or "G"
	Fields  map[string]string // tag number (as string) → value
}

// InjectError is a typed injection failure carrying the HTTP status the
// API should return.
type InjectError struct {
	Status int
	Msg    string
}

func (e *InjectError) Error() string { return e.Msg }

func injectErr(status int, format string, args ...any) *InjectError {
	return &InjectError{Status: status, Msg: "inject: " + fmt.Sprintf(format, args...)}
}

// requiredFields per message type (tag numbers as strings).
var injectRequired = map[string][]string{
	"D": {"11", "55", "54", "38", "40"},
	"F": {"11", "41", "55", "54", "38"},
	"G": {"11", "41", "55", "54", "38", "40"},
}

// engineStampedTags may not be supplied by the caller: the FIX engine
// stamps them (spec §25).
var engineStampedTags = map[string]string{
	"8": "BeginString", "9": "BodyLength", "10": "CheckSum",
	"34": "MsgSeqNum", "35": "MsgType", "43": "PossDupFlag",
	"49": "SenderCompID", "52": "SendingTime", "56": "TargetCompID",
	"122": "OrigSendingTime",
}

// InjectOrder validates, builds, sends, and records one outbound order
// message. The whole check-mutate-send sequence holds execMu so a
// concurrent injection cannot interleave a duplicate ClOrdID or
// overshoot the quota.
func (s *Session) InjectOrder(req InjectRequest) (*orders.Order, error) {
	if s.Role != RoleInitiator {
		return nil, injectErr(400, "send-order is only available on INITIATOR sessions")
	}
	msgType := strings.ToUpper(strings.TrimSpace(req.MsgType))
	required, ok := injectRequired[msgType]
	if !ok {
		return nil, injectErr(400, "unsupported msgType %q (want D, F or G)", req.MsgType)
	}
	if !s.GetStatus().Connected() {
		return nil, injectErr(400, "session is not connected (status %s); wait for LOGON_ACCEPTED", s.GetStatus())
	}
	fields, err := validateInjectFields(msgType, required, req.Fields)
	if err != nil {
		return nil, err
	}

	s.execMu.Lock()
	defer s.execMu.Unlock()

	switch msgType {
	case "D":
		return s.injectNewOrder(fields)
	case "F":
		return s.injectCancel(fields)
	case "G":
		return s.injectReplace(fields)
	}
	return nil, injectErr(400, "unsupported msgType %q", msgType)
}

// validateInjectFields checks presence, tag syntax, engine-stamped tags
// and numeric fields. It returns the normalized field map.
func validateInjectFields(msgType string, required []string, in map[string]string) (map[int]string, error) {
	out := make(map[int]string, len(in))
	for k, v := range in {
		ks := strings.TrimSpace(k)
		tag, err := strconv.Atoi(ks)
		if err != nil || tag <= 0 {
			return nil, injectErr(400, "field tag %q is not a valid FIX tag number", k)
		}
		if name, stamped := engineStampedTags[ks]; stamped {
			return nil, injectErr(400, "tag %s (%s) is stamped by the FIX engine and must not be supplied", ks, name)
		}
		v = strings.TrimSpace(v)
		if v == "" {
			return nil, injectErr(400, "field %s is empty", ks)
		}
		out[tag] = v
	}
	for _, r := range required {
		if _, ok := out[mustTag(r)]; !ok {
			return nil, injectErr(400, "msgType %s requires field %s", msgType, r)
		}
	}
	// Numeric sanity for the common quantity/price tags when present.
	for _, t := range []string{"38", "40", "44"} {
		if v, ok := out[mustTag(t)]; ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 || (t == "38" && f == 0) {
				return nil, injectErr(400, "field %s must be a positive number, got %q", t, v)
			}
		}
	}
	return out, nil
}

func mustTag(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// buildInjectMessage assembles the FIX message; the engine stamps
// header/trailer (sequence number, CompIDs, sending time, checksum).
func buildInjectMessage(msgType string, fields map[int]string) *quickfix.Message {
	msg := quickfix.NewMessage()
	msg.Header.SetString(quickfix.Tag(35), msgType)
	for tag, v := range fields {
		msg.Body.SetString(quickfix.Tag(tag), v)
	}
	if _, ok := fields[60]; !ok {
		msg.Body.SetString(quickfix.Tag(60), time.Now().UTC().Format("20060102-15:04:05.000"))
	}
	return msg
}

// injectNewOrder sends a 35=D and records the OUTBOUND order.
func (s *Session) injectNewOrder(fields map[int]string) (*orders.Order, error) {
	clOrdID := fields[11]
	if _, exists := s.orderStore.Get(clOrdID); exists {
		return nil, injectErr(409, "duplicate ClOrdID %q", clOrdID)
	}
	qty, _ := strconv.ParseFloat(fields[38], 64)
	price, _ := strconv.ParseFloat(fields[44], 64) // "" → 0, fine

	msg := buildInjectMessage("D", fields)
	if err := s.sendExecutionLocked(msg); err != nil {
		return nil, injectSendErr(err)
	}
	seq := s.execSeq.Add(1)
	o := &orders.Order{
		OrderID:   fmt.Sprintf("FLB%06d", seq),
		ClOrdID:   clOrdID,
		Direction: orders.DirectionOutbound,
		Symbol:    fields[55],
		Side:      fields[54],
		OrderQty:  qty,
		Price:     price,
		OrdType:   fields[40],
		LeavesQty: qty,
		Status:    orders.StatusNew,
	}
	if err := s.orderStore.Add(o); err != nil {
		// The message already went out; keep the blotter honest about
		// the failure instead of silently dropping the order.
		return nil, injectErr(409, "order %q: %v", clOrdID, err)
	}
	s.PublishOrderEvent("ORDER_CREATED", o)
	s.loggerOrDefault().Info("inject: NewOrderSingle sent",
		"token", shortToken(s.Token), "cl_ord_id", clOrdID)
	return o, nil
}

// injectCancel sends a 35=F against a working OUTBOUND order.
func (s *Session) injectCancel(fields map[int]string) (*orders.Order, error) {
	o, err := s.outboundWorkingOrder(fields[41])
	if err != nil {
		return nil, err
	}
	reqClOrdID := fields[11]
	if _, exists := s.orderStore.Get(reqClOrdID); exists {
		return nil, injectErr(409, "duplicate ClOrdID %q", reqClOrdID)
	}
	msg := buildInjectMessage("F", fields)
	if err := s.sendExecutionLocked(msg); err != nil {
		return nil, injectSendErr(err)
	}
	o.PrevStatus = o.Status
	o.Status = orders.StatusPendingCancel
	o.CancelReqClOrdID = reqClOrdID
	if uerr := s.orderStore.Update(o); uerr != nil {
		return nil, injectErr(409, "order %q: %v", o.ClOrdID, uerr)
	}
	s.PublishOrderEvent("ORDER_UPDATED", o)
	return o, nil
}

// injectReplace sends a 35=G against a working OUTBOUND order and
// records the pending request; the remote's 35=8 (39=5) confirms it.
func (s *Session) injectReplace(fields map[int]string) (*orders.Order, error) {
	o, err := s.outboundWorkingOrder(fields[41])
	if err != nil {
		return nil, err
	}
	reqClOrdID := fields[11]
	if _, exists := s.orderStore.Get(reqClOrdID); exists {
		return nil, injectErr(409, "duplicate ClOrdID %q", reqClOrdID)
	}
	newQty, _ := strconv.ParseFloat(fields[38], 64)
	if newQty < o.CumQty {
		return nil, injectErr(400, "replace qty %v below already-filled CumQty %v", newQty, o.CumQty)
	}
	newPrice, _ := strconv.ParseFloat(fields[44], 64)
	msg := buildInjectMessage("G", fields)
	if err := s.sendExecutionLocked(msg); err != nil {
		return nil, injectSendErr(err)
	}
	o.PendingReplace = &orders.PendingReplace{
		ClOrdID:  reqClOrdID,
		OrderQty: newQty,
		Price:    newPrice,
	}
	if uerr := s.orderStore.Update(o); uerr != nil {
		return nil, injectErr(409, "order %q: %v", o.ClOrdID, uerr)
	}
	s.PublishOrderEvent("ORDER_UPDATED", o)
	return o, nil
}

// outboundWorkingOrder loads the order referenced by OrigClOrdID (41)
// and verifies it is an OUTBOUND order that can still be worked.
func (s *Session) outboundWorkingOrder(origClOrdID string) (*orders.Order, error) {
	o, ok := s.orderStore.Get(origClOrdID)
	if !ok {
		return nil, injectErr(404, "order %q not found", origClOrdID)
	}
	if o.Direction != orders.DirectionOutbound {
		return nil, injectErr(400, "order %q was not injected outbound", origClOrdID)
	}
	if !o.Status.Working() {
		return nil, injectErr(409, "order %q is %s (not working)", origClOrdID, o.Status)
	}
	return o, nil
}

// injectSendErr maps send-path failures (quota, engine down) to HTTP
// statuses.
func injectSendErr(err error) *InjectError {
	var qe *simulator.QuotaExceededError
	if errors.As(err, &qe) {
		return injectErr(429, "%v", qe)
	}
	return injectErr(503, "send failed: %v", err)
}
