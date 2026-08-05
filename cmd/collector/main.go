// Command collector periodically computes cost and writes it to PostgreSQL.
//
// It is a separate process from cmd/api for reasons that are operational rather than
// aesthetic:
//
//   - SCALING. The API scales with user traffic. The collector must NOT: two collectors would
//     compute every sample twice. The upsert makes that harmless rather than catastrophic --
//     the second write updates the first rather than duplicating it -- but they would still
//     double the Prometheus query load for no benefit. Phase 7 adds leader election so a
//     highly-available deployment has exactly one ACTIVE collector.
//   - BLAST RADIUS. A collector bug that exhausts memory scraping a huge cluster should not
//     take the dashboard down with it.
//   - RESOURCE SHAPE. The API is latency-sensitive and mostly idle; the collector is a
//     periodic CPU and memory spike. One container sized for both is either wasteful at idle
//     or throttled during collection.
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

	"golang.org/x/sync/errgroup"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/config"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/costing"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/kube"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/logging"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/pricing"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/prom"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// scrapeLag is how long after a window closes we wait before collecting it.
//
// WHY THIS EXISTS
// Prometheus scrapes on its own schedule -- 30 seconds in our kube-prometheus-stack values.
// The instant a window closes at 09:05:00, the samples covering 09:04:30 to 09:05:00 may not
// be ingested yet. Querying immediately would read a window that is complete in wall-clock
// terms but incomplete in the TSDB, and quietly under-report its final seconds.
//
// Set comfortably above the scrape interval. The cost is that data trails real time by about a
// minute, which for cost reporting is irrelevant.
const scrapeLag = 90 * time.Second

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
		"cluster", cfg.ClusterName,
		"interval", cfg.Collector.Interval,
		"workers", cfg.Collector.Workers,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// -------------------------------------------------------------------------
	// Dependencies, constructed and validated before any background work starts.
	// -------------------------------------------------------------------------
	db, err := postgres.New(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("initialising database: %w", err)
	}
	defer func() {
		logger.Info("closing database pool")
		db.Close()
	}()

	restCfg, err := kube.RESTConfig(cfg.Kube)
	if err != nil {
		return fmt.Errorf("building kubernetes rest config: %w", err)
	}
	clientset, err := kube.NewClientset(restCfg)
	if err != nil {
		return fmt.Errorf("building kubernetes clientset: %w", err)
	}
	store := kube.NewStore(clientset, cfg.Kube, logger)

	promClient, err := prom.NewClient(cfg.Prometheus, logger)
	if err != nil {
		return fmt.Errorf("building prometheus client: %w", err)
	}

	catalogue, err := pricing.LoadCatalogueFile(cfg.Pricing.CataloguePath)
	if err != nil {
		return fmt.Errorf("loading pricing catalogue: %w", err)
	}
	pricer := pricing.NewCatalogueProvider(catalogue, logger)

	engine, err := costing.NewEngine(costing.Options{
		ClusterName: cfg.ClusterName,
		Inventory:   store,
		Usage:       promClient,
		Pricer:      pricer,
		Workers:     cfg.Collector.Workers,
		Log:         logger,
	})
	if err != nil {
		return err
	}

	w := newWriter(db, cfg.ClusterName, logger)

	// -------------------------------------------------------------------------
	// Run the informers and the collection loop together.
	//
	// Unlike the API, the collector must NOT begin work before its caches are warm. An
	// unsynced informer returns an empty list with NO ERROR, so a cycle collected from one
	// would write a perfectly plausible window in which the cluster cost nothing -- which is
	// indistinguishable from a genuine drop in spend, and therefore worse than no data at all.
	//
	// So the loop waits for a sync signal instead of starting immediately.
	// -------------------------------------------------------------------------
	g, gctx := errgroup.WithContext(ctx)

	synced := make(chan struct{})
	g.Go(func() error {
		if startErr := store.Start(gctx); startErr != nil {
			return fmt.Errorf("starting kubernetes informers: %w", startErr)
		}
		// CLOSING the channel rather than sending on it. A close is observed by EVERY receiver
		// and can be waited on any number of times, whereas a send has to match the number of
		// waiters exactly -- and a mismatch is either a deadlock or a missed signal. This is
		// the idiomatic Go broadcast.
		close(synced)
		return nil
	})

	g.Go(func() error {
		return collectLoop(gctx, engine, w, cfg, logger, synced)
	})

	if err := g.Wait(); err != nil {
		return fmt.Errorf("running collector: %w", err)
	}

	logger.Info("shutdown complete")
	return nil
}

// collectLoop runs a collection cycle on every tick until ctx is cancelled.
func collectLoop(
	ctx context.Context, engine *costing.Engine, w *writer,
	cfg *config.Config, logger *slog.Logger, synced <-chan struct{},
) error {
	// Wait for the informer caches before the first cycle.
	select {
	case <-synced:
	case <-ctx.Done():
		// Shutdown during startup is a normal exit, not a failure -- the same reasoning as the
		// bug fixed in kube.Store.Start during the Phase 1 audit.
		logger.Info("shutdown requested before the first collection")
		return nil
	}

	// A ticker, not `for { work(); time.Sleep(interval) }`.
	//
	// Sleep is UNINTERRUPTIBLE: a five-minute sleep ignores SIGTERM for up to five minutes, so
	// the kubelet SIGKILLs the pod long before it notices. Sleep also DRIFTS -- 5m after work
	// that took 30s gives a 5m30s cycle, so collection times wander across the hour and stop
	// lining up with the aligned windows they are meant to produce.
	ticker := time.NewTicker(cfg.Collector.Interval)
	// Stopping the ticker releases its runtime timer; omitting this leaks one per process.
	defer ticker.Stop()

	logger.Info("collector loop started",
		"interval", cfg.Collector.Interval, "scrape_lag", scrapeLag)

	// Collect immediately rather than waiting a full interval for the first tick, so a restart
	// costs one window of data rather than two.
	runCycle(ctx, engine, w, cfg, logger)

	for {
		select {
		case <-ctx.Done():
			logger.Info("collector loop stopping")
			if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil

		case <-ticker.C:
			runCycle(ctx, engine, w, cfg, logger)
		}
	}
}

// runCycle collects and persists one window.
//
// IT RETURNS NO ERROR, DELIBERATELY. A failed cycle must not stop the loop: Prometheus being
// briefly unavailable, or the database restarting, should cost one window rather than the whole
// collector. Exiting would also discard the informer cache and force a full resync on restart,
// making a transient failure considerably more expensive than it needs to be.
//
// Failures are logged at ERROR so they are alertable. Phase 9 adds a consecutive-failure
// counter, which is the signal that actually matters -- one failed cycle is noise, twenty in a
// row is an outage.
func runCycle(
	ctx context.Context, engine *costing.Engine, w *writer,
	cfg *config.Config, logger *slog.Logger,
) {
	// Aligned AND lagged: the last interval boundary at least scrapeLag ago. Alignment is what
	// makes the upsert idempotent across restarts; see costing.AlignedWindow.
	window := costing.AlignedWindow(time.Now().Add(-scrapeLag), cfg.Collector.Interval)

	started := time.Now()
	result, err := engine.Collect(ctx, window)
	if err != nil {
		// Cancellation during shutdown is expected and not worth an ERROR line.
		if errors.Is(err, context.Canceled) {
			logger.Info("collection cancelled by shutdown", "window", window.String())
			return
		}
		logger.Error("collection cycle failed", "window", window.String(), "error", err)
		return
	}

	// Per-namespace failures are logged individually so the affected namespace is nameable, but
	// at WARN rather than ERROR: the cycle succeeded for everything else, and treating a partial
	// gap as a service error makes alerting useless.
	for ns, nsErr := range result.NamespaceErrors {
		logger.Warn("namespace excluded from this window", "namespace", ns, "error", nsErr)
	}

	if err := w.write(ctx, result); err != nil {
		logger.Error("persisting allocations failed",
			"window", window.String(), "allocations", len(result.Allocations), "error", err)
		return
	}

	logger.Info("collection cycle complete",
		append(result.Summary(), "duration_ms", time.Since(started).Milliseconds())...)
}
