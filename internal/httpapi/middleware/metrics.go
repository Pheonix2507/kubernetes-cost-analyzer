package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/metrics"
)

// Metrics records RED — Rate, Errors, Duration — for every request.
//
// WHY THE ROUTE LABEL COMES FROM r.Pattern
// ========================================
// This is the decision that keeps the metric affordable, and getting it wrong is the commonest way a team
// destroys their own Prometheus. It is also where I first wrote down a WRONG reason, which is worth
// recording because the wrong reason is the one everybody repeats.
//
// THE CLAIM I MADE AND HAD TO WITHDRAW: that labelling by r.URL.Path would make every query string a new
// series. It would not. net/http parses a URL into Path and RawQuery, so r.URL.Path for
// `/api/v1/pods?namespace=team-a` is just `/api/v1/pods` -- the query string is not in it. A mutation test
// caught this: swapping r.Pattern for r.URL.Path did not change the series count, so the test asserting
// otherwise was passing for no reason.
//
// The genuinely unbounded sources are these, and r.Pattern handles both:
//
//	PATH PARAMETERS. `GET /api/v1/pods/{name}` matched against /api/v1/pods/api-7d9f-x2k gives an
//	r.URL.Path containing the pod name -- so one series per pod, churning on every deploy. r.Pattern gives
//	the literal `GET /api/v1/pods/{name}`. This API has no path parameters TODAY, which is exactly why
//	getting it right now matters: the first route that adds one would otherwise be the one that breaks
//	Prometheus, and nobody would connect the two.
//
//	UNMATCHED PATHS. A scanner probing /wp-admin, /.env and a thousand friends produces an r.URL.Path per
//	probe. Those requests are already rejected, so labelling them by path would let an attacker grow our
//	memory footprint using requests we refuse -- cardinality as an attacker-controlled quantity.
//
// r.Pattern is the pattern the ServeMux MATCHED, so the ceiling is the number of registered routes --
// bounded by the router itself, which is the only kind of bound worth relying on: it cannot be exceeded by
// anything a client sends.
//
// Go 1.23 added it. Before that this needed either a framework exposing its route table or a
// hand-maintained path-to-pattern map, and the map is where the bug lives -- a route added to the router
// and forgotten in the map falls silently into the default label.
func Metrics(m *metrics.API) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The /metrics endpoint does not measure itself.
			//
			// Not for tidiness: a scrape every 30 seconds would be the dominant entry in the request rate
			// for an otherwise idle service, so "requests per second" would mostly describe Prometheus
			// watching us. Worse, its duration would pollute the latency histogram with a request whose
			// cost scales with the number of metrics rather than with anything a user did.
			if r.URL.Path == metricsPath {
				next.ServeHTTP(w, r)
				return
			}

			m.HTTPInFlight.Inc()
			defer m.HTTPInFlight.Dec()

			start := time.Now()
			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			// The pattern, resolved AFTER the handler has run.
			//
			// It has to be after: the mux populates r.Pattern when it matches, and this middleware runs
			// before the mux has looked. Reading it up front would give an empty string on every request.
			route := r.Pattern
			if route == "" {
				// An unmatched request -- a 404 for a path no route claims. Labelled with a FIXED string
				// rather than the path, because this is exactly the unbounded case: a scanner probing
				// /wp-admin, /.env and /admin.php would otherwise mint a series per probe, and an attacker
				// could grow our memory footprint by making requests we already rejected.
				route = "unmatched"
			}

			m.HTTPRequests.WithLabelValues(route, r.Method, statusClass(rec.status)).Inc()
			// The duration histogram carries no status label -- see the note in internal/metrics. It takes
			// this metric from ~540 series to ~108, and "how long did our 404s take" is not a question
			// anyone asks.
			m.HTTPDuration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
		})
	}
}

// metricsPath is where the registry is exposed. Shared with the router so the exclusion above and the
// route registration cannot disagree.
const metricsPath = "/metrics"

// MetricsPath is the exposed endpoint, for the router.
func MetricsPath() string { return metricsPath }

// statusClass buckets a status code into 2xx/3xx/4xx/5xx.
//
// FIVE VALUES INSTEAD OF FORTY, and the reduction is the point rather than a side effect. Alerts ask
// "are we serving errors", not "are we serving 502s specifically" -- nothing pages on the difference
// between a 502 and a 503. The exact code is in the access log with a request ID attached, which is
// where you go once an alert has told you to look.
//
// Keeping the exact code would multiply every series by the number of codes actually seen, which is
// itself traffic-dependent: a new client sending malformed requests would grow the metric.
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		// 1xx, and anything a handler invents. Bucketed rather than passed through, so an unexpected code
		// cannot become an unexpected label value.
		return strconv.Itoa(code/100) + "xx"
	}
}

// NOTE: this middleware uses the package's shared responseRecorder rather than defining its own.
//
// An earlier version of this file declared a second, near-identical wrapper. Two wrappers in one package
// is worse than duplication: one of them documented that it did not forward http.Flusher and the other
// did, so which behaviour a request got depended on which middleware happened to be outermost. The
// shared type now forwards Flush and provides Unwrap, which closes the limitation the original honestly
// flagged and left open.
//
// Both middlewares still create their own wrapper, so a request passing through both is wrapped twice.
// That costs one small allocation and buys INDEPENDENCE: either middleware works without the other, and
// neither has to know the chain order to find a recorder somebody else installed. Coupling two
// middlewares through a shared mutable object to save an allocation per request is a bad trade.
