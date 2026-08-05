package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/httpapi/middleware"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/pricing"
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

	// Versioned from the very first endpoint. Adding /v1 later means either breaking
	// every client or maintaining an unversioned alias forever, and "we will version it
	// when we need to" is how you end up doing both.
	//
	// These are all GET and all read-only, which mirrors the RBAC in deploy/rbac: this
	// service is structurally incapable of mutating the cluster it observes.
	mux.HandleFunc("GET /api/v1/nodes", handleListNodes(inv, pricer))
	mux.HandleFunc("GET /api/v1/namespaces", handleListNamespaces(inv))
	mux.HandleFunc("GET /api/v1/pods", handleListPods(inv))

	// The cost endpoints: the reason the rest of this exists.
	//
	// /costs/summary is aggregated and is what a dashboard renders. /allocations is the raw fact
	// rows behind it, cursor-paginated -- the escape hatch for "that figure looks wrong, show me
	// what it was computed from".
	mux.HandleFunc("GET /api/v1/costs/summary", handleCostSummary(opts.Reports))
	mux.HandleFunc("GET /api/v1/allocations", handleAllocations(opts.Reports))

	// MIDDLEWARE ORDER IS A CORRECTNESS CONCERN, NOT A STYLE CHOICE.
	// The first entry is outermost; a request travels down the list and back up.
	//
	//  1. RequestID     -- must be first so EVERYTHING downstream, including the
	//                      panic logger, has a correlation ID to attach.
	//  2. RequestLogger -- outside Recover, so it observes the 500 that Recover
	//                      writes. Inside Recover, a panicking request would produce
	//                      no access-log line at all, and the request would vanish
	//                      from the logs precisely when you most need it.
	//  3. Recover       -- innermost, wrapping the handler whose panics we are
	//                      catching.
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
	return middleware.Chain(mux,
		middleware.RequestID,
		middleware.RequestLogger(log),
		middleware.RateLimit(middleware.RateLimitConfig{
			RequestsPerSecond: opts.RateLimitPerSecond,
			Burst:             opts.RateLimitBurst,
		}, log),
		middleware.APIKeyAuth(opts.APIKeys, log),
		middleware.Recover(log),
	)
}
