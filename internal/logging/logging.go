// Package logging builds the application's structured logger and carries it
// through request scope via context.
//
// WHY STRUCTURED LOGGING, NOT fmt.Printf
// --------------------------------------
// A line like:
//
//	failed to collect costs for namespace team-payments: timeout after 30s
//
// is readable by a human and useless to a machine. You cannot ask it "how many
// collection failures per namespace in the last hour?" without writing a regex that
// breaks the next time someone rewords the message.
//
// Structured logging emits the same event as key-value pairs:
//
//	{"level":"error","msg":"collection failed","namespace":"team-payments","error":"timeout"}
//
// Now it is queryable. That single property is what makes logs an observability
// signal rather than a diary, and it is why we standardise on it from the first
// commit -- retrofitting structure onto a codebase full of Printf is miserable.
//
// WHY log/slog RATHER THAN zap OR zerolog
// ---------------------------------------
// slog is in the standard library (Go 1.21+). zap and zerolog are measurably faster
// in allocation-heavy benchmarks, and that difference matters if you are logging on
// a per-packet hot path. We are not: this service logs per HTTP request and per
// collection cycle, where the cost is irrelevant next to a database round trip.
// In exchange we get no dependency to keep updated, and a logger that every library
// in the ecosystem is converging on via slog.Handler.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Options configures the logger.
//
// This package deliberately does NOT import our config package, even though main
// will populate these fields straight from it. Depending on config would mean this
// package could never be reused without dragging the whole configuration schema
// along, and it would create a needless edge in the dependency graph. Accepting
// plain primitives keeps logging a leaf package.
type Options struct {
	// Level is one of "debug", "info", "warn", "error". Unrecognised values fall
	// back to info: a bad log level should not prevent the service from starting,
	// because then you lose the very logs that would tell you why. config.Validate
	// already rejects invalid levels at startup, so this is defence in depth.
	Level string

	// JSON selects the machine-readable handler. True in production (so Loki,
	// Elasticsearch or CloudWatch can parse fields), false locally where a human is
	// reading the terminal.
	JSON bool

	// AddSource attaches the file and line that emitted each record. Genuinely
	// useful when hunting a log line's origin, but it costs a runtime.Caller
	// lookup per record, so it is normally enabled only in development.
	AddSource bool

	// Output defaults to os.Stderr when nil.
	//
	// WHY STDERR AND NOT STDOUT: in a container, both are captured by the runtime,
	// but stdout is conventionally the channel for a program's actual OUTPUT.
	// Keeping diagnostics on stderr means a CLI subcommand can pipe real output
	// without logs corrupting it.
	Output io.Writer

	// Attrs are key-value pairs attached to every record from this logger --
	// service name, version, commit. Set once here rather than remembered at
	// thousands of call sites.
	Attrs []any
}

// New builds a *slog.Logger from opts. It never returns an error: a logger is the
// thing you need in order to report problems, so it must not be able to fail.
func New(opts Options) *slog.Logger {
	out := opts.Output
	if out == nil {
		out = os.Stderr
	}

	handlerOpts := &slog.HandlerOptions{
		Level:     ParseLevel(opts.Level),
		AddSource: opts.AddSource,
	}

	// slog.Handler is an interface, which is the extension point that makes slog
	// worth standardising on: swapping the output format, or routing records to a
	// test buffer, or adding sampling, is a handler change and touches no call site.
	var handler slog.Handler
	if opts.JSON {
		handler = slog.NewJSONHandler(out, handlerOpts)
	} else {
		handler = slog.NewTextHandler(out, handlerOpts)
	}

	logger := slog.New(handler)
	if len(opts.Attrs) > 0 {
		logger = logger.With(opts.Attrs...)
	}
	return logger
}

// ParseLevel maps a level name to a slog.Level, defaulting to Info.
func ParseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// -----------------------------------------------------------------------------
// Request-scoped loggers
// -----------------------------------------------------------------------------

// ctxKey is an unexported named type used as the context key.
//
// WHY NOT A PLAIN STRING KEY: context keys are compared by value AND type across
// the whole process, including code in other modules. A string key "logger" can
// collide with an identical key set by a dependency, and one package would silently
// read the other's value. An unexported type cannot be constructed outside this
// package, so collision is impossible by construction. This is the standard Go
// idiom and the reason context.WithValue's key parameter is `any`.
type ctxKey struct{}

// WithLogger returns a copy of ctx carrying l.
//
// This is how a request ID reaches every log line emitted while handling that
// request, WITHOUT threading a logger parameter through every function signature
// down the call stack. The middleware attaches an enriched logger once; anything
// downstream retrieves it.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the logger stored in ctx, or slog.Default() if there is none.
//
// It NEVER returns nil. That is the entire point: a helper that can return nil
// forces `if logger != nil` at every call site, and the one place someone forgets
// is a nil-pointer panic inside an error path -- the code least likely to be
// covered by tests and most likely to run during an incident.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
