package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/httpapi/middleware"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/logging"
)

// ErrorBody is the single error shape every endpoint returns.
//
// WHY A CONSISTENT ENVELOPE MATTERS
// ---------------------------------
// A client must be able to handle errors with ONE code path. If some endpoints return
// a bare string, some a {"message": ...} object and some an HTML error page, every
// caller ends up sniffing the response shape before it can react. The TanStack Query
// layer in Phase 8 will parse exactly this structure and nothing else.
//
// `code` is a stable machine-readable token; clients branch on it. `message` is for
// humans and may be reworded freely -- so clients must never match on it. That split
// is what lets us improve error text without breaking API consumers.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail carries the machine-readable code and human-readable message.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// RequestID lets a user quote an exact identifier when reporting a failure,
	// which turns an unfalsifiable bug report into a one-query log lookup.
	RequestID string `json:"request_id,omitempty"`
}

// writeJSON serialises payload as JSON with the given status code.
//
// WHY BUFFER FIRST INSTEAD OF ENCODING STRAIGHT TO w
// --------------------------------------------------
// The obvious implementation is:
//
//	w.WriteHeader(status)
//	json.NewEncoder(w).Encode(payload)
//
// It has a real bug. WriteHeader commits the status code and flushes the headers
// immediately. If Encode then fails halfway -- an unsupported type, a channel or
// function in a struct, a MarshalJSON that errors -- the client has already been told
// "200 OK" and receives a truncated body it will fail to parse. There is no way to
// retract a status code once sent.
//
// Encoding into a buffer first means a marshalling failure happens while we can still
// choose to send a 500 instead. The cost is holding one response in memory, which is
// irrelevant for JSON API payloads (and if a response were ever large enough for that
// to matter, it should be streamed and paginated, not buffered).
func writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(payload); err != nil {
		logging.FromContext(r.Context()).Error("encoding json response failed",
			"error", err,
			"status_intended", status,
		)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to encode response")
		return
	}

	// Content-Type MUST be set before WriteHeader. The header map is written to the
	// wire when WriteHeader is called, so anything set afterwards is silently
	// discarded -- the response simply arrives without it, with no error anywhere to
	// explain why. A very common and very confusing net/http mistake.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	// The body is already fully built, so a write failure here means the client
	// disconnected. Nothing useful can be done: we cannot change the status, and
	// logging every client disconnect would be noise.
	_, _ = w.Write(buf.Bytes())
}

// writeError sends a structured error response, attaching the request ID
// automatically so every error is traceable back to its log line.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	body := ErrorBody{Error: ErrorDetail{
		Code:      code,
		Message:   message,
		RequestID: middleware.RequestIDFromContext(r.Context()),
	}}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		// ErrorBody contains only strings, so this is unreachable in practice. We
		// still avoid recursing into writeJSON: a fallback that can fail the same
		// way it was called is an infinite loop waiting to happen.
		http.Error(w, `{"error":{"code":"internal_error","message":"internal server error"}}`,
			http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
