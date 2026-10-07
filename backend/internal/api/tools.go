// Stateless developer tools (spec §36, phase 6): a FIX message decoder
// that needs no session and no account.
//
//	POST /api/v1/tools/decode   decode + validate one raw FIX message
//
// Rate limiting: ServeHTTP applies the same per-IP HTTP rate limiter to
// every route on the mux, so this anonymous endpoint is bounded exactly
// like the others; the handler additionally caps the input size and the
// field count so one paste cannot burn meaningful CPU.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"fixlab.dev/fixlab/backend/internal/dictionary"
	"fixlab.dev/fixlab/backend/internal/messages"
	"fixlab.dev/fixlab/backend/internal/security"
)

// Bounds for the anonymous decoder.
const (
	maxDecodeRawLen = 64 * 1024 // 64 KiB of pasted FIX
	maxDecodeFields = 5000      // parsed tag=value pairs
)

// decodeBody is the POST /api/v1/tools/decode request. beginString is an
// optional override; by default the dictionary is chosen from the
// message's own 8=BeginString. token is an optional session token: when
// present, the session's ACTIVE dictionary is used instead — including
// an uploaded custom dictionary (phase 2.4). The endpoint stays
// stateless when token is absent.
type decodeBody struct {
	RawFIX      string `json:"rawFix"`
	BeginString string `json:"beginString"`
	Token       string `json:"token"`
}

// decodeResult is the 200 response: parsed fields enriched with the
// dictionary plus Tag 9 (BodyLength) and Tag 10 (CheckSum) validation.
type decodeResult struct {
	Valid              bool             `json:"valid"`
	MsgType            string           `json:"msgType"`
	MsgName            string           `json:"msgName"`
	BeginString        string           `json:"beginString"`
	Fields             []messages.Field `json:"fields"`
	ChecksumOK         bool             `json:"checksumOk"`
	ExpectedChecksum   string           `json:"expectedChecksum"`
	ActualChecksum     string           `json:"actualChecksum"`
	BodyLengthOK       bool             `json:"bodyLengthOk"`
	ExpectedBodyLength int              `json:"expectedBodyLength"`
	ActualBodyLength   int              `json:"actualBodyLength"`
}

// decodeError is a structural problem with the input: the endpoint
// answers 400 and names the problem. Checksum / body-length mismatches
// are NOT errors — they are verdicts carried by decodeResult.
type decodeError struct{ msg string }

func (e *decodeError) Error() string { return e.msg }

func decodeErr(format string, args ...any) *decodeError {
	return &decodeError{msg: fmt.Sprintf(format, args...)}
}

// handleDecode decodes one raw FIX message. Delimiters may be SOH
// (\x01), "|" or "^A" — they are normalized before parsing.
func (s *Server) handleDecode(w http.ResponseWriter, r *http.Request) {
	var body decodeBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	// Session-aware decode (phase 2.4): with a token, the session's
	// active dictionary — custom override included — resolves the
	// tags. Unknown tokens are 404, exactly like every other
	// session-scoped route.
	var dict *dictionary.Dictionary
	if strings.TrimSpace(body.Token) != "" {
		token := strings.TrimSpace(body.Token)
		if !security.IsValidToken(token) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		sess, err := s.mgr.GetSession(token)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		dict = sess.Dictionary()
	}
	res, err := decodeFIXMessage(body.RawFIX, dict, body.BeginString)
	if err != nil {
		var de *decodeError
		if errors.As(err, &de) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": de.msg})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type rawField struct {
	tag    int
	tagStr string
	value  string
}

// decodeFIXMessage parses raw into a decodeResult. dict, when non-nil,
// is the dictionary to enrich with (phase 2.4: a session's active
// dictionary, custom override included); when nil, beginOverride (or
// the message's own 8=) selects the embedded dictionary.
func decodeFIXMessage(raw string, dict *dictionary.Dictionary, beginOverride string) (*decodeResult, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, decodeErr("rawFix is empty: paste a FIX message to decode")
	}
	if len(trimmed) > maxDecodeRawLen {
		return nil, decodeErr("rawFix is %d bytes: the limit is %d bytes", len(trimmed), maxDecodeRawLen)
	}

	// Normalize the three accepted delimiters to SOH. "^A" first so a
	// literal caret-A pair never survives into a tag value.
	soh := strings.ReplaceAll(strings.ReplaceAll(trimmed, "^A", "\x01"), "|", "\x01")

	kvs := make([]rawField, 0, 64)
	for i, part := range strings.Split(soh, "\x01") {
		if part == "" {
			continue // tolerate stray/duplicate delimiters
		}
		eq := strings.IndexByte(part, '=')
		if eq <= 0 {
			return nil, decodeErr("malformed field %d %q: expected tag=value", i+1, part)
		}
		tagStr, value := part[:eq], part[eq+1:]
		tag, err := strconv.Atoi(tagStr)
		if err != nil || tag <= 0 {
			return nil, decodeErr("invalid tag %q in field %d: tags are positive integers", tagStr, i+1)
		}
		kvs = append(kvs, rawField{tag: tag, tagStr: tagStr, value: value})
	}
	if len(kvs) > maxDecodeFields {
		return nil, decodeErr("message has %d fields: the limit is %d", len(kvs), maxDecodeFields)
	}
	if len(kvs) < 3 {
		return nil, decodeErr("message has only %d field(s): a FIX message needs at least 8=, 9= and 10=", len(kvs))
	}
	// FIX framing order: 8= first, 9= second, 10= last.
	if kvs[0].tag != 8 {
		return nil, decodeErr("missing 8=BeginString: it must be the first field of the message")
	}
	if kvs[1].tag != 9 {
		return nil, decodeErr("missing 9=BodyLength: it must be the second field of the message")
	}
	if kvs[len(kvs)-1].tag != 10 {
		return nil, decodeErr("missing 10=CheckSum: it must be the last field of the message")
	}

	beginString := strings.TrimSpace(beginOverride)
	if beginString == "" {
		beginString = kvs[0].value
	}
	if dict == nil {
		var err error
		dict, err = dictionary.ForBeginString(beginString)
		if err != nil {
			return nil, decodeErr("%s", err.Error())
		}
	} else {
		// Session-provided dictionary (phase 2.4): report its own
		// BeginString; the beginString override is ignored.
		beginString = dict.BeginString
	}

	// Tag 9 = byte count from the delimiter after 9=value through the SOH
	// terminating the field before 10= (inclusive).
	actualBL, err := strconv.Atoi(strings.TrimSpace(kvs[1].value))
	if err != nil {
		return nil, decodeErr("invalid 9=BodyLength %q: not an integer", kvs[1].value)
	}
	var bodyLen int
	{
		var b strings.Builder
		for _, f := range kvs[2 : len(kvs)-1] {
			b.WriteString(f.tagStr)
			b.WriteByte('=')
			b.WriteString(f.value)
			b.WriteByte('\x01')
		}
		bodyLen = b.Len()
	}

	// Tag 10 = (sum of every byte before the 10= field, delimiters
	// included) mod 256, rendered as 3 digits.
	var sum int
	for _, f := range kvs[:len(kvs)-1] {
		for i := 0; i < len(f.tagStr); i++ {
			sum += int(f.tagStr[i])
		}
		sum += '='
		for i := 0; i < len(f.value); i++ {
			sum += int(f.value[i])
		}
		sum += '\x01'
	}
	expectedChecksum := fmt.Sprintf("%03d", sum%256)
	actualChecksum := kvs[len(kvs)-1].value
	checksumOK := false
	if n, err := strconv.Atoi(strings.TrimSpace(actualChecksum)); err == nil {
		checksumOK = n == sum%256
	}

	fields := make([]messages.Field, 0, len(kvs))
	var msgType string
	for _, f := range kvs {
		name := dict.FieldName(f.tag)
		if name == "" {
			name = "Unknown"
		}
		fields = append(fields, messages.Field{
			Tag:             f.tag,
			Name:            name,
			Value:           f.value,
			EnumDescription: dict.EnumDescription(f.tag, f.value),
		})
		if f.tag == 35 && msgType == "" {
			msgType = f.value
		}
	}

	bodyLengthOK := actualBL == bodyLen
	return &decodeResult{
		Valid:              checksumOK && bodyLengthOK,
		MsgType:            msgType,
		MsgName:            dict.MessageName(msgType),
		BeginString:        beginString,
		Fields:             fields,
		ChecksumOK:         checksumOK,
		ExpectedChecksum:   expectedChecksum,
		ActualChecksum:     actualChecksum,
		BodyLengthOK:       bodyLengthOK,
		ExpectedBodyLength: bodyLen,
		ActualBodyLength:   actualBL,
	}, nil
}
