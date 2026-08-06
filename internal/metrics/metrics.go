// Package metrics is this project's own instrumentation.
//
// WHY THIS PACKAGE EXISTS
// ======================
// There is a pleasing symmetry to it: everything so far has READ Prometheus to work out what a cluster
// costs. This package WRITES to the same Prometheus, so the tool can be operated with the same
// machinery it uses to observe everything else.
//
// The reason it is a package rather than a handful of package-level vars next to the code they measure
// is cardinality. Metric definitions are the one place where a careless label costs the whole
// monitoring system, so they are collected here where they can be reviewed together and where the
// LABEL SETS are visible side by side. A metric defined next to its call site gets its labels chosen by
// whoever was writing that function.
//
// HOW IT COMMUNICATES WITH THE REST OF THE APPLICATION
// A Registry is constructed in main and injected: the HTTP middleware records into it, the collector
// records into it, and the /metrics handler serves it. Nothing reaches for a global.
package metrics

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// namespace prefixes every metric.
//
// A short, unambiguous prefix so `kca_` in a PromQL autocomplete lists exactly this project's metrics
// and nothing else. Prometheus convention is `<namespace>_<subsystem>_<name>_<unit>`, and the unit
// suffix is not decoration -- `_seconds` and `_bytes` and `_total` tell a reader what they are looking
// at without opening the code, and tell Grafana how to format an axis.
const namespace = "kca"

// TWO REGISTRIES, ONE PER BINARY -- AND A BUG IS WHY
// ==================================================
// This was originally a single Registry holding every metric, constructed by both binaries. It shipped a
// false alert, found by reading a real /metrics output rather than by any test.
//
// The API registered the collector's metrics and never set them, so it served:
//
//	kca_collector_last_success_timestamp_seconds 0
//
// The freshness alert is `time() - kca_collector_last_success_timestamp_seconds > 900`. Against 0 -- which
// is 1970 -- that is about 1.8 billion, so the alert would fire permanently from the API's instance while
// the real collector reported a perfectly good value from its own. Prometheus keeps them as separate
// series by job and instance, so the healthy one does not mask the broken one: the page fires forever,
// from a process that does not collect anything.
//
// A metric a process never writes is not neutral. It is an assertion of zero, and zero is a real value
// that alerts act on.
//
// So each binary gets its own type, and the compiler enforces the boundary: the collector cannot touch an
// HTTP histogram and the API cannot pretend to have run a cycle. Sharing the metric NAMES across two
// processes would be fine -- Prometheus distinguishes by job -- but sharing the DEFINITIONS made both
// processes claim to do both jobs.

// base is what both registries share: the underlying registry and how it is exposed.
type base struct {
	reg *prometheus.Registry
}

// Registerer exposes the registry, for tests that want to gather directly.
func (b *base) Registerer() *prometheus.Registry { return b.reg }

// Handler returns the /metrics HTTP handler.
//
// WHY THE REGISTRY OWNS THIS RATHER THAN EACH CALLER BUILDING ITS OWN
// ------------------------------------------------------------------
// Both binaries expose /metrics, and an earlier version had each construct its own promhttp.HandlerFor
// with its own options and its own slog adapter. Two copies of a configuration choice is two places for
// it to diverge -- and the choice that would have diverged is ErrorHandling, which decides whether one
// broken collector blanks every other metric.
//
// ContinueOnError, deliberately: a single failing collector logs and the scrape returns everything else.
// HTTPErrorOnError would return 500, so Prometheus records the whole scrape as failed and one bad metric
// takes out the freshness gauges and the request histogram too. Partial data beats none -- the same
// argument internal/costing makes for a failing namespace.
func (b *base) Handler(log *slog.Logger) http.Handler {
	return promhttp.HandlerFor(b.reg, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
		ErrorLog:      slogErrorLog{log: log},
	})
}

// newBase builds a registry with the standard runtime collectors.
//
// Registered EXPLICITLY rather than inherited from the default registry, which is what makes the exposed
// set exactly what these constructors list:
//
//	Go collector      -- goroutines, GC pauses, heap. `go_goroutines` climbing without bound is the leak
//	                     signature, and both binaries run informers and worker pools that could produce one.
//	Process collector -- open file descriptors, CPU, RSS. `process_open_fds` against its limit is how a
//	                     connection leak shows up before it becomes a refused request.
func newBase() base {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return base{reg: reg}
}

// API holds the metrics cmd/api maintains.
//
// WHY AN EXPLICIT REGISTRY AND NOT promauto WITH THE DEFAULT ONE
// -------------------------------------------------------------
// The one-line version is `promauto.NewCounter(...)` at package scope, which registers into
// prometheus.DefaultRegisterer -- a package-level global. That has the problems every global has, and
// two that bite specifically here:
//
//   - TESTS CANNOT BE ISOLATED. A metric registered at package init exists for the whole test binary,
//     so two tests that both increment it see each other's counts, and the second one to run fails on a
//     number the first one caused. The usual workaround is to reset metrics between tests, which is
//     mutable global state being managed by hand.
//   - DOUBLE REGISTRATION PANICS. Register the same name twice and client_golang panics at init, so a
//     name collision is a crash at startup rather than a compile error -- and with a global registry the
//     collision can come from a dependency you did not know instrumented itself.
//
// An explicit registry makes each test construct its own, and makes the set of exposed metrics
// something you can read in one place.
type API struct {
	base

	// --- RED ---

	// HTTPRequests is the R and the E of RED: rate comes from the counter's derivative, errors from
	// the status_class label.
	//
	// ONE COUNTER FOR BOTH, rather than a separate errors counter. A separate one would need the same
	// labels and would double the series for no new information -- and worse, it makes the error RATIO
	// a division between two metrics that could disagree if one were incremented and the other missed.
	// `sum(rate(...{status_class="5xx"}[5m])) / sum(rate(...[5m]))` is exact by construction.
	HTTPRequests *prometheus.CounterVec

	// HTTPDuration is the D. A histogram, not a summary.
	//
	// WHY HISTOGRAM AND NOT SUMMARY -- the classic Prometheus question, and it has a real answer.
	// A summary computes quantiles IN THE PROCESS, so its p99 is that replica's p99 and quantiles
	// cannot be aggregated: averaging two replicas' p99s is not the p99 of the pair, and there is no
	// arithmetic that recovers it. A histogram ships bucket COUNTS, which are additive, so
	// histogram_quantile over sum(rate(...)) by (le) gives a genuine fleet-wide p99.
	//
	// The cost is that quantiles are interpolated within a bucket, so bucket choice matters -- see
	// httpBuckets.
	HTTPDuration *prometheus.HistogramVec

	// HTTPInFlight is saturation: how many requests are being served right now.
	//
	// A gauge rather than something derived, because "in flight" is a level and not an event. It is the
	// signal that distinguishes "slow" from "overloaded": rising latency with flat in-flight is a slow
	// dependency, rising latency WITH rising in-flight is queueing.
	HTTPInFlight prometheus.Gauge

	// --- Data freshness, read from the database ---

	// FactsLastWindow and RollupLastDay are the newest data present, as timestamps.
	//
	// WHY THESE ARE READ FROM POSTGRES RATHER THAN REPORTED BY THE PROCESS THAT WROTE THEM
	// -----------------------------------------------------------------------------------
	// You cannot scrape a process that has exited. cmd/rollup is a batch job: it runs, writes, and dies
	// -- so by the time Prometheus comes to scrape it there is nothing listening. The canonical answer
	// is a Pushgateway, an extra component whose whole job is to hold a metric on behalf of a dead
	// process.
	//
	// There is a better answer here, and it is better for a reason beyond avoiding a component: the
	// DATABASE ALREADY RECORDS WHAT HAPPENED. `max(rolled_up_at)` on the rollup table is the last
	// successful rollup, and `max(window_start)` on the fact table is the freshest collected data. So
	// the API reads those and exposes them as gauges.
	//
	// That makes the metric a property of the DATA rather than a claim by a process. A Pushgateway
	// reports "the job said it finished"; this reports "the rows are there". The second cannot be
	// wrong about a job that exited zero having written nothing -- which is exactly the failure mode a
	// job's own success metric cannot see.
	FactsLastWindow prometheus.Gauge
	RollupLastDay   prometheus.Gauge

	// --- Cost, deliberately minimal ---

	// ClusterCost and ClusterWaste are the only cost metrics exposed by default.
	//
	// WHY COST IS BARELY IN PROMETHEUS AT ALL
	// --------------------------------------
	// Phase 0 chose Postgres for cost precisely because Prometheus cannot do what cost needs: joins,
	// hierarchy rollups, monthly reports, and exact decimal arithmetic. Prometheus stores float64, and
	// this project went to real trouble to keep money out of floats.
	//
	// So why expose any? Because ALERTING on a cost spike is a genuine want -- "this namespace tripled
	// overnight" is worth knowing at 09:00, and Postgres has no alerting engine. These gauges exist for
	// that and nothing else.
	//
	// CLUSTER-LEVEL ONLY, and that is a cardinality decision rather than laziness. Per-container cost
	// on a 5,000-container cluster is 5,000 series per metric, churning on every deploy. Per-NAMESPACE
	// is tempting and still wrong in the common case: preview environments create and destroy a
	// namespace per pull request, so the label is unbounded over time even though it is small at any
	// instant. Prometheus keeps a series in memory for hours after its last sample, so churn costs more
	// than breadth.
	//
	// POSTGRES REMAINS THE SYSTEM OF RECORD. Two systems holding the same number is only safe if one of
	// them is authoritative, and this is the approximate copy: float64, cluster-grained, for alerting.
	// Anything a human quotes comes from the API.
	ClusterCost  prometheus.Gauge
	ClusterWaste *prometheus.GaugeVec
}

// Collector holds the metrics cmd/collector maintains.
//
// A separate type from API, so the compiler prevents each binary from exposing metrics it never writes.
// See the note above the base type for the false alert that made this necessary.
type Collector struct {
	base

	// CollectorCycles counts completed cycles by outcome.
	CollectorCycles *prometheus.CounterVec

	// CollectorLastSuccess is the timestamp of the last fully successful cycle.
	//
	// THE MOST IMPORTANT METRIC IN THIS FILE, and the reason RED does not fit a batch job.
	//
	// A dead collector emits NOTHING. So `rate(kca_collector_cycles_total[5m]) == 0` never fires --
	// there is no series to evaluate, it just goes stale, and stale series are invisible unless
	// something is looking for absence. Alerting on a bad value only works when a process is alive
	// enough to report a bad value.
	//
	// A timestamp gauge inverts that: `time() - kca_collector_last_success_timestamp_seconds > 900`
	// fires whether the process is slow, wedged, crash-looping or entirely gone, because `time()` keeps
	// advancing regardless. This is the standard pattern for anything periodic and it is the one people
	// leave out.
	//
	// Unix SECONDS as a float64 gauge, which is the Prometheus convention for timestamps -- and it is
	// why the metric name ends `_timestamp_seconds` rather than `_time` or `_at`.
	CollectorLastSuccess prometheus.Gauge

	// CollectorCycleDuration measures how long a cycle takes.
	//
	// Matters because of a specific failure: if a cycle takes longer than the interval, cycles overlap
	// and the collector silently falls behind forever. Comparing this against the configured interval
	// is how that is caught before the backlog is unrecoverable.
	CollectorCycleDuration prometheus.Histogram

	// CollectorAllocations counts fact rows written, and CollectorNamespaceErrors counts namespaces
	// that failed within an otherwise-successful cycle.
	//
	// The second is the interesting one: internal/costing deliberately isolates a failing namespace and
	// continues, so a cycle can SUCCEED while silently covering less of the cluster. Without this
	// metric that partial coverage is invisible, which would make the isolation a way of hiding
	// problems rather than surviving them.
	CollectorAllocations     prometheus.Counter
	CollectorNamespaceErrors *prometheus.CounterVec

	// FallbackPricedNodes is how many nodes were priced from the fallback guess rather than the
	// catalogue.
	//
	// A gauge rather than a counter: it is a LEVEL -- how many nodes are currently unpriced -- not a
	// count of events. A counter would only ever rise, so replacing an unknown instance type with a
	// catalogued one would never show as an improvement.
	//
	// This answers "how much of this bill is guessed?", which the cycle log already reported but nothing
	// could alert on. A fleet entirely on fallback rates produces confident-looking numbers that are
	// arithmetic on an invented price, and the only prior signal was a per-node database column nobody
	// queried.
	FallbackPricedNodes prometheus.Gauge
}

// httpBuckets are the latency buckets for the request histogram.
//
// CHOSEN FOR THIS SERVICE'S ACTUAL LATENCIES rather than copied from a tutorial. The default
// prometheus.DefBuckets top out at 10 seconds and start at 5 milliseconds, which is wrong at both ends
// here: a /healthz is well under a millisecond, and the slowest real query -- a 400-day cost summary --
// is a few hundred milliseconds.
//
// Buckets matter because histogram_quantile INTERPOLATES WITHIN A BUCKET. If every request lands
// between 5ms and 10ms, the p99 is a linear guess across that whole span, and the reported figure moves
// when the traffic mix changes rather than when latency does. Resolution where the data actually lives
// is the whole point.
//
// Twelve buckets, and that count is itself a cardinality decision: buckets multiply by every label
// combination, so 9 routes x 5 status classes x 12 buckets is about 540 series from this one metric.
// Twenty buckets would be 900 for no extra insight.
var httpBuckets = []float64{
	0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5,
}

// collectorBuckets span a cycle's duration.
//
// A cycle is seconds to minutes, so these run from 100ms to 5 minutes. The top bucket is deliberately
// above the default collection interval: the number worth seeing is whether a cycle is approaching the
// interval, and a histogram whose last bucket is below the interval cannot show it.
var collectorBuckets = []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// NewAPI builds the API's registry.
//
// RED for the HTTP surface, the data-freshness gauges read from Postgres, and the small cost gauges. It
// does NOT define the collector's metrics -- see the note on `base` for the false alert that caused.
func NewAPI() *API {
	m := &API{base: newBase()}

	m.HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "http", Name: "requests_total",
		Help: "HTTP requests by route pattern, method and status class.",
	},
		// THE LABEL SET IS THE WHOLE DESIGN OF THIS METRIC.
		//
		// `route` is the registered PATTERN, never the request path -- see the middleware. A path carries
		// query strings and any future path parameter, so labelling by it is unbounded: ?namespace=a and
		// ?namespace=b would be two series, and a client walking namespaces would create one per
		// namespace forever.
		//
		// `status_class` is 2xx/3xx/4xx/5xx rather than the exact code. Five values instead of forty, and
		// it is what alerts actually ask: nothing pages on the difference between a 502 and a 503, and the
		// exact code is in the access log with a request ID attached.
		[]string{"route", "method", "status_class"},
	)

	m.HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "http", Name: "request_duration_seconds",
		Help:    "HTTP request duration by route pattern and method.",
		Buckets: httpBuckets,
	},
		// NO status_class HERE, unlike the counter, and the asymmetry is deliberate. Adding it would
		// multiply this histogram's series by five for a question nobody asks -- "how long did our 404s
		// take" is not a latency objective. Dropping it takes this metric from ~540 series to ~108.
		[]string{"route", "method"},
	)

	m.HTTPInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "http", Name: "requests_in_flight",
		Help: "Requests currently being served. Rising alongside latency means queueing rather than a slow dependency.",
	})

	m.FactsLastWindow = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "data", Name: "facts_last_window_timestamp_seconds",
		Help: "Unix time of the newest collected window. Read from the database, so it reports rows that exist rather than a job's claim.",
	})

	m.RollupLastDay = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "data", Name: "rollup_last_write_timestamp_seconds",
		Help: "Unix time of the rollup job's last write. Replaces a Pushgateway: a batch job cannot be scraped after it exits.",
	})

	m.ClusterCost = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "cost", Name: "cluster_hourly",
		Help: "Approximate cluster cost over the trailing hour. FLOAT64 and for alerting only -- Postgres is the system of record. Gate alerts on data freshness: a stale cluster also reads zero.",
	})

	m.ClusterWaste = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "cost", Name: "cluster_wasted_hours",
		Help: "Approximate wasted resource over the trailing hour, by resource.",
	},
		// `resource` is cpu or memory: two values, and the only label here. Not namespace, not workload --
		// see the note on ClusterCost.
		[]string{"resource"},
	)

	m.reg.MustRegister(
		m.HTTPRequests, m.HTTPDuration, m.HTTPInFlight,
		m.FactsLastWindow, m.RollupLastDay,
		m.ClusterCost, m.ClusterWaste,
	)
	return m
}

// NewCollector builds the collector's registry.
//
// Batch-loop metrics only. No HTTP histogram: the collector serves three operational endpoints, and
// instrumenting those would report a request rate that is entirely Prometheus scraping us.
func NewCollector() *Collector {
	m := &Collector{base: newBase()}

	m.CollectorCycles = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "collector", Name: "cycles_total",
		Help: "Collection cycles by outcome.",
	},
		// Three values: success, partial, failed. `partial` exists because internal/costing isolates a
		// failing namespace and carries on, so an outcome that is neither clean nor a failure is a real
		// state and collapsing it into either would lie.
		[]string{"outcome"},
	)

	m.CollectorLastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "collector", Name: "last_success_timestamp_seconds",
		Help: "Unix time of the last fully successful cycle. Alert on `time() - this`, because a dead collector emits nothing to alert on.",
	})

	m.CollectorCycleDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "collector", Name: "cycle_duration_seconds",
		Help:    "Time to complete a cycle. Approaching the collection interval means cycles are about to overlap.",
		Buckets: collectorBuckets,
	})

	m.CollectorAllocations = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "collector", Name: "allocations_written_total",
		Help: "Container-allocation fact rows written.",
	})

	m.CollectorNamespaceErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "collector", Name: "namespace_errors_total",
		Help: "Namespaces that failed within a cycle. A cycle can succeed while covering less of the cluster.",
	},
		// `reason`, NOT `namespace`. A namespace name is unbounded over time in any cluster with
		// per-pull-request preview environments, and the useful question is "what KIND of thing is
		// failing" -- the affected namespace is already in a WARN log line with the full error.
		[]string{"reason"},
	)

	m.FallbackPricedNodes = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "pricing", Name: "fallback_priced_nodes",
		Help: "Nodes priced from the fallback guess rather than the catalogue. Non-zero means part of the bill is an estimate.",
	})

	m.reg.MustRegister(
		m.CollectorCycles, m.CollectorLastSuccess, m.CollectorCycleDuration,
		m.CollectorAllocations, m.CollectorNamespaceErrors, m.FallbackPricedNodes,
	)
	return m
}

// ObserveCycle records the outcome of one collection cycle.
//
// A single method rather than the caller touching four metrics, because these four must move together:
// a cycle that incremented the counter but forgot the timestamp would look alive to the dashboard and
// dead to the alert. Keeping them in one place makes that impossible rather than unlikely.
func (m *Collector) ObserveCycle(outcome string, took time.Duration, rowsWritten int, namespaceErrors map[string]int) {
	m.CollectorCycles.WithLabelValues(outcome).Inc()
	m.CollectorCycleDuration.Observe(took.Seconds())
	if rowsWritten > 0 {
		m.CollectorAllocations.Add(float64(rowsWritten))
	}
	for reason, n := range namespaceErrors {
		m.CollectorNamespaceErrors.WithLabelValues(reason).Add(float64(n))
	}
	// ONLY a clean cycle advances the timestamp.
	//
	// This is the point of the whole metric. A partial cycle wrote SOME rows, so a naive
	// implementation would move the clock forward and the freshness alert would never fire -- while
	// half the cluster went unmeasured indefinitely. "Last success" has to mean success.
	if outcome == OutcomeSuccess {
		m.CollectorLastSuccess.Set(float64(time.Now().Unix()))
	}
}

// The cycle outcomes. Constants rather than strings at call sites, because a typo would create a
// silent fourth series that no dashboard or alert references.
const (
	OutcomeSuccess = "success"
	// OutcomePartial means the cycle finished but some namespaces failed. A real third state: the data
	// is usable and incomplete, and calling it either "success" or "failed" would be a lie.
	OutcomePartial = "partial"
	OutcomeFailed  = "failed"
)

// slogErrorLog adapts slog to promhttp's error logger.
//
// promhttp predates slog and wants an interface with Println(...any). Adapting keeps scrape errors in the
// same structured stream as everything else -- carrying the service and version attributes, and
// searchable alongside request logs -- rather than arriving as unstructured lines from a second logger.
type slogErrorLog struct{ log *slog.Logger }

// Println satisfies promhttp.Logger. Exported only because that interface requires it -- the type itself
// is unexported, so this is not part of the package's API.
func (l slogErrorLog) Println(v ...any) {
	l.log.Error("metrics scrape error", slog.Any("detail", v))
}
