// Package httpx holds the HTTP conventions every control-plane service shares:
// RFC 7807 problem responses (Implementation Guide §4), bounded JSON decoding,
// a CORS allowlist, and Idempotency-Key replay (SRS IR-1).
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MaxBodyBytes bounds request bodies on control-plane routes. Inference bodies
// are bounded separately because prompts can legitimately be large.
const MaxBodyBytes = 1 << 20

// Problem is an RFC 7807 problem document. Error duplicates Detail as an
// extension member so clients written against the old {"error": "..."} shape
// keep working.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
	// Fields carries per-field validation messages when a request is rejected.
	Fields map[string]string `json:"fields,omitempty"`
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteProblem writes an application/problem+json response.
func WriteProblem(w http.ResponseWriter, status int, detail string) {
	WriteProblemFields(w, status, detail, nil)
}

// WriteProblemFields writes a problem with per-field validation messages.
func WriteProblemFields(w http.ResponseWriter, status int, detail string, fields map[string]string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
		Error:  detail,
		Fields: fields,
	})
}

// DecodeJSON decodes a bounded JSON body into dst, rejecting unknown trailing
// data. On failure it writes a 400 problem and returns the error, so handlers
// can simply return.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			WriteProblem(w, http.StatusRequestEntityTooLarge, "Request body is too large")
		case errors.Is(err, io.EOF):
			WriteProblem(w, http.StatusBadRequest, "Request body is empty")
		default:
			WriteProblem(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		}
		return err
	}
	return nil
}

// CORS returns middleware that admits cross-origin requests only from the given
// origins. The console is served same-origin through the gateway, so in
// production the list is usually empty; in development it holds the frontend
// dev server. A wildcard is deliberately not supported: the old gateway sent
// Access-Control-Allow-Origin: * on every authenticated route.
func CORS(allowed []string, next http.Handler) http.Handler {
	set := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			set[o] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && set[origin] {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-ID, X-API-Key")
			h.Set("Access-Control-Expose-Headers", "X-Request-ID, Retry-After, X-RateLimit-Remaining")
			h.Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SplitList parses a comma-separated configuration value.
func SplitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
