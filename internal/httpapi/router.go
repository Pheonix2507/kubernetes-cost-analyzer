package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/httpapi/middleware"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/metrics"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/pricing"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/recommend"
)

// RouterOptions carries the router's dependencies.
//
// A struct rather than six positional arguments: NewRouter had grown to four and was about to reach
// seven, at which point every call site is a puzzle and inserting a parameter silently reorders any
// call whose types happen to line up.
type RouterOptions struct {
	Log       *slog.Logger
	Readiness *health.Aggregator
	Inventory Inventory
	Pricer    pricing.Provider
	Reports   Reports
	Stats     Stats
	// Clusters lists the fleet. Also consulted by the currency guard on every aggregating
	// endpoint, which is why it is a dependency of more than just GET /api/v1/clusters.
	Clusters Clusters
	// Trends reads the daily rollup and the monthly statements. A fourth read interface rather than a
	// wider one, so a handler depends only on the queries it calls.
	Trends Trends
	// Recommender is the rule engine. Injected rather than constructed here, so its thresholds are an
	// operator's decision made once in main rather than a default buried in the HTTP layer.
	Recommender *recommend.Engine

	// Metrics is this process's own instrumentation. Injected rather than a package global, so a test
	// gets a fresh registry and two tests cannot see each other's counts.
	//
	// Nil DISABLES instrumentation rather than panicking, which is what lets every existing handler test
	// keep constructing a router without one. A metric nobody scrapes is not worth making mandatory.
	Metrics *metrics.API

	// APIKeys enables authentication. Empty disables it, which config.Validate permits only
	// outside production.
	APIKeys []string
	// RateLimitPerSecond and RateLimitBurst bound requests per client. Zero disables limiting.
	RateLimitPerSecond float64
	RateLimitBurst     int
}

// NewRouter builds the complete HTTP handler: routes plus the middleware chain.
//
// WHY THE STANDARD LIBRARY ServeMux AND STILL NOT chi
// --------------------------------------------------
// Phase 0 chose ServeMux and predicted that Phase 5 would need chi, on the reasoning that auth and
// rate limiting would require route GROUPS with per-group middleware while the probes stayed open.
//
// Phase 5 arrived and that prediction was WRONG, which is worth recording rather than quietly
// dropping. The exemption turned out to be a property of two PATHS, not of a route group -- so both
// middlewares check a small set of exempt paths and every route shares one chain. That is simpler
// than sub-muxes, keeps the exemption list in one obvious place, and means there is still no
// framework between the request and the handler.
//
// What WOULD justify chi: per-route middleware that genuinely varies -- an admin group needing
// stronger auth, or a write endpoint needing a different rate limit. Adding it then is a change to
// this file alone, because chi is 100% http.Handler-compatible.
func NewRouter(opts RouterOptions) http.Handler {
	log, readiness, inv, pricer := opts.Log, opts.Readiness, opts.Inventory, opts.Pricer
	mux := http.NewServeMux()

	// Method-qualified patterns mean a POST to /healthz gets an automatic 405 Method
	// Not Allowed rather than being silently treated as a GET.
	mux.HandleFunc("GET /healthz", handleLive())
	mux.HandleFunc("GET /readyz", handleReady(readiness))
	mux.HandleFunc("GET /version", handleVersion())

	// /metrics: the Prometheus scrape endpoint.
	//
	// NOT UNDER /api/v1, deliberately. It is not part of the versioned contract a client consumes -- it is
	// an operational surface whose shape is Prometheus's exposition format, and versioning it would imply
	// we might offer a v2 of a format we do not define.
	//
	// It is also NOT in middleware.UnauthenticatedPaths. Metric names and label values describe internal
	// topology -- route patterns, and how many namespaces failed -- so an unauthenticated /metrics on a
	// public ingress is reconnaissance. Prometheus can present a bearer token via a ServiceMonitor, so
	// there is no kubelet-style excuse for exempting it.
	if opts.Metrics != nil {
		// The handler comes from the registry, which owns how it is exposed -- see metrics.Registry.Handler.
		// Both binaries serve /metrics, and building the handler twice would be two places for the
		// ErrorHandling choice to diverge.
		mux.Handle("GET "+middleware.MetricsPath(), opts.Metrics.Handler(log))
	}

	// Versioned from the very first endpoint. Adding /v1 later means either breaking
	// every client or maintaining an unversioned alias forever, and "we will version it
	// when we need to" is how you end up doing both.
	//
	// These are all GET and all read-only, which mirrors the RBAC in deploy/rbac: this
	// service is structurally incapable of mutating the cluster it observes.
	// Listed with inventory rather than under costs, because it answers "what is out there".
	// Unlike the other three it reads POSTGRES, not the informer cache: the informers watch only
	// the cluster this process runs in, whereas the fleet is whatever has reported into the
	// database. That distinction is the whole reason a central API can serve many clusters.
	// The fleet port, substituted rather than nil-checked in four handlers. See unknownFleet.
	clusters := opts.Clusters
	if clusters == nil {
		log.Warn("no cluster repository wired: the mixed-currency guard is INACTIVE, " +
			"so a fleet reporting in more than one currency would be summed silently")
		clusters = unknownFleet{}
	}

	mux.HandleFunc("GET /api/v1/clusters", handleListClusters(clusters))
	mux.HandleFunc("GET /api/v1/nodes", handleListNodes(inv, pricer))
	mux.HandleFunc("GET /api/v1/namespaces", handleListNamespaces(inv))
	mux.HandleFunc("GET /api/v1/pods", handleListPods(inv))

	// The cost endpoints: the reason the rest of this exists.
	//
	// /costs/summary is aggregated and is what a dashboard renders. /allocations is the raw fact
	// rows behind it, cursor-paginated -- the escape hatch for "that figure looks wrong, show me
	// what it was computed from".
	mux.HandleFunc("GET /api/v1/costs/summary", handleCostSummary(opts.Reports, clusters))
	mux.HandleFunc("GET /api/v1/allocations", handleAllocations(opts.Reports, clusters))

	// Advice, as distinct from data. The endpoint that says what to CHANGE.
	mux.HandleFunc("GET /api/v1/recommendations", handleRecommendations(opts.Stats, opts.Recommender, clusters))

	// History. /trend is cost THROUGH TIME, served from the daily rollup -- the same question
	// /costs/summary answers for a single period, asked repeatedly. /reports/monthly is the frozen
	// statement, which is a different thing again: not "what is true now" but "what did we say".
	mux.HandleFunc("GET /api/v1/costs/trend", handleTrend(opts.Trends, clusters))
	mux.HandleFunc("GET /api/v1/reports/monthly", handleMonthlyReports(opts.Trends))

	// MIDDLEWARE ORDER IS A CORRECTNESS CONCERN, NOT A STYLE CHOICE.
	// The first entry is outermost; a request travels down the list and back up.
	//
	//  1. RequestID     -- first, so EVERYTHING downstream (including a 401 and a panic) has a
	//                      correlation ID to log against.
	//  2. RequestLogger -- outside Recover, so it observes the 500 that Recover writes. Inside it,
	//                      a panicking request would produce no access-log line at all.
	//  3. RateLimit     -- BEFORE auth, deliberately. Key validation is a SHA-256 per configured
	//                      key, so an unauthenticated flood would otherwise make us do real work
	//                      per request before rejecting it. Throttling first means the cheapest
	//                      possible response to abuse. The limiter falls back to the peer address
	//                      when no key is presented, so unauthenticated callers are still bounded.
	//  4. APIKeyAuth    -- before the handlers, so no handler ever runs unauthenticated.
	//  5. Recover       -- innermost, wrapping the handlers whose panics we are catching.
	//
	// Both RateLimit and APIKeyAuth exempt /healthz and /readyz. That is not convenience: the
	// kubelet cannot present a key, and a 401 or 429 on a probe reads as a failure and kills the
	// container -- so either control would take the service down the moment it was enabled.
	chain := []middleware.Middleware{
		middleware.RequestID,
		middleware.RequestLogger(log),
	}
	// Metrics sits INSIDE RequestLogger and OUTSIDE Recover, and the position is load-bearing.
	//
	// Outside Recover, so a panicking request is recorded as the 5xx that Recover writes. Inside it and a
	// panic would never reach the counter -- so the error rate would be lowest exactly when the service
	// was at its worst, which is the most dangerous possible direction for a metric to be wrong.
	//
	// Inside RateLimit and APIKeyAuth, so a rejected request IS counted: a flood of 429s and a wave of
	// 401s are both things you want visible as traffic rather than silently dropped before measurement.
	if opts.Metrics != nil {
		chain = append(chain, middleware.Metrics(opts.Metrics))
	}
	chain = append(chain,
		middleware.RateLimit(middleware.RateLimitConfig{
			RequestsPerSecond: opts.RateLimitPerSecond,
			Burst:             opts.RateLimitBurst,
		}, log),
		middleware.APIKeyAuth(opts.APIKeys, log),
		middleware.Recover(log),
	)

	return middleware.Chain(mux, chain...)
}
