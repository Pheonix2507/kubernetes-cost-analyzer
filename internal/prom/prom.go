// Package prom reads observed resource consumption out of Prometheus.
//
// WHY PROMETHEUS AND NOT metrics-server
// -------------------------------------
// metrics-server holds only the latest scrape -- roughly a 15-second window, in memory, with
// no history. It powers `kubectl top` and the HPA and is the right tool for both. It cannot
// answer "what did this container use between 09:00 and 09:05 yesterday", which is the only
// question cost cares about.
//
// THE TWO METRICS THAT MATTER, AND WHY THEY ARE READ DIFFERENTLY
// -------------------------------------------------------------
// CPU and memory are fundamentally different kinds of measurement, and treating them alike is
// the most common way to get usage wrong:
//
//	container_cpu_usage_seconds_total   a COUNTER -- cumulative CPU-seconds burned since the
//	                                    container started. Monotonically increasing, and reset
//	                                    to zero on restart. Meaningless read directly; you
//	                                    want its RATE.
//	container_memory_working_set_bytes  a GAUGE -- how much memory is resident right now.
//	                                    Meaningful read directly, but a single instant is a
//	                                    poor summary of a five-minute window, so we AVERAGE it.
//
// Reading the counter directly would report a container's entire lifetime CPU as this window's
// usage. Taking a rate of the gauge would report bytes-per-second, which is not a thing.
package prom

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/config"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
)

// Compile-time proof that the client can serve as a readiness dependency.
var _ health.Checker = (*Client)(nil)

// Client queries Prometheus.
type Client struct {
	api     promv1.API
	timeout time.Duration
	log     *slog.Logger
}

// NewClient builds a Prometheus client.
func NewClient(cfg config.Prometheus, log *slog.Logger) (*Client, error) {
	apiClient, err := promapi.NewClient(promapi.Config{Address: cfg.URL})
	if err != nil {
		return nil, fmt.Errorf("building prometheus client for %s: %w", cfg.URL, err)
	}
	return &Client{
		api:     promv1.NewAPI(apiClient),
		timeout: cfg.Timeout,
		log:     log,
	}, nil
}

// Name implements health.Checker.
func (c *Client) Name() string { return "prometheus" }

// Check implements health.Checker with the cheapest possible query.
//
// `up` is a metric Prometheus always has if it is scraping anything at all, and asking for it
// exercises the full path -- HTTP, query parsing, the TSDB -- without touching cost data or
// scanning a large series set. A heavier probe query would make readiness expensive, and
// readiness is polled every few seconds forever.
func (c *Client) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// Not time.Now(): a caller-independent instant is not available, and Prometheus needs an
	// evaluation timestamp. Using the current time is correct here -- we are asking "are you
	// answering queries now".
	_, warnings, err := c.api.Query(ctx, "up", time.Now())
	if err != nil {
		return fmt.Errorf("prometheus query failed: %w", err)
	}
	if len(warnings) > 0 {
		// Warnings are NOT failures. Prometheus emits them for partial results and deprecated
		// syntax, and treating them as errors would take a healthy replica out of service over
		// a deprecation notice.
		c.log.Debug("prometheus returned warnings on the health query", "warnings", warnings)
	}
	return nil
}

// ContainerUsage returns per-container usage over the window.
//
// Two queries, and the range selector is set to the window length so each sample summarises
// exactly the window being priced -- no more and no less.
func (c *Client) ContainerUsage(ctx context.Context, namespace string, start, end time.Time) (map[domain.ContainerKey]domain.Usage, error) {
	window := end.Sub(start)
	if window <= 0 {
		return nil, fmt.Errorf("window end %s is not after start %s", end.Format(time.RFC3339), start.Format(time.RFC3339))
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	usage := make(map[domain.ContainerKey]domain.Usage)

	cpuQuery := cpuQuery(namespace, window)
	if err := c.collect(ctx, cpuQuery, end, func(k domain.ContainerKey, v float64) {
		u := usage[k]
		// The query returns CORES (a rate of CPU-seconds per second). Multiplying by 1000
		// gives millicores, matching the unit used everywhere else in this codebase.
		//
		// Rounded rather than truncated: truncation would report every container using under
		// 0.5 millicores as exactly zero, and the idle-service fixture depends on being
		// distinguishable from a container that genuinely reports nothing.
		u.CPUMillicores = int64(math.Round(v * 1000))
		usage[k] = u
	}); err != nil {
		return nil, fmt.Errorf("querying cpu usage: %w", err)
	}

	memQuery := memoryQuery(namespace, window)
	if err := c.collect(ctx, memQuery, end, func(k domain.ContainerKey, v float64) {
		u := usage[k]
		u.MemoryBytes = int64(math.Round(v))
		usage[k] = u
	}); err != nil {
		return nil, fmt.Errorf("querying memory usage: %w", err)
	}

	return usage, nil
}

// cpuQuery builds the CPU usage query.
//
// EVERY PART OF THIS SELECTOR IS LOAD-BEARING:
//
//	rate(...[window])   the metric is a COUNTER of cumulative CPU-seconds. rate() converts it
//	                    to cores, and crucially it HANDLES COUNTER RESETS -- a container
//	                    restart zeroes the counter, and a naive difference would read as a
//	                    huge negative number.
//	container!=""       cAdvisor emits a POD-LEVEL aggregate series with an empty container
//	                    label, in ADDITION to one series per container. Omitting this filter
//	                    double-counts every pod's usage -- the single easiest way to produce
//	                    numbers that are exactly 2x too high.
//	container!="POD"    older cAdvisor labelled the pause container "POD". Harmless to keep and
//	                    it protects against an older kubelet.
//	sum by (...)        a container has one series PER CPU CORE on some cAdvisor versions, so
//	                    the series must be summed to get the container's total.
func cpuQuery(namespace string, window time.Duration) string {
	return fmt.Sprintf(
		`sum by (namespace, pod, container) (`+
			`rate(container_cpu_usage_seconds_total{container!="",container!="POD"%s}[%s])`+
			`)`,
		namespaceSelector(namespace), promDuration(window))
}

// memoryQuery builds the memory usage query.
//
// avg_over_time on a GAUGE, not rate(): the metric is already a level, so a rate would give
// bytes per second, which is meaningless. Averaging summarises the window, which is what cost
// integrates over.
func memoryQuery(namespace string, window time.Duration) string {
	return fmt.Sprintf(
		`sum by (namespace, pod, container) (`+
			`avg_over_time(container_memory_working_set_bytes{container!="",container!="POD"%s}[%s])`+
			`)`,
		namespaceSelector(namespace), promDuration(window))
}

// namespaceSelector adds a namespace matcher, or nothing for a cluster-wide query.
//
// The namespace is interpolated into the query string, which would be an injection risk if the
// value were untrusted -- PromQL has no bind parameters, so there is no safe alternative to
// interpolation. It is safe here because the value comes from the informer cache, meaning the
// Kubernetes API server has already validated it as a DNS-1123 label. The defensive check
// below makes that reasoning explicit rather than implicit, so a future caller passing a
// user-supplied string cannot silently break it.
func namespaceSelector(namespace string) string {
	if namespace == "" {
		return ""
	}
	if !isDNS1123Label(namespace) {
		// Dropping the selector is safer than injecting a malformed one: a cluster-wide query
		// returns MORE than asked for, which the caller then filters, whereas a broken
		// selector could match unintended series or fail the query outright.
		return ""
	}
	return fmt.Sprintf(`,namespace="%s"`, namespace)
}

// isDNS1123Label mirrors the check in internal/httpapi. Duplicated deliberately rather than
// shared: this package must not import the HTTP layer, and a six-line validator is a smaller
// cost than an inverted dependency or a third package existing only to hold it.
func isDNS1123Label(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			continue
		case c == '-':
			if i == 0 || i == len(s)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// promDuration renders a duration in PromQL's syntax.
//
// PromQL accepts "5m" and "30s" but NOT Go's "1h0m0s" or "1.5m". time.Duration.String()
// produces exactly those invalid forms, so it cannot be used directly -- a mistake that
// surfaces as a query parse error at runtime rather than a compile error.
func promDuration(d time.Duration) string {
	seconds := int64(d.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return fmt.Sprintf("%ds", seconds)
}

// collect runs an instant query and invokes fn for each returned series.
//
// The query is evaluated at `at`, which is the WINDOW END. Combined with a range selector of
// the window length, that means each sample covers exactly [start, end) -- the same half-open
// interval the fact table uses.
func (c *Client) collect(ctx context.Context, query string, at time.Time, fn func(domain.ContainerKey, float64)) error {
	value, warnings, err := c.api.Query(ctx, query, at)
	if err != nil {
		return fmt.Errorf("query %q at %s: %w", query, at.Format(time.RFC3339), err)
	}
	if len(warnings) > 0 {
		c.log.Warn("prometheus returned warnings", "warnings", warnings, "query", query)
	}

	// An instant query returns a Vector. A type assertion rather than a blind cast: a
	// malformed query can return a Scalar or a String, and asserting without the ok form
	// would panic on it.
	vector, ok := value.(model.Vector)
	if !ok {
		return fmt.Errorf("query %q returned %T, want a vector (this usually means the query is malformed)",
			query, value)
	}

	for _, sample := range vector {
		// NaN and Inf must be dropped, not converted.
		//
		// Prometheus produces NaN for a rate over insufficient data -- a container that started
		// mid-window has fewer than two samples. int64(NaN) is IMPLEMENTATION-DEFINED in Go
		// and in practice yields a huge negative number, which would then be stored as a
		// negative cost and reduce someone's bill. Skipping the series means the container is
		// simply treated as having no observed usage, and max(request, usage) still bills it
		// correctly on its request.
		v := float64(sample.Value)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		// Negative usage is physically impossible and indicates a counter-reset artefact.
		if v < 0 {
			continue
		}

		key := domain.ContainerKey{
			Namespace: string(sample.Metric["namespace"]),
			Pod:       string(sample.Metric["pod"]),
			Container: string(sample.Metric["container"]),
		}
		// A series missing any identifying label cannot be attributed to anything, and
		// attributing it to the empty pod would create a phantom row.
		if key.Namespace == "" || key.Pod == "" || key.Container == "" {
			continue
		}
		fn(key, v)
	}

	return nil
}

// Describe returns the queries this client issues, for diagnostics.
//
// Exposed so an operator can paste them straight into Prometheus' expression browser when the
// numbers look wrong. "Which query produced this figure" should never require reading the
// source.
func Describe(namespace string, window time.Duration) []string {
	return []string{cpuQuery(namespace, window), memoryQuery(namespace, window)}
}
