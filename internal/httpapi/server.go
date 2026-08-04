package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/config"
)

// Server owns the HTTP listener and its lifecycle.
//
// It is a thin wrapper over *http.Server whose real purpose is to own SHUTDOWN
// correctly, and to be startable/stoppable from main with a single ctx-aware call.
type Server struct {
	http            *http.Server
	log             *slog.Logger
	shutdownTimeout time.Duration
}

// NewServer configures the listener. It does not bind the port; Run does that.
func NewServer(cfg config.API, log *slog.Logger, handler http.Handler) *Server {
	return &Server{
		http: &http.Server{
			Addr:    cfg.Addr,
			Handler: handler,

			// Go's defaults for all of these are ZERO, meaning NO TIMEOUT. A public
			// listener with no timeouts will eventually accumulate connections that
			// never complete -- each holding a goroutine and a file descriptor --
			// until the process runs out of descriptors. This is not a theoretical
			// attack; slow mobile networks produce it accidentally.
			ReadTimeout:  cfg.ReadTimeout,
			WriteTimeout: cfg.WriteTimeout,
			IdleTimeout:  cfg.IdleTimeout,

			// ReadHeaderTimeout specifically bounds how long a client may take to
			// send its request HEADERS. This is the Slowloris defence: an attacker
			// opens many connections and dribbles headers a byte at a time, holding
			// each one open indefinitely. ReadTimeout alone does not cover it in
			// every code path, which is why gosec flags a server without this field
			// set (rule G112).
			ReadHeaderTimeout: 5 * time.Second,

			// Route net/http's own internal errors (TLS handshake failures, malformed
			// requests) into our structured logger. Without this they go to the
			// standard log package and land as unstructured text that no log query
			// will ever match.
			ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
		},
		log:             log,
		shutdownTimeout: cfg.ShutdownTimeout,
	}
}

// Run starts the server and blocks until ctx is cancelled or the server fails.
//
// Cancelling ctx triggers a graceful shutdown. Run returns nil on a clean shutdown,
// so main can distinguish "asked to stop and did so properly" from a real failure.
func (s *Server) Run(ctx context.Context) error {
	// BUFFERED channel, capacity 1. This matters.
	//
	// If it were unbuffered, the goroutine below would block forever trying to send
	// once nobody is receiving -- which is exactly what happens on the shutdown path,
	// where Run has already moved past the select. That goroutine would never exit:
	// a goroutine leak on every shutdown. With capacity 1 the send always completes
	// and the goroutine always returns.
	serveErr := make(chan error, 1)

	go func() {
		s.log.Info("http server listening", "addr", s.http.Addr)

		err := s.http.ListenAndServe()
		// ListenAndServe ALWAYS returns a non-nil error. After a clean Shutdown that
		// error is http.ErrServerClosed, which is a normal, expected outcome and not
		// a failure. Treating it as one would make every graceful shutdown look like
		// a crash in the logs and exit non-zero.
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		// The server stopped on its own. Almost always "address already in use" or a
		// permissions failure on the port.
		if err != nil {
			return fmt.Errorf("http server failed: %w", err)
		}
		return nil

	case <-ctx.Done():
		return s.shutdown(serveErr)
	}
}

// shutdown stops accepting new connections and waits for in-flight requests.
func (s *Server) shutdown(serveErr <-chan error) error {
	s.log.Info("shutting down http server", "grace_period", s.shutdownTimeout)

	// ============================================================================
	// THE SINGLE MOST COMMON GRACEFUL-SHUTDOWN BUG IN GO
	// ============================================================================
	// This context derives from context.Background(), NOT from the ctx passed to Run.
	//
	// We only reach this function BECAUSE that ctx was cancelled. Deriving from it:
	//
	//     shutdownCtx, cancel := context.WithTimeout(ctx, s.shutdownTimeout)  // WRONG
	//
	// produces a context that is ALREADY CANCELLED. http.Shutdown checks it
	// immediately, returns context.Canceled without waiting for anything, and every
	// in-flight request is dropped the instant SIGTERM arrives.
	//
	// The code looks correct, compiles, passes review, and "works" -- shutdown is
	// fast and no error appears. It only shows up as a small number of 502s during
	// every deploy, which teams routinely dismiss as unavoidable. It is not.
	//
	// A cancelled parent can never grant a child a deadline. Shutdown needs its own
	// independent budget, so its parent must be Background.
	// ============================================================================
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()

	// Shutdown closes listeners, then waits for active requests to finish. It does
	// NOT interrupt them: a handler mid-work runs to completion (or until the
	// deadline). It also does not wait for hijacked (WebSocket) connections.
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		// Typically context.DeadlineExceeded: some request outlived the grace
		// period. Worth surfacing loudly -- it means either the grace period is too
		// short or a handler has no internal timeout of its own.
		return fmt.Errorf("graceful shutdown exceeded %s: %w", s.shutdownTimeout, err)
	}

	// Drain the goroutine's result so it is guaranteed to have exited before we
	// return. Without this, Run could return while the serve goroutine is still
	// running, and main might call os.Exit underneath it.
	if err := <-serveErr; err != nil {
		return fmt.Errorf("http server failed during shutdown: %w", err)
	}

	s.log.Info("http server stopped cleanly")
	return nil
}
