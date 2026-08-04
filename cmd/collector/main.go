// Command collector periodically gathers resource usage and computes cost.
//
// PHASE 0 STATUS: this binary is a working skeleton with no collection logic. It
// loads config, builds a logger, connects to the database, runs a ticker loop and
// shuts down gracefully -- and does nothing else.
//
// WHY BUILD IT EMPTY NOW RATHER THAN LATER
// ----------------------------------------
// The alternative is to start collection inside cmd/api "just for now". That decision
// is very hard to reverse: within a phase or two the collector shares in-process
// caches, goroutines and lifecycle with the API, and separating them means untangling
// all of it while also writing the new cost logic. Establishing the process boundary
// while it is free means Phase 4 only has to fill in a loop body.
//
// See the header of cmd/api/main.go for why the two are separate processes at all.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/config"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/logging"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := logging.New(logging.Options{
		Level:     cfg.LogLevel,
		JSON:      cfg.IsProduction(),
		AddSource: !cfg.IsProduction(),
		Output:    os.Stderr,
		Attrs:     append([]any{"service", "collector"}, buildinfo.LogAttrs()...),
	})
	logger.Info("starting",
		"build", buildinfo.String(),
		"env", cfg.Env,
		"interval", cfg.Collector.Interval,
		"workers", cfg.Collector.Workers,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := postgres.New(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("initialising database: %w", err)
	}
	defer func() {
		logger.Info("closing database pool")
		db.Close()
	}()

	if err := collectLoop(ctx, cfg.Collector, logger); err != nil {
		return fmt.Errorf("collection loop: %w", err)
	}

	logger.Info("shutdown complete")
	return nil
}

// collectLoop ticks on an interval until ctx is cancelled.
//
// WHY time.NewTicker AND NOT `for { work(); time.Sleep(interval) }`
// -----------------------------------------------------------------
// Two reasons, and both matter:
//
//  1. time.Sleep is UNINTERRUPTIBLE. A 5-minute sleep ignores SIGTERM for up to five
//     minutes, so the kubelet SIGKILLs the pod long before it notices. A ticker in a
//     select alongside ctx.Done() reacts immediately.
//  2. Sleep DRIFTS. sleep(5m) after work that took 30s produces a 5m30s cycle, so
//     collection times wander across the hour and cost buckets stop lining up with
//     wall-clock boundaries. A ticker fires on a fixed schedule.
//
// A ticker's own trade-off: if work outlasts the interval, ticks are DROPPED rather
// than queued (the channel has capacity 1). That is the right default here -- we want
// the next collection, not a backlog of stale ones -- but it must be observable, so
// Phase 4 will record skipped cycles as a metric rather than letting them pass
// silently.
func collectLoop(ctx context.Context, cfg config.Collector, logger *slog.Logger) error {
	ticker := time.NewTicker(cfg.Interval)
	// Stopping a ticker releases its runtime timer. Omitting this leaks the timer for
	// the lifetime of the process.
	defer ticker.Stop()

	logger.Info("collector loop started", "interval", cfg.Interval)

	for {
		select {
		case <-ctx.Done():
			// A cancelled context is the NORMAL exit path here, not a failure, so we
			// return nil. Returning ctx.Err() would make every clean shutdown exit
			// non-zero and look like a crash to Kubernetes.
			logger.Info("collector loop stopping")
			if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil

		case <-ticker.C:
			// PHASE 4 fills this in: list pods via informers, query Prometheus for
			// usage, compute cost, write allocation rows. For now it only proves the
			// loop and the shutdown path work.
			logger.Info("collection cycle skipped: no collectors registered yet")
		}
	}
}
