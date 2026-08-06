// Command collector periodically computes cost and writes it to PostgreSQL.
//
// It is a separate process from cmd/api for reasons that are operational rather than
// aesthetic:
//
//   - SCALING. The API scales with user traffic. The collector must NOT: two collectors would
//     compute every sample twice. The upsert makes that harmless rather than catastrophic --
//     the second write updates the first rather than duplicating it -- but they would still
//     double the Prometheus query load for no benefit.
//
//     PHASE 7 DID NOT ADD LEADER ELECTION HERE, and the earlier note claiming it would is
//     corrected rather than quietly dropped. Phase 7 added a Postgres advisory lock to
//     cmd/rollup, where the semantics fit exactly: a batch job tries the lock, and exits
//     successfully if another run holds it.
//
//     A CONTINUOUS process needs a different pattern. "Try once, exit if held" would put the
//     second replica into CrashLoopBackOff; a follower has to keep waiting and take over when
//     the leader dies, which is renewal, observation and a TTL -- that is what
//     client-go's leaderelection package and a coordination.k8s.io Lease are for, and it needs
//     an RBAC grant this service deliberately does not have.
//
//     Until then the correct answer is simpler and is a DEPLOYMENT decision rather than a code
//     one: run this as a single-replica Deployment. Phase 10 owns the Helm chart, so Phase 10 is
//     where "exactly one active collector" is either enforced by replicas: 1 or earned with a
//     Lease. Writing the harder mechanism before the chart exists would be building the answer
//     to a question nothing is yet asking.
//
//   - BLAST RADIUS. A collector bug that exhausts memory scraping a huge cluster should not
//     take the dashboard down with it.
//
//   - RESOURCE SHAPE. The API is latency-sensitive and mostly idle; the collector is a
//     periodic CPU and memory spike. One container sized for both is either wasteful at idle
//     or throttled during collection.
package main

import (
	"context"
	"errors"
	"flag"
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
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/kube"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/logging"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/metrics"
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
	// Before run(), which loads configuration. See the note in cmd/api/main.go and
	// buildinfo.PrintVersionAndExit: a version request has to work in a pod whose config is
	// wrong, because that is the pod you are asking about.
	showVersion := flag.Bool("version", false, "print build information and exit")
	flag.Parse()
	if *showVersion {
		buildinfo.PrintVersionAndExit()
	}

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
	logger.Info("pricing catalogue loaded",
		"path", cfg.Pricing.CataloguePath,
		"currency", catalogue.Currency,
		"cpu_share", catalogue.Split.CPU,
		"memory_share", catalogue.Split.Memory,
		"has_fallback", catalogue.Fallback != nil,
	)

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

	w := newWriter(db, store, cfg.ClusterName, logger)

	// This process's own instrumentation, and its readiness aggregator.
	//
	// Both constructed here and injected, not reached for as globals. See internal/metrics for why an
	// explicit registry matters: a package-level one is shared by every test in the binary.
	appMetrics := metrics.NewCollector()

	// The collector's dependencies are the database, Prometheus and the informer cache -- the same three
	// the API checks, minus nothing. A collector that cannot reach Prometheus cannot collect, so it should
	// say so at a URL rather than only in a log line.
	readiness := health.NewAggregator(2*time.Second, db, postgres.NewSchemaChecker(db.Pool()), promClient, store)

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
		return collectLoop(gctx, engine, w, pricer, cfg, logger, synced, appMetrics)
	})

	// The observability listener: /metrics for Prometheus, /healthz and /readyz for the kubelet.
	//
	// Started ALONGSIDE the collection loop rather than after it, and before the informers are warm, for
	// the same reason cmd/api listens before its caches sync: a probe that gets connection-refused during
	// a slow startup is a probe failure, and enough of those means the kubelet kills a container that was
	// working perfectly. Liveness passes immediately, readiness reports honestly until the dependencies
	// are actually up.
	g.Go(func() error {
		return runObservabilityServer(gctx, observabilityServer(
			cfg.Collector.HTTPAddr, logger, appMetrics, readiness,
		), logger)
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
	pricer *pricing.CatalogueProvider, cfg *config.Config, logger *slog.Logger,
	synced <-chan struct{}, m *metrics.Collector,
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
	runCycle(ctx, engine, w, pricer, cfg, logger, m)

	for {
		select {
		case <-ctx.Done():
			logger.Info("collector loop stopping")
			if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil

		case <-ticker.C:
			runCycle(ctx, engine, w, pricer, cfg, logger, m)
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
	ctx context.Context, engine *costing.Engine, w *writer, pricer *pricing.CatalogueProvider,
	cfg *config.Config, logger *slog.Logger, m *metrics.Collector,
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
			// NOT counted as a failure. A cycle interrupted by SIGTERM did not fail -- the operator
			// stopped it -- and counting it would make every rolling deploy look like an incident, which
			// is how an error metric stops being trusted.
			return
		}
		logger.Error("collection cycle failed", "window", window.String(), "error", err)
		m.ObserveCycle(metrics.OutcomeFailed, time.Since(started), 0, nil)
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
		// FAILED even though Collect succeeded. The cost was computed and then lost, which for this
		// process is indistinguishable from never having computed it: nothing downstream can read a
		// result that was not persisted.
		m.ObserveCycle(metrics.OutcomeFailed, time.Since(started), 0, nil)
		return
	}

	// FALLBACK COUNT IS REPORTED EVERY CYCLE, because "how much of this bill is guessed?" is
	// otherwise unanswerable in practice.
	//
	// pricing tracks it, but nothing was reading it -- so a cluster whose entire fleet was
	// priced from the fallback rate would have produced confident-looking numbers with the
	// caveat visible only in a per-node database column nobody queries. Surfacing it on the
	// cycle log makes an estimated bill obvious at a glance, and Phase 9 turns it into the
	// metric this really wants to be.
	summary := append(result.Summary(),
		"duration_ms", time.Since(started).Milliseconds(),
		"fallback_priced_nodes", pricer.FallbackCount(),
	)
	logger.Info("collection cycle complete", summary...)

	// PARTIAL when any namespace failed, and the distinction is the whole reason for a third outcome.
	//
	// internal/costing deliberately isolates a failing namespace and carries on, so a cycle can finish
	// having measured less of the cluster than it should. Recording that as "success" would make the
	// isolation a way of HIDING problems rather than surviving them -- and crucially, ObserveCycle only
	// advances the last-success timestamp for a clean cycle, so a persistently partial collector still
	// trips the freshness alert instead of looking healthy forever.
	outcome := metrics.OutcomeSuccess
	if len(result.NamespaceErrors) > 0 {
		outcome = metrics.OutcomePartial
	}
	m.ObserveCycle(outcome, time.Since(started), len(result.Allocations), classifyNamespaceErrors(result.NamespaceErrors))

	m.FallbackPricedNodes.Set(float64(pricer.FallbackCount()))
}

// classifyNamespaceErrors buckets per-namespace failures into a BOUNDED set of reasons.
//
// WHY NOT LABEL BY NAMESPACE, WHICH IS THE OBVIOUS THING
// ----------------------------------------------------
// A namespace name is unbounded over time in any cluster with per-pull-request preview environments:
// each one creates and destroys a namespace, so the label churns even though the count at any instant is
// small. Prometheus keeps a series in memory for hours after its last sample, so churn costs more than
// breadth -- and the affected namespace is already named in a WARN log line with the full error, which is
// detail no label could carry anyway.
//
// The useful question a metric answers is "what KIND of thing is failing": a Prometheus timeout needs a
// different response from a query rejection, and both need a different response from a context deadline.
func classifyNamespaceErrors(errs map[string]error) map[string]int {
	if len(errs) == 0 {
		return nil
	}
	out := make(map[string]int, 3)
	for _, err := range errs {
		out[classifyError(err)]++
	}
	return out
}

// classifyError maps an error to one of a fixed set of reasons.
//
// A CLOSED SET, checked in order of specificity. The default is "other" rather than the error's text:
// error strings contain namespace names, query fragments and driver detail, so using one as a label value
// is the unbounded-cardinality bug arriving through the back door -- and it is the version people miss,
// because it does not look like a label decision.
func classifyError(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.DeadlineExceeded):
		// The Prometheus query outlived its timeout. Usually means Prometheus is overloaded rather than
		// that anything is wrong here.
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "other"
	}
}
