// Custom FIX dictionaries (phase 2.4, spec §48): per-session upload of a
// broker's QuickFIX data-dictionary XML so custom tags resolve to names
// and enum descriptions everywhere FixLab inspects messages.
//
//	POST   /api/v1/sessions/{token}/dictionary   upload → 200 + stats
//	GET    /api/v1/sessions/{token}/dictionary   active dictionary info
//	DELETE /api/v1/sessions/{token}/dictionary   revert to the standard dictionary
//
// The override is ephemeral: it dies with the session and is never
// persisted. It affects inspection only — WS FIX_MSG_IN/OUT field
// enrichment, /messages, the decode API (when called with the session
// token), and MCP list_messages. The QuickFIX/Go engine keeps
// validating the wire against the standard embedded dictionary; the
// engine config additionally passes user-defined fields (tags >= 5000)
// through to the app instead of session-rejecting them, so custom
// fields are inspectable. See README "Custom FIX dictionaries".
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"fixlab.dev/fixlab/backend/internal/dictionary"
)

// maxCustomDictXMLBytes caps an uploaded dictionary. The HTTP layer
// already caps request bodies at FIXLAB_HTTP_MAX_BODY (1 MiB); this
// lower cap keeps per-session memory bounded.
const maxCustomDictXMLBytes = 512 * 1024

// dictionaryUploadBody carries one custom dictionary upload.
type dictionaryUploadBody struct {
	XML  string `json:"xml"`
	Name string `json:"name"`
}

// handleDictionaryUpload validates and installs a per-session custom
// FIX dictionary (phase 2.4). Every failure names the exact problem
// (400); success returns the parsed stats (200).
func (s *Server) handleDictionaryUpload(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	var body dictionaryUploadBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	xmlText := strings.TrimSpace(body.XML)
	if xmlText == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "xml is empty: paste or upload a QuickFIX data-dictionary XML document"})
		return
	}
	if len(xmlText) > maxCustomDictXMLBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "dictionary is too large: limit is 512 KiB",
		})
		return
	}
	d, err := dictionary.ParseBytes([]byte(xmlText))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "custom"
	}
	sess.SetCustomDictionary(name, d)
	writeJSON(w, http.StatusOK, map[string]any{
		"custom":      true,
		"name":        name,
		"beginString": d.BeginString,
		"fields":      len(d.Fields),
		"messages":    len(d.MsgTypeToName),
	})
}

// handleDictionaryGet reports the session's active dictionary.
func (s *Server) handleDictionaryGet(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sess.DictionaryInfo())
}

// handleDictionaryDelete reverts to the standard embedded dictionary.
func (s *Server) handleDictionaryDelete(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionForToken(w, r)
	if !ok {
		return
	}
	sess.ClearCustomDictionary()
	writeJSON(w, http.StatusOK, map[string]any{"custom": false})
}
