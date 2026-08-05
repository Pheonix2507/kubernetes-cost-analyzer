package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/httpapi/middleware"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/pricing"
)

// NewRouter builds the complete HTTP handler: routes plus the middleware chain.
//
// WHY THE STANDARD LIBRARY ServeMux AND NOT chi OR gin
// ----------------------------------------------------
// Since Go 1.22, net/http.ServeMux understands method-qualified patterns
// ("GET /readyz") and path wildcards ("GET /api/v1/pods/{name}") with precedence
// rules. For Phase 0's three routes it is entirely sufficient, has zero dependencies,
// and -- more importantly for learning -- there is no framework magic between the
// request and the handler. Understanding that a middleware is a function wrapping an
// interface is much harder when a router hides the composition.
//
// WHAT WOULD JUSTIFY chi LATER: route GROUPS with per-group middleware. In Phase 5
// the public /api/v1 routes will need auth and rate limiting while /healthz and
// /metrics must NOT (a rate-limited liveness probe is a self-inflicted outage, and an
// authenticated one cannot be called by the kubelet). ServeMux has no grouping, so we
// would hand-roll sub-muxes and separate chains. That is the point at which chi earns
// its place -- it is 100% http.Handler-compatible, so this file changes and nothing
// else does.
//
// Adopting a router before that need exists would be choosing a dependency on
// speculation.
func NewRouter(log *slog.Logger, readiness *health.Aggregator, inv Inventory, pricer pricing.Provider) http.Handler {
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
	return middleware.Chain(mux,
		middleware.RequestID,
		middleware.RequestLogger(log),
		middleware.Recover(log),
	)
}
