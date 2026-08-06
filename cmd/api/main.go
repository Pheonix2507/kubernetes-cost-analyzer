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
//     instance.
//
//     PHASE 7 DID NOT ADD LEADER ELECTION HERE, and the earlier claim that it would is
//     corrected rather than dropped. Phase 7 put a Postgres advisory lock in cmd/rollup,
//     where "try once, exit if held" is exactly right for a batch job. A CONTINUOUS
//     process needs renewal, observation and a TTL -- a coordination.k8s.io Lease and an
//     RBAC grant this service deliberately does not have. Until Phase 10 owns the Helm
//     chart the correct answer is a single-replica Deployment, which is a deployment
//     decision rather than a code one. See the same note in cmd/collector.
//
//   - FAILURE BLAST RADIUS: a collector bug that exhausts memory scraping a huge
//     cluster should not take the dashboard down with it.
//
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

	"golang.org/x/sync/errgroup"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/config"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/httpapi"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/kube"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/logging"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/pricing"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/recommend"
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

	// Kubernetes client. Dual-mode: in-cluster credentials when running as a pod,
	// kubeconfig on a laptop. See internal/kube.RESTConfig.
	restCfg, err := kube.RESTConfig(cfg.Kube)
	if err != nil {
		return fmt.Errorf("building kubernetes rest config: %w", err)
	}
	clientset, err := kube.NewClientset(restCfg)
	if err != nil {
		return fmt.Errorf("building kubernetes clientset: %w", err)
	}
	// Constructed here, STARTED below. Registering informers is cheap and cannot fail;
	// starting them opens watches and spawns goroutines, which we only want to do once
	// every dependency has been constructed successfully.
	store := kube.NewStore(clientset, cfg.Kube, logger)
	logger.Info("kubernetes client configured", "host", restCfg.Host, "qps", cfg.Kube.QPS)

	// Pricing catalogue. Loaded and fully validated HERE, at startup, so a typo in the
	// catalogue is a refusal to start with a clear message -- not a cost report that
	// silently prices half the fleet at zero. Same principle as config.Load.
	catalogue, err := pricing.LoadCatalogueFile(cfg.Pricing.CataloguePath)
	if err != nil {
		return fmt.Errorf("loading pricing catalogue: %w", err)
	}
	pricer := pricing.NewCatalogueProvider(catalogue, logger)
	logger.Info("pricing catalogue loaded",
		"path", cfg.Pricing.CataloguePath,
		"currency", catalogue.Currency,
		"regions", len(catalogue.Regions),
		"cpu_share", catalogue.Split.CPU,
		"memory_share", catalogue.Split.Memory,
		"has_fallback", catalogue.Fallback != nil,
	)

	// The aggregator receives db and store as health.Checkers. It has no idea one is Postgres
	// and the other an informer cache.
	//
	// Prometheus is deliberately NOT a readiness dependency of the API, even though
	// prom.Client implements health.Checker. The API serves inventory and (from Phase 5) cost
	// data out of Postgres; it never queries Prometheus. Adding a check for a dependency this
	// process does not use would take it out of service during a monitoring outage it is
	// entirely unaffected by. The COLLECTOR is the process that depends on Prometheus.
	readiness := health.NewAggregator(2*time.Second, db, store)

	// -------------------------------------------------------------------------
	// 5. Run the HTTP server and the informers together.
	// -------------------------------------------------------------------------
	// An unauthenticated API must never be silent about it. config.Validate refuses this in
	// production, so reaching here with no keys means development -- but a warning on every start
	// is what stops a development setting quietly reaching somewhere it should not.
	if len(cfg.API.APIKeys) == 0 {
		logger.Warn("API AUTHENTICATION IS DISABLED: no API_KEYS configured",
			"env", cfg.Env,
			"note", "anyone who can reach this port can read cost data; refused outright when APP_ENV=production",
		)
	} else {
		logger.Info("api authentication enabled", "configured_keys", len(cfg.API.APIKeys))
	}

	// One repository serving both the Reports and Stats interfaces. Two narrow interfaces over one
	// implementation: each handler declares only what it needs, and there is still one place that
	// knows how to talk to the database.
	reportRepo := postgres.NewReportRepository(db.Pool())
	// A SECOND repository, not a wider first one. RollupRepository reads
	// container_allocations_daily and monthly_reports; nothing served by reportRepo touches either.
	// Folding them together would make every existing handler depend on tables it never reads, and
	// would give the trend queries access to the fact-table SQL they must not fall back to silently.
	rollupRepo := postgres.NewRollupRepository(db.Pool())

	router := httpapi.NewRouter(httpapi.RouterOptions{
		Log:       logger,
		Readiness: readiness,
		Inventory: store,
		Pricer:    pricer,
		Reports:   reportRepo,
		Stats:     reportRepo,
		// The SAME pool, a different repository. The trend endpoint reads container_allocations_daily
		// and monthly_reports, which no other handler touches -- so it gets its own repository rather
		// than widening reportRepo with queries its other callers would never issue.
		Trends:             rollupRepo,
		Recommender:        recommend.NewEngine(recommend.DefaultThresholds()),
		APIKeys:            cfg.API.APIKeys,
		RateLimitPerSecond: cfg.API.RateLimitPerSecond,
		RateLimitBurst:     cfg.API.RateLimitBurst,
	})
	srv := httpapi.NewServer(cfg.API, logger, router)

	// errgroup, not sync.WaitGroup. The difference matters here:
	//
	//   - A WaitGroup only tells you when goroutines have FINISHED. It carries no error
	//     and cannot stop siblings.
	//   - errgroup.WithContext gives a derived context that is CANCELLED as soon as any
	//     goroutine returns a non-nil error, and Wait returns that first error.
	//
	// So if informer sync fails, gctx is cancelled, the HTTP server shuts down
	// gracefully, and run() returns the real cause. Without that coupling we would sit
	// there serving an empty inventory -- reporting a cluster that costs nothing.
	g, gctx := errgroup.WithContext(ctx)

	// WHY THE SERVER STARTS BEFORE THE CACHES ARE WARM
	//
	// The tempting order is: sync informers, then start listening. It is wrong in
	// Kubernetes. Cache sync on a large cluster can take tens of seconds, and during
	// that window nothing would be listening on :8080 -- so the LIVENESS probe gets
	// connection refused, and once it exceeds failureThreshold the kubelet kills the
	// container. It restarts, starts syncing again, and is killed again: a
	// CrashLoopBackOff caused entirely by startup ordering.
	//
	// Listening immediately means probes get answered from the first moment. Liveness
	// passes (the process is fine), readiness fails with "informer caches not yet
	// synced" (we genuinely cannot serve yet), and the pod joins Service endpoints the
	// instant the caches are warm. This is the liveness/readiness split from
	// internal/health paying off in the startup path.
	g.Go(func() error {
		return srv.Run(gctx)
	})

	g.Go(func() error {
		// Blocks until every cache has completed its initial List, or the timeout
		// expires. A failure here aborts the whole process via gctx.
		if err := store.Start(gctx); err != nil {
			return fmt.Errorf("starting kubernetes informers: %w", err)
		}
		// Returning nil retires this goroutine without cancelling gctx. The informers
		// themselves keep running in their own goroutines until gctx is done.
		return nil
	})

	if err := g.Wait(); err != nil {
		return fmt.Errorf("running api: %w", err)
	}

	// Distinguish a requested shutdown from an unexpected return. context.Canceled
	// here means "a signal asked us to stop", which is a success, not a failure.
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	logger.Info("shutdown complete")
	return nil
}
