// Command api serves the Kubernetes Cost Analyzer HTTP API.
//
// WHY cmd/api AND NOT A SINGLE BINARY
// -----------------------------------
// The API and the collector have genuinely different operational shapes, and forcing
// them into one process couples decisions that should stay independent:
//
//   - SCALING: the API scales with user traffic. The collector must NOT -- running
//     two collectors means every cost sample is written twice, and the numbers are
//     silently wrong. The API wants an HPA; the collector wants exactly one active
//     instance (Phase 7 adds leader election for HA).
//   - FAILURE BLAST RADIUS: a collector bug that exhausts memory scraping a huge
//     cluster should not take the dashboard down with it.
//   - RESOURCES: the API is latency-sensitive and mostly idle. The collector is a
//     periodic CPU and memory spike. Sizing one container for both means either
//     wasting money at idle or being throttled during collection.
//
// Splitting them now costs one extra directory. Splitting them later, after shared
// in-process state has grown between them, costs a refactor.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/config"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/httpapi"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/logging"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// main is deliberately five lines. All real work is in run().
//
// WHY THE run() error PATTERN
// ---------------------------
// os.Exit terminates the process IMMEDIATELY and DOES NOT RUN DEFERRED FUNCTIONS.
// So this does not work:
//
//	func main() {
//	    db, _ := postgres.New(...)
//	    defer db.Close()          // never runs
//	    if err != nil { os.Exit(1) }  // skips the defer above
//	}
//
// Every cleanup silently stops happening: connections are not drained, buffers are
// not flushed, temp files are not removed. Keeping os.Exit confined to main, with all
// logic in a function that RETURNS, means defers always run. It also makes the
// startup sequence testable, because run() is an ordinary function.
func main() {
	if err := run(); err != nil {
		// Written to stderr directly rather than through slog: this path includes
		// config failures that happen BEFORE a logger exists.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// -------------------------------------------------------------------------
	// 1. Configuration. First, because everything else depends on it, and because
	//    a misconfigured process should die now rather than half-started.
	// -------------------------------------------------------------------------
	cfg, err := config.Load()
	if err != nil {
		return err // already descriptive and may wrap several problems at once
	}

	// -------------------------------------------------------------------------
	// 2. Logger. Second, so every subsequent step can be observed.
	// -------------------------------------------------------------------------
	logger := logging.New(logging.Options{
		Level: cfg.LogLevel,
		// JSON in production for machine parsing; human-readable text locally.
		JSON: cfg.IsProduction(),
		// Source locations cost a runtime.Caller per record, so they are a
		// development-only convenience.
		AddSource: !cfg.IsProduction(),
		Output:    os.Stderr,
		// Version and commit on EVERY line, so any log we examine later can be tied
		// to the exact build that produced it.
		Attrs: append([]any{"service", "api"}, buildinfo.LogAttrs()...),
	})
	logger.Info("starting", "build", buildinfo.String(), "env", cfg.Env)

	// -------------------------------------------------------------------------
	// 3. Signal handling. Set up BEFORE any long-lived resource exists, so that a
	//    Ctrl-C during startup is still handled cleanly.
	//
	//    signal.NotifyContext converts an OS signal into context cancellation,
	//    which is the idiom that lets the same ctx propagate a shutdown request to
	//    every goroutine in the process without any of them knowing about signals.
	//
	//    SIGTERM is what Kubernetes sends first when terminating a pod; SIGINT is
	//    Ctrl-C locally. Handling SIGTERM is what makes zero-downtime deploys
	//    possible: ignore it and the kubelet SIGKILLs us after the grace period,
	//    dropping every in-flight request on every deploy.
	// -------------------------------------------------------------------------
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop() // restores default signal handling; a second Ctrl-C then kills us

	// -------------------------------------------------------------------------
	// 4. Dependencies. Constructed here and INJECTED downwards.
	//
	//    This is dependency injection, and it needs no framework -- it is just
	//    passing arguments. main is the only place that knows which concrete
	//    implementations are in use; everything below receives interfaces. That is
	//    what makes the rest of the codebase testable without a database.
	// -------------------------------------------------------------------------
	db, err := postgres.New(ctx, cfg.Database)
	if err != nil {
		// Reached only for a malformed DATABASE_URL. An unreachable database does
		// NOT fail here by design -- see the long comment in postgres.New.
		return fmt.Errorf("initialising database: %w", err)
	}
	// Runs after the HTTP server has fully shut down, because defers unwind in LIFO
	// order and the server's shutdown happens inside srv.Run below. Closing the pool
	// while requests are still in flight would fail those requests.
	defer func() {
		logger.Info("closing database pool")
		db.Close()
	}()

	// The readiness aggregator receives db as a health.Checker. It has no idea it is
	// talking to Postgres, which is why adding the Kubernetes and Prometheus checks
	// in later phases will be a change to THIS LINE ONLY.
	readiness := health.NewAggregator(2*time.Second, db)

	// -------------------------------------------------------------------------
	// 5. Wire and run the HTTP server.
	// -------------------------------------------------------------------------
	router := httpapi.NewRouter(logger, readiness)
	srv := httpapi.NewServer(cfg.API, logger, router)

	// Blocks until ctx is cancelled by a signal, or the listener fails.
	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("running api server: %w", err)
	}

	// Distinguish a requested shutdown from an unexpected return. context.Canceled
	// here means "a signal asked us to stop", which is a success, not a failure.
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	logger.Info("shutdown complete")
	return nil
}
