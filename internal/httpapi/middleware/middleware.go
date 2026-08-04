// Package middleware provides composable HTTP middleware.
//
// WHY MIDDLEWARE EXISTS AS A CONCEPT
// ----------------------------------
// Certain concerns apply to EVERY request: assigning a correlation ID, logging the
// outcome, surviving a panic, enforcing auth, applying rate limits. Implementing them
// inside each handler means duplicating them in every handler and forgetting them in
// the one that matters. Middleware lifts them out into a chain that every request
// traverses, so a handler contains only its own logic.
//
// THE ENTIRE MECHANISM IS ONE INTERFACE
// -------------------------------------
// Go needs no framework for this because of http.Handler:
//
//	type Handler interface { ServeHTTP(ResponseWriter, *Request) }
//
// A middleware is just a function that takes a Handler and returns a Handler that
// wraps it. Because the wrapper satisfies the same interface, wrappers nest
// arbitrarily and compose with anything in the ecosystem. This is the canonical
// example of Go's preference for small interfaces over inheritance -- and the reason
// middleware written for chi, gorilla or echo all work with the standard library.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/logging"
)

// Middleware wraps an http.Handler with additional behaviour.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware around h.
//
// ORDER: the FIRST middleware listed is the OUTERMOST -- it sees the request first
// and the response last. Reading Chain(mux, A, B, C) top to bottom therefore matches
// the order a request actually traverses: A -> B -> C -> mux, then back out
// C -> B -> A.
//
// The loop runs BACKWARDS to achieve that. Wrapping is inside-out (the last wrap
// applied ends up outermost), so iterating in reverse makes the listed order read
// forwards. Getting this backwards is a genuinely easy mistake and produces subtle
// bugs -- a panic recoverer placed innermost by accident will not catch panics
// raised by the middleware around it.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// -----------------------------------------------------------------------------
// Request ID
// -----------------------------------------------------------------------------

// RequestIDHeader is the header carrying the correlation ID.
const RequestIDHeader = "X-Request-Id"

type requestIDKey struct{}

// RequestID attaches a correlation ID to each request, reusing an inbound one if
// present.
//
// WHY REUSE AN INBOUND ID: in a real deployment a request crosses several services.
// If each minted its own ID, correlating one user action across their logs would be
// impossible. Honouring an upstream ID means one identifier follows the request
// through the entire system, so a single log query reconstructs the whole path. This
// is the cheapest possible form of distributed tracing, and the foundation the real
// thing (Phase 9) builds on.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" {
			id = newRequestID()
		}

		// Echo it back so a client (or the browser network tab) can quote the ID
		// when reporting a problem, turning "it was broken at about 3pm" into an
		// exact log lookup.
		w.Header().Set(RequestIDHeader, id)

		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		// r.WithContext returns a SHALLOW COPY of the request; it does not mutate
		// the original. Requests are per-goroutine values and must be treated as
		// immutable -- mutating the shared one would race with anything else
		// holding it.
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext returns the request ID, or "" if none was set.
func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

// newRequestID returns 16 hex characters of randomness.
//
// crypto/rand rather than math/rand: math/rand is seeded deterministically and is
// predictable, so IDs could collide across replicas that started simultaneously --
// and collisions are precisely the failure that makes correlation IDs useless. In
// Go 1.24+ crypto/rand.Read cannot fail (it panics on a broken OS entropy source
// rather than returning an error), so there is no error to handle here.
func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// -----------------------------------------------------------------------------
// Structured request logging
// -----------------------------------------------------------------------------

// responseRecorder wraps http.ResponseWriter to capture the status code and byte
// count, which the interface itself does not expose.
//
// KNOWN LIMITATION, STATED DELIBERATELY: http.ResponseWriter is an interface, and the
// concrete value the server passes in also implements optional interfaces --
// http.Flusher (streaming), http.Hijacker (WebSocket upgrades), io.ReaderFrom
// (sendfile fast path). Wrapping it in a struct that implements ONLY ResponseWriter
// hides those, so a later WebSocket or SSE endpoint would fail with a confusing
// "not a Hijacker" error pointing at this file.
//
// The fix when we need it is to forward the optional interfaces explicitly (or use
// http.ResponseController, added in Go 1.20, which handles the unwrapping). We have
// no streaming endpoints yet, so the simple version is correct for now -- but this
// is a real trap and worth recognising before it bites.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

// WriteHeader records the status code before delegating to the real writer.
func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Write records the byte count, and infers a 200 for handlers that write a body
// without calling WriteHeader first.
func (r *responseRecorder) Write(b []byte) (int, error) {
	// A handler that writes a body without calling WriteHeader gets an implicit
	// 200 from net/http. We must record the same, or such responses would be
	// logged with status 0.
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// RequestLogger logs one structured line per completed request and puts a
// request-scoped logger into the context.
//
// The context logger is the important half: because it already carries the request
// ID, every log line emitted anywhere downstream is automatically correlated,
// without threading a logger through every function signature.
func RequestLogger(base *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			reqLogger := base.With(
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
			)
			r = r.WithContext(logging.WithLogger(r.Context(), reqLogger))

			rec := &responseRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			if rec.status == 0 {
				rec.status = http.StatusOK
			}

			// Log level follows the outcome, so that "show me the errors" is a level
			// filter and not a text search. 5xx is our fault; 4xx is usually the
			// client's and should not page anyone.
			level := slog.LevelInfo
			switch {
			case rec.status >= 500:
				level = slog.LevelError
			case rec.status >= 400:
				level = slog.LevelWarn
			}

			reqLogger.LogAttrs(r.Context(), level, "http request",
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				// Milliseconds as a float: whole milliseconds lose all resolution on
				// fast endpoints, where every request would log as 0.
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000.0),
				slog.String("remote_addr", r.RemoteAddr),
			)
		})
	}
}

// -----------------------------------------------------------------------------
// Panic recovery
// -----------------------------------------------------------------------------

// Recover converts a panic into a 500 response instead of a dead process.
//
// WHY THIS IS NOT OPTIONAL
// ------------------------
// net/http runs each request in its own goroutine, and an unrecovered panic in ANY
// goroutine terminates the WHOLE PROCESS -- not just that request. So one malformed
// input that triggers a nil-map write in one handler takes down every in-flight
// request on that replica. In Kubernetes the container then restarts, and a client
// that can reproduce the panic can hold the entire deployment in CrashLoopBackOff.
//
// A panic still indicates a bug that must be fixed. This middleware buys the time to
// fix it properly instead of during an outage, which is why it logs the full stack at
// error level rather than swallowing it quietly.
func Recover(base *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}

				// http.ErrAbortHandler is the documented way for a handler to
				// abandon a response deliberately (the standard library's reverse
				// proxy uses it). Re-panicking preserves that contract; logging it
				// as a crash would fill the logs with false alarms.
				//
				// NOTE ON THE TYPE ASSERTION: recover() returns `any`, so a bare
				// `rec == http.ErrAbortHandler` compares an interface{} to an error
				// by identity. That happens to work for this exact sentinel, but it
				// fails the moment the value is wrapped -- and errorlint correctly
				// flags it. Asserting to error first and using errors.Is handles a
				// wrapped sentinel too, and reads as the intent rather than as a
				// pointer comparison that works by luck.
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}

				logging.FromContext(r.Context()).Error("recovered from panic",
					"panic", rec,
					// The stack is the only thing that makes a panic diagnosable
					// after the fact. Captured as a single string field so it stays
					// attached to this record in the log aggregator rather than
					// being split across lines.
					"stack", string(debug.Stack()),
				)

				// The client gets a generic message. A panic value or stack trace
				// can contain SQL, file paths, or internal hostnames, and returning
				// those to a caller is an information-disclosure bug.
				//
				// If the handler already began writing a body, these headers are
				// ignored by net/http and the client sees a truncated response.
				// Nothing can be done about that at this point -- which is another
				// reason respond.go buffers its JSON before writing anything.
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"internal server error"}}`))
			}()

			next.ServeHTTP(w, r)
		})
	}
}
