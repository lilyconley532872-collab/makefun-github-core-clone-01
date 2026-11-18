// http.go — shared HTTP plumbing: the response type, auth, body reading, JSON helpers.

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

const bodyLimitBytes = 32 * 1024 * 1024

var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

// response is one handler's answer. Exactly one of json/raw/noBody applies.
type response struct {
	status  int
	json    interface{}
	raw     []byte
	ctype   string
	headers map[string]string
	noBody  bool
}

func jsonResp(status int, body interface{}, headers map[string]string) response {
	return response{status: status, json: body, headers: headers}
}

func errorResp(status int, message string) response {
	return response{status: status, json: map[string]interface{}{"message": message}}
}

func rawResp(status int, raw []byte, ctype string, headers map[string]string) response {
	return response{status: status, raw: raw, ctype: ctype, headers: headers}
}

// tokenAccepted: the trial's token when set, else any non-empty token (local runs).
func tokenAccepted(token string) bool {
	if token == "" {
		return false
	}
	if shopToken != "" {
		return token == shopToken
	}
	return true
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return auth[len("Bearer "):]
	}
	return ""
}

// loginForRequest: the login this request acts as, binding the token to the first
// owner it names. Returns "" when the token is bound to a different owner.
func loginForRequest(s *Store, token, owner string) string {
	if token == "" || !tokenAccepted(token) {
		return ""
	}
	if bound, ok := s.loginFor(token); ok {
		if bound == owner {
			return bound
		}
		return ""
	}
	return s.bindLogin(token, owner)
}

// readBody reads the request body (capped at 32 MiB, like the TS entry).
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	lr := io.LimitReader(r.Body, bodyLimitBytes+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if len(data) > bodyLimitBytes {
		return nil, errBodyTooLarge
	}
	return data, nil
}

var errBodyTooLarge = &bodyTooLargeError{}

type bodyTooLargeError struct{}

func (e *bodyTooLargeError) Error() string { return "Request body too large" }

// decodeSegments splits a path into decoded segments (no leading empties).
func decodeSegments(path string) []string {
	var out []string
	for _, raw := range strings.Split(path, "/") {
		if raw == "" {
			continue
		}
		if decoded, err := unescapePath(raw); err == nil {
			out = append(out, decoded)
		} else {
			out = append(out, raw)
		}
	}
	return out
}

func unescapePath(s string) (string, error) {
	// Path segments are %-decoded (like decodeURIComponent), keeping '+' literal.
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := hexVal(s[i+1])
			lo, ok2 := hexVal(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
			return "", errBadEscape
		}
		b.WriteByte(s[i])
	}
	return b.String(), nil
}

var errBadEscape = &badEscapeError{}

type badEscapeError struct{}

func (e *badEscapeError) Error() string { return "invalid escape" }

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// parseJSONBody parses a JSON body; empty body -> empty map (like the TS entry).
func parseJSONBody(body []byte) (interface{}, error) {
	if len(body) == 0 {
		return map[string]interface{}{}, nil
	}
	var v interface{}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func isRecord(v interface{}) bool {
	_, ok := v.(map[string]interface{})
	return ok
}

func asRecord(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return nil
}

func strField(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func numField(m map[string]interface{}, key string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	if f, ok := m[key].(float64); ok {
		return f, true
	}
	return 0, false
}

func intField(m map[string]interface{}, key string, fallback int) int {
	if f, ok := numField(m, key); ok {
		return int(f)
	}
	return fallback
}

// requestIDCounter supplies the X-GitHub-Request-Id values (req_1, req_2, ...).
var requestIDCounter atomic.Int64

func nextRequestID() string {
	return "req_" + strconv.FormatInt(requestIDCounter.Add(1), 10)
}

// sendResponse writes a response, always with X-GitHub-Request-Id.
func sendResponse(w http.ResponseWriter, resp response, requestID string) {
	h := w.Header()
	h.Set("X-GitHub-Request-Id", requestID)
	for k, v := range resp.headers {
		h.Set(k, v)
	}
	if resp.raw != nil {
		h.Set("Content-Type", resp.ctype)
		if resp.ctype == "" {
			h.Set("Content-Type", "application/octet-stream")
		}
		h.Set("Content-Length", strconv.Itoa(len(resp.raw)))
		w.WriteHeader(resp.status)
		w.Write(resp.raw)
		return
	}
	if resp.status == 204 || resp.noBody || resp.json == nil {
		w.WriteHeader(resp.status)
		return
	}
	body, err := json.Marshal(resp.json)
	if err != nil {
		h.Set("Content-Type", "application/json")
		w.WriteHeader(500)
		w.Write([]byte(`{"message":"Internal Server Error"}`))
		return
	}
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.status)
	w.Write(body)
}
