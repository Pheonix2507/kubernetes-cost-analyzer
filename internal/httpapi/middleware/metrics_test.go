package middleware

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/metrics"
)

// WHAT THESE TESTS ARE ACTUALLY FOR
// =================================
// Not "does the counter increment" -- that is client_golang's job. The thing worth testing is the LABEL
// SET, because a careless label is the one bug here whose cost lands on the monitoring system rather than
// on this service, and it is invisible until Prometheus starts struggling weeks later.
//
// So the assertions are about cardinality: the same route with different query strings must produce ONE
// series, and an unmatched path must not produce one per probe.

// countSeries reports how many distinct label combinations a metric has.
//
// testutil.CollectAndCount, rather than parsing the exposition text. It counts children of a metric
// vector, which is exactly the quantity that costs Prometheus memory -- and it fails to compile if the
// metric name is wrong, unlike a string search over scraped output.
func countSeries(t *testing.T, m *metrics.API, name string) int {
	t.Helper()
	return testutil.CollectAndCount(m.HTTPRequests, name)
}

// serve runs a request through the metrics middleware against a trivial mux.
//
// A REAL http.ServeMux, not a bare handler, and that is essential rather than incidental: r.Pattern is
// populated by the MUX when it matches a route. A test that wrapped a plain HandlerFunc would see an
// empty pattern for every request, so it would pass while proving nothing about the label.
func serve(m *metrics.API, method, target string, status int) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pods", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	})
	mux.HandleFunc("GET /api/v1/costs/summary", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	})

	h := Metrics(m)(mux)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
}

// TestMetrics_PathParametersDoNotCreateSeries is THE cardinality test, and it replaces one that was
// asserting the wrong thing.
//
// THE TEST THIS REPLACES claimed that labelling by r.URL.Path would make every query string a new series.
// It would not: net/http parses a URL into Path and RawQuery, so r.URL.Path for
// `/api/v1/pods?namespace=team-a` is just `/api/v1/pods`. A mutation -- swapping r.Pattern for r.URL.Path
// -- left the series count unchanged, so the test passed for no reason. The common wisdom I was repeating
// is simply false about query strings.
//
// The real risk is a PATH PARAMETER, and it is worse than the query-string version would have been because
// pod names churn: a Deployment rollout replaces every one of them, so the label set turns over completely
// on each deploy and Prometheus holds each dead series in memory for hours afterwards.
//
// This API has no path parameters today. That is precisely why this test exists now: the first route that
// adds one would otherwise be the change that breaks Prometheus, and nobody would connect the two.
func TestMetrics_PathParametersDoNotCreateSeries(t *testing.T) {
	t.Parallel()

	m := metrics.NewAPI()

	// A route WITH a wildcard, which is what makes this test able to fail.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pods/{name}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := Metrics(m)(mux)

	// Twenty pod names, as a Deployment rollout would produce.
	for _, name := range []string{
		"api-7d9f-a1", "api-7d9f-b2", "api-7d9f-c3", "api-7d9f-d4", "api-7d9f-e5",
		"api-7d9f-f6", "api-7d9f-g7", "api-7d9f-h8", "api-7d9f-i9", "api-7d9f-j0",
		"api-8e0g-a1", "api-8e0g-b2", "api-8e0g-c3", "api-8e0g-d4", "api-8e0g-e5",
		"api-8e0g-f6", "api-8e0g-g7", "api-8e0g-h8", "api-8e0g-i9", "api-8e0g-j0",
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/pods/"+name, nil))
	}

	if got := countSeries(t, m, "kca_http_requests_total"); got != 1 {
		t.Errorf("20 requests to one parameterised route produced %d series, want 1.\n"+
			"Labelling by r.URL.Path gives 20, and every rollout replaces all of them -- so the label "+
			"set turns over completely on each deploy while Prometheus keeps the dead series in memory",
			got)
	}

	// And the label is the PATTERN, with the wildcard intact rather than a pod name.
	out := gather(t, m)
	if !strings.Contains(out, `route="GET /api/v1/pods/{name}"`) {
		t.Errorf("expected the pattern with its wildcard as the label; got:\n%s",
			firstMatchingLine(out, "kca_http_requests_total"))
	}
	if strings.Contains(out, "api-7d9f") {
		t.Error("a pod name leaked into a label value")
	}
}

// firstMatchingLine finds a metric line for an error message.
func firstMatchingLine(out, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return "(not found)"
}

// TestMetrics_UnmatchedPathsCollapseToOneSeries covers the hostile case.
//
// A scanner probing /wp-admin, /.env, /admin.php and a few thousand friends must not be able to grow our
// memory footprint by making requests we already reject. Labelling an unmatched request with its path
// would make cardinality an ATTACKER-CONTROLLED quantity -- a denial of service against the monitoring
// system, delivered through 404s.
func TestMetrics_UnmatchedPathsCollapseToOneSeries(t *testing.T) {
	t.Parallel()

	m := metrics.NewAPI()

	for _, probe := range []string{
		"/wp-admin", "/.env", "/admin.php", "/.git/config", "/actuator/env",
		"/api/v2/does-not-exist", "/%2e%2e/etc/passwd", "/phpmyadmin",
	} {
		serve(m, http.MethodGet, probe, http.StatusOK)
	}

	if got := countSeries(t, m, "kca_http_requests_total"); got != 1 {
		t.Errorf("8 probes of nonexistent paths produced %d series, want 1 (all labelled \"unmatched\").\n"+
			"Labelling by path would make cardinality attacker-controlled: a scanner could exhaust "+
			"Prometheus's memory using requests we already refuse", got)
	}

	// And the label value is the fixed string, not any of the paths.
	out := gather(t, m)
	if !strings.Contains(out, `route="unmatched"`) {
		t.Error(`expected route="unmatched"; the fixed label is what bounds this case`)
	}
	for _, leak := range []string{"wp-admin", ".env", "phpmyadmin"} {
		if strings.Contains(out, leak) {
			t.Errorf("the probed path %q leaked into a label value", leak)
		}
	}
}

// TestMetrics_StatusClassIsBucketed pins five values instead of forty.
//
// Nothing pages on the difference between a 502 and a 503, and the exact code is in the access log with a
// request ID attached. Keeping it as a label would multiply every series by the number of codes actually
// seen -- which is traffic-dependent, so a new client sending malformed requests would grow the metric.
func TestMetrics_StatusClassIsBucketed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code int
		want string
	}{
		{200, "2xx"}, {201, "2xx"}, {204, "2xx"},
		{301, "3xx"}, {304, "3xx"},
		{400, "4xx"}, {401, "4xx"}, {404, "4xx"}, {429, "4xx"},
		{500, "5xx"}, {502, "5xx"}, {503, "5xx"},
	}
	for _, tt := range tests {
		if got := statusClass(tt.code); got != tt.want {
			t.Errorf("statusClass(%d) = %q, want %q", tt.code, got, tt.want)
		}
	}

	// Twelve distinct codes across one route collapse to four series, one per class.
	m := metrics.NewAPI()
	for _, tt := range tests {
		serve(m, http.MethodGet, "/api/v1/pods", tt.code)
	}
	if got := countSeries(t, m, "kca_http_requests_total"); got != 4 {
		t.Errorf("12 distinct status codes produced %d series, want 4 (one per class)", got)
	}
}

// TestMetrics_DurationHistogramOmitsStatus pins the deliberate asymmetry with the counter.
//
// The counter carries status_class and the histogram does not. Adding it would multiply the histogram's
// series by five -- 9 routes x 12 buckets x 5 classes rather than 9 x 12 -- for a question nobody asks:
// "how long did our 404s take" is not a latency objective.
func TestMetrics_DurationHistogramOmitsStatus(t *testing.T) {
	t.Parallel()

	m := metrics.NewAPI()
	// Same route, two very different outcomes.
	serve(m, http.MethodGet, "/api/v1/pods", http.StatusOK)
	serve(m, http.MethodGet, "/api/v1/pods", http.StatusInternalServerError)

	// The counter separates them.
	if got := countSeries(t, m, "kca_http_requests_total"); got != 2 {
		t.Errorf("counter has %d series, want 2 (2xx and 5xx)", got)
	}
	// The histogram does not.
	if got := testutil.CollectAndCount(m.HTTPDuration, "kca_http_request_duration_seconds"); got != 1 {
		t.Errorf("histogram has %d series, want 1: adding status_class would multiply it by five "+
			"for a question nobody asks", got)
	}
}

// TestMetrics_ScrapeEndpointDoesNotMeasureItself keeps the request rate meaningful.
//
// Prometheus scrapes every 30 seconds. On an otherwise idle service that would be the DOMINANT entry in
// the request rate, so "requests per second" would mostly describe Prometheus watching us -- and the
// latency histogram would carry a request whose cost scales with the number of metrics rather than with
// anything a user did.
func TestMetrics_ScrapeEndpointDoesNotMeasureItself(t *testing.T) {
	t.Parallel()

	m := metrics.NewAPI()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+metricsPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := Metrics(m)(mux)
	for range 5 {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, metricsPath, nil))
	}

	if got := countSeries(t, m, "kca_http_requests_total"); got != 0 {
		t.Errorf("the scrape endpoint recorded %d series about itself, want 0", got)
	}
}

// TestMetrics_InFlightReturnsToZero guards a leak that would be permanent.
//
// The gauge is incremented on entry and decremented by a defer. If a handler panicked and the decrement
// were not deferred, in-flight would climb forever -- and it is the saturation signal, so the metric that
// tells you the service is overloaded would eventually claim it always is. A gauge that only goes up is
// worse than no gauge.
func TestMetrics_InFlightReturnsToZero(t *testing.T) {
	t.Parallel()

	m := metrics.NewAPI()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pods", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// A panicking route, to prove the defer holds. Recover is NOT in this chain, so the panic escapes --
	// which is exactly the case the defer must survive.
	mux.HandleFunc("GET /api/v1/boom", func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	})

	h := Metrics(m)(mux)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil))

	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/boom", nil))
	}()

	if got := testutil.ToFloat64(m.HTTPInFlight); got != 0 {
		t.Errorf("in-flight = %v after a normal AND a panicking request, want 0.\n"+
			"A non-deferred decrement leaks on every panic, and the saturation signal would "+
			"eventually claim permanent overload", got)
	}
}

// TestResponseRecorder_ForwardsFlush covers the wrapper trap.
//
// Embedding http.ResponseWriter promotes only the methods in that interface. http.Flusher is a SEPARATE
// interface a handler discovers by type assertion, so a wrapper that does not implement it makes
// `w.(http.Flusher)` fail -- and a streaming handler silently falls back to buffering.
//
// The failure is invisible: the response is still correct, only the timing changes. Nothing in this API
// streams today, which is exactly why this needs a test rather than a comment -- the bug would be
// introduced by whoever adds the first streaming endpoint, and discovered by a user watching a progress
// indicator that never moves.
func TestResponseRecorder_ForwardsFlush(t *testing.T) {
	t.Parallel()

	var flushed bool
	rec := &responseRecorder{ResponseWriter: &flushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: &flushed}}

	// The assertion a streaming handler performs.
	f, ok := any(rec).(http.Flusher)
	if !ok {
		t.Fatal("responseRecorder does not implement http.Flusher; a streaming handler's type " +
			"assertion would fail and it would buffer silently")
	}
	f.Flush()
	if !flushed {
		t.Error("Flush did not reach the underlying writer")
	}

	// And Unwrap, which is how http.ResponseController reaches past this for anything the standard
	// library adds later. Implementing every optional interface by hand does not survive Go adding another.
	u, ok := any(rec).(interface{ Unwrap() http.ResponseWriter })
	if !ok {
		t.Fatal("responseRecorder does not implement Unwrap; http.ResponseController cannot see past it")
	}
	if u.Unwrap() == nil {
		t.Error("Unwrap returned nil")
	}
}

// flushRecorder records whether Flush reached the bottom of the wrapper chain.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed *bool
}

func (f *flushRecorder) Flush() { *f.flushed = true }

// gather renders the registry as exposition text, for assertions about label VALUES.
func gather(t *testing.T, m *metrics.API) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler(slog.New(slog.DiscardHandler)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read exposition output: %v", err)
	}
	return string(body)
}
