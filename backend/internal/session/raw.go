// Raw FIX message transmission (phase 2.2, MCP sandbox_send_raw).
//
// SendRaw validates FIX framing on a caller-supplied raw message, strips
// every engine-stamped tag (8/9/10/34/49/52/56/…), and transmits the body
// through the FIX engine, which stamps the header/trailer itself (spec
// §25). This is a power tool for testing unusual flows — it cannot do
// truly raw byte injection, because sequence numbers, CompIDs, sending
// time, body length and checksum are always engine-stamped. That
// limitation is deliberate and documented.
package session

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/quickfixgo/quickfix"

	"fixlab.dev/fixlab/backend/internal/messages"
)

// rawStampedTags are stripped from caller input: the engine owns them.
// Tag 35 (MsgType) is read from the input but set on the header by us;
// the engine restamps it with the same value.
var rawStampedTags = map[int]bool{
	8: true, 9: true, 10: true, 34: true, 43: true,
	49: true, 52: true, 56: true, 122: true,
}

// rawErr builds an InjectError so the API maps statuses the same way it
// does for order injection (400 validation, 429 quota, 503 send failure).
func rawErr(status int, format string, args ...any) *InjectError {
	return &InjectError{Status: status, Msg: "send-raw: " + fmt.Sprintf(format, args...)}
}

// SendRaw validates framing, strips engine-stamped tags, transmits the
// message through the engine, and returns the sent record. It works on
// both ACCEPTOR and INITIATOR sessions (the message goes out over the
// live FIX session); it fails fast when the session is not logged on.
// Application-message quota applies, exactly like every other outbound
// application message.
func (s *Session) SendRaw(rawFix string) (*messages.Record, error) {
	msgType, body, err := parseRawFix(rawFix)
	if err != nil {
		return nil, err
	}

	msg := quickfix.NewMessage()
	msg.Header.SetString(quickfix.Tag(35), msgType)
	for tag, v := range body {
		msg.Body.SetString(quickfix.Tag(tag), v)
	}

	if err := s.SendExecution(msg); err != nil {
		return nil, injectSendErr(err)
	}
	return s.lastSentRecord(msgType, body)
}

// parseRawFix normalizes the three accepted delimiters (SOH, "|", "^A"),
// validates framing (must start with 8=, contain 35=, end with 10=), and
// returns the message type plus the body fields minus engine-stamped tags.
func parseRawFix(rawFix string) (string, map[int]string, error) {
	trimmed := strings.TrimSpace(rawFix)
	if trimmed == "" {
		return "", nil, rawErr(400, "rawFix is empty")
	}
	soh := strings.ReplaceAll(strings.ReplaceAll(trimmed, "^A", "\x01"), "|", "\x01")
	parts := strings.Split(soh, "\x01")
	fields := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			fields = append(fields, p)
		}
	}
	if len(fields) < 3 {
		return "", nil, rawErr(400, "not a FIX message: need at least 8=, 9= and 10= fields")
	}
	if !strings.HasPrefix(fields[0], "8=") {
		return "", nil, rawErr(400, "malformed: message must start with 8=BeginString, got %q", fields[0])
	}
	if !strings.HasPrefix(fields[len(fields)-1], "10=") {
		return "", nil, rawErr(400, "malformed: message must end with 10=CheckSum, got %q", fields[len(fields)-1])
	}

	var msgType string
	body := make(map[int]string)
	for _, f := range fields {
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			return "", nil, rawErr(400, "malformed field %q (want tag=value)", f)
		}
		tag, err := strconv.Atoi(strings.TrimSpace(kv[0]))
		if err != nil || tag <= 0 {
			return "", nil, rawErr(400, "invalid tag %q", kv[0])
		}
		val := kv[1]
		if tag == 35 {
			msgType = val
			continue
		}
		if rawStampedTags[tag] {
			continue // stripped: the engine stamps these
		}
		body[tag] = val
	}
	if msgType == "" {
		return "", nil, rawErr(400, "malformed: no 35=MsgType found")
	}
	return msgType, body, nil
}

// lastSentRecord finds the newest outbound record matching the sent
// message. The engine reports outbound messages through OnMessage
// synchronously inside SendExecution (QuickFIX/Go invokes ToApp before
// send returns), so the record is already in the ring buffer when we
// look. Matching on message type plus every body field makes the lookup
// unambiguous even when heartbeats interleave.
func (s *Session) lastSentRecord(msgType string, body map[int]string) (*messages.Record, error) {
	for _, rec := range s.MessageHistory(s.MessageCount()) {
		if rec.Direction != messages.Outbound || rec.MsgType != msgType {
			continue
		}
		got := make(map[int]string, len(rec.Fields))
		for _, f := range rec.Fields {
			got[f.Tag] = f.Value
		}
		match := true
		for tag, want := range body {
			if got[tag] != want {
				match = false
				break
			}
		}
		if match {
			return rec, nil
		}
	}
	return nil, rawErr(503, "message was sent but its record could not be located in history")
}
