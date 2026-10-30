package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"go.opentelemetry.io/otel/trace"
)

const (
	headerRequestID = "X-Request-Id"
	headerTraceID   = "X-Trace-Id"
)

// proxyErrorBody mirrors the OpenAI-style {"error":{...}} envelope so
// SDKs that already parse provider errors can surface llmtap faults
// without a special branch. type is always llmtap_proxy_error.
type proxyErrorBody struct {
	Error proxyErrorDetail `json:"error"`
}

type proxyErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// requestID returns the inbound X-Request-Id when present, else a
// fresh 16-byte hex id. Clients can pass their own for log joins.
func requestID(r *http.Request) string {
	if r == nil {
		return newRequestID()
	}
	if v := r.Header.Get(headerRequestID); v != "" {
		return v
	}
	return newRequestID()
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Extremely unlikely; fall back to zeros rather than
		// panicking on an error path.
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// setCorrelationHeaders stamps X-Request-Id and, when a recording
// span is present, X-Trace-Id onto the response. Safe to call before
// WriteHeader on every proxy-generated error path.
func setCorrelationHeaders(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		return
	}
	w.Header().Set(headerRequestID, requestID(r))
	if r == nil {
		return
	}
	sc := trace.SpanFromContext(r.Context()).SpanContext()
	if sc.IsValid() {
		w.Header().Set(headerTraceID, sc.TraceID().String())
	}
}

// writeProxyJSONError writes a JSON error body with correlation
// headers. Used for proxy-generated upstream faults (notably 502)
// so clients can distinguish llmtap from the provider.
func writeProxyJSONError(w http.ResponseWriter, r *http.Request, status int, message, code string) {
	setCorrelationHeaders(w, r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(proxyErrorBody{
		Error: proxyErrorDetail{
			Message: message,
			Type:    "llmtap_proxy_error",
			Code:    code,
		},
	})
}

// writeProxyPlainError is http.Error plus correlation headers. Used
// for admission rejects (401/429/503/413) where a plaintext body is
// established convention.
func writeProxyPlainError(w http.ResponseWriter, r *http.Request, status int, message string) {
	setCorrelationHeaders(w, r)
	http.Error(w, message, status)
}
