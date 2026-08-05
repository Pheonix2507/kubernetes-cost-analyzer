package prom

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeAPI implements the subset of promv1.API we use, so the query-result handling can be
// tested without a Prometheus.
//
// The interface is large, so only Query is implemented and everything else panics. That is
// deliberate: if a future change starts calling QueryRange, the panic names it immediately
// rather than the test silently exercising a path it does not cover.
type fakeAPI struct {
	promv1.API
	result   model.Value
	warnings promv1.Warnings
	err      error
	// gotQuery and gotTime record what was asked, so the query text and evaluation instant
	// can be asserted.
	gotQuery string
	gotTime  time.Time
}

func (f *fakeAPI) Query(_ context.Context, query string, ts time.Time, _ ...promv1.Option) (model.Value, promv1.Warnings, error) {
	f.gotQuery = query
	f.gotTime = ts
	return f.result, f.warnings, f.err
}

func newTestClient(api promv1.API) *Client {
	return &Client{api: api, timeout: 5 * time.Second, log: testLogger()}
}

func sample(ns, pod, container string, v float64) *model.Sample {
	return &model.Sample{
		Metric: model.Metric{
			"namespace": model.LabelValue(ns),
			"pod":       model.LabelValue(pod),
			"container": model.LabelValue(container),
		},
		Value: model.SampleValue(v),
	}
}

// =============================================================================
// Query construction -- every filter in these strings is load-bearing
// =============================================================================

// TestCPUQuery_HasTheFiltersThatMatter is the highest-value test in this package.
//
// A missing container!="" filter produces numbers that are roughly TWICE too high and look
// entirely plausible: cAdvisor emits a pod-level aggregate series in addition to one series per
// container. Measured on the development cluster, the same namespace reported 24.0Mi without the
// filter and 11.5Mi with it.
//
// Nothing about that failure looks like a bug. It just quietly doubles the bill.
func TestCPUQuery_HasTheFiltersThatMatter(t *testing.T) {
	t.Parallel()

	q := cpuQuery("team-payments", 5*time.Minute)

	required := []struct {
		fragment string
		why      string
	}{
		{
			fragment: `container!=""`,
			why: "cAdvisor emits a POD-LEVEL aggregate series with an empty container label in " +
				"ADDITION to one per container. Without this every pod is counted twice",
		},
		{
			fragment: `container!="POD"`,
			why: "older cAdvisor labelled the pause container POD; harmless to keep and it " +
				"protects against an older kubelet",
		},
		{
			fragment: "rate(",
			why: "the metric is a COUNTER of cumulative CPU-seconds. Read directly it reports a " +
				"container's whole lifetime as this window's usage, and rate() also handles the " +
				"counter reset a container restart causes",
		},
		{
			fragment: "sum by (namespace, pod, container)",
			why: "some cAdvisor versions emit one series PER CPU CORE, so the series must be " +
				"summed to get the container's total",
		},
		{
			fragment: `namespace="team-payments"`,
			why:      "the namespace shard; without it every worker queries the whole cluster",
		},
		{
			fragment: "[300s]",
			why: "the range must match the window being priced, or the sample summarises the " +
				"wrong period",
		},
	}

	for _, r := range required {
		if !strings.Contains(q, r.fragment) {
			t.Errorf("cpu query is missing %q\nquery: %s\nwhy this matters: %s", r.fragment, q, r.why)
		}
	}
}

// TestMemoryQuery_UsesAvgOverTimeNotRate covers the counter/gauge distinction. Taking a rate of
// a gauge yields bytes per second, which is not a thing.
func TestMemoryQuery_UsesAvgOverTimeNotRate(t *testing.T) {
	t.Parallel()

	q := memoryQuery("", 5*time.Minute)

	if !strings.Contains(q, "avg_over_time(") {
		t.Errorf("memory query does not use avg_over_time: %s", q)
	}
	if strings.Contains(q, "rate(") {
		t.Errorf("memory query uses rate() on a GAUGE, which yields bytes per second: %s", q)
	}
	// WORKING SET, not usage_bytes. usage_bytes includes the page cache, which the kernel
	// reclaims under pressure -- so a container that read a large file once looks permanently
	// enormous, and every right-sizing recommendation derived from it is wrong.
	if !strings.Contains(q, "container_memory_working_set_bytes") {
		t.Errorf("memory query does not use working set: %s", q)
	}
	if strings.Contains(q, "container_memory_usage_bytes") {
		t.Errorf("memory query uses usage_bytes, which includes reclaimable page cache: %s", q)
	}
}

// TestNamespaceSelector covers the shard filter, including the injection guard.
//
// PromQL has NO bind parameters, so the namespace must be interpolated -- there is no safe
// alternative. It is safe in practice because the value comes from the informer cache, meaning
// the API server already validated it as a DNS-1123 label. The explicit check makes that
// reasoning enforced rather than assumed, so a future caller passing a user-supplied string
// cannot silently break it.
func TestNamespaceSelector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		namespace string
		want      string
		why       string
	}{
		{"valid", "team-payments", `,namespace="team-payments"`, "the ordinary shard case"},
		{"empty means cluster-wide", "", "", "no filter at all, so the query spans every namespace"},
		{
			name: "injection attempt is dropped", namespace: `x"} or up{`, want: "",
			why: "dropping the selector is safer than injecting a malformed one: a cluster-wide " +
				"query returns MORE than asked for and the caller filters it, whereas a broken " +
				"selector could match unintended series or fail outright",
		},
		{"uppercase is not a valid label", "Team-Payments", "", "cannot match a real namespace"},
		{"space is not valid", "team payments", "", "cannot match a real namespace"},
		{"leading hyphen is not valid", "-leading", "", "not a DNS-1123 label"},
		{"trailing hyphen is not valid", "trailing-", "", "not a DNS-1123 label"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := namespaceSelector(tt.namespace); got != tt.want {
				t.Errorf("namespaceSelector(%q) = %q, want %q\nwhy: %s",
					tt.namespace, got, tt.want, tt.why)
			}
		})
	}
}

// TestPromDuration covers a mistake that surfaces as a runtime query-parse error rather than a
// compile error.
//
// PromQL accepts "300s" but NOT Go's "5m0s" or "1h0m0s", which is exactly what
// time.Duration.String() produces.
func TestPromDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Minute, "300s"},
		{30 * time.Second, "30s"},
		{time.Hour, "3600s"},
		{90 * time.Second, "90s"},
		// Sub-second windows clamp to 1s: PromQL rejects "0s" as a range.
		{0, "1s"},
		{500 * time.Millisecond, "1s"},
	}

	for _, tt := range tests {
		if got := promDuration(tt.d); got != tt.want {
			t.Errorf("promDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
		// Guard against Go's own formatting leaking in.
		if got := promDuration(tt.d); strings.ContainsAny(got, "hm.") {
			t.Errorf("promDuration(%v) = %q, which PromQL cannot parse", tt.d, got)
		}
	}
}

// =============================================================================
// Result handling -- where a bad value becomes a negative bill
// =============================================================================

// TestContainerUsage_RejectsNaNAndInf is a correctness test with real financial consequences.
//
// Prometheus returns NaN for a rate computed over insufficient data -- a container that started
// mid-window has fewer than two samples. int64(NaN) is IMPLEMENTATION-DEFINED in Go and in
// practice yields a huge NEGATIVE number, which would be stored as negative usage, become
// negative billable, and REDUCE someone's bill by an enormous amount.
//
// Skipping the series instead means the container is treated as having no observed usage, and
// max(request, usage) still bills it correctly on its request.
func TestContainerUsage_RejectsNaNAndInf(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{result: model.Vector{
		sample("ns", "good", "app", 0.5),
		sample("ns", "nan", "app", math.NaN()),
		sample("ns", "posinf", "app", math.Inf(1)),
		sample("ns", "neginf", "app", math.Inf(-1)),
		sample("ns", "negative", "app", -1.0),
	}}

	start := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	usage, err := newTestClient(api).ContainerUsage(context.Background(), "ns", start, start.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("ContainerUsage: %v", err)
	}

	// Only the valid sample survives.
	if got := usage[domain.ContainerKey{Namespace: "ns", Pod: "good", Container: "app"}]; got.CPUMillicores != 500 {
		t.Errorf("valid sample = %dm, want 500m", got.CPUMillicores)
	}

	for _, bad := range []string{"nan", "posinf", "neginf", "negative"} {
		key := domain.ContainerKey{Namespace: "ns", Pod: bad, Container: "app"}
		if got, found := usage[key]; found {
			t.Errorf("%s sample was kept as %dm; int64(NaN) is implementation-defined and in "+
				"practice hugely negative, which would REDUCE a bill", bad, got.CPUMillicores)
		}
	}
}

// TestContainerUsage_SkipsSeriesMissingLabels covers a series that cannot be attributed.
// Attributing it to the empty pod would create a phantom row with real cost.
func TestContainerUsage_SkipsSeriesMissingLabels(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{result: model.Vector{
		sample("ns", "pod", "app", 1.0),
		sample("", "pod", "app", 1.0), // no namespace
		sample("ns", "", "app", 1.0),  // no pod
		sample("ns", "pod", "", 1.0),  // no container (the pod-level aggregate)
	}}

	start := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	usage, err := newTestClient(api).ContainerUsage(context.Background(), "ns", start, start.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("ContainerUsage: %v", err)
	}
	if len(usage) != 1 {
		t.Errorf("got %d entries, want 1: unattributable series must be dropped, not "+
			"attributed to an empty key", len(usage))
	}
}

// TestContainerUsage_RoundsRatherThanTruncates covers the idle-service fixture.
//
// Truncating would report every container using under 0.5 millicores as exactly zero, making a
// genuinely idle container indistinguishable from one Prometheus has no data for.
func TestContainerUsage_RoundsRatherThanTruncates(t *testing.T) {
	t.Parallel()

	// 0.0006 cores = 0.6 millicores. Truncation gives 0; rounding gives 1.
	api := &fakeAPI{result: model.Vector{sample("ns", "p", "c", 0.0006)}}

	start := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	usage, err := newTestClient(api).ContainerUsage(context.Background(), "ns", start, start.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("ContainerUsage: %v", err)
	}
	got := usage[domain.ContainerKey{Namespace: "ns", Pod: "p", Container: "c"}]
	if got.CPUMillicores != 1 {
		t.Errorf("0.0006 cores = %dm, want 1m (truncation would lose sub-millicore usage)",
			got.CPUMillicores)
	}
}

// TestContainerUsage_EvaluatesAtWindowEnd pins the join between the range selector and the
// evaluation instant: together they must cover exactly [start, end).
func TestContainerUsage_EvaluatesAtWindowEnd(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{result: model.Vector{}}
	start := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)

	if _, err := newTestClient(api).ContainerUsage(context.Background(), "ns", start, end); err != nil {
		t.Fatalf("ContainerUsage: %v", err)
	}
	if !api.gotTime.Equal(end) {
		t.Errorf("query evaluated at %s, want the window END %s", api.gotTime, end)
	}
}

func TestContainerUsage_RejectsInvalidWindow(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{result: model.Vector{}}
	c := newTestClient(api)
	start := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)

	for _, end := range []time.Time{start, start.Add(-time.Minute)} {
		if _, err := c.ContainerUsage(context.Background(), "ns", start, end); err == nil {
			t.Errorf("accepted a window ending at %s, which is not after %s", end, start)
		}
	}
}

// TestContainerUsage_RejectsNonVectorResult covers a malformed query returning a scalar. A blind
// type assertion would panic; the ok form turns it into a diagnosable error.
func TestContainerUsage_RejectsNonVectorResult(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{result: &model.Scalar{Value: 42}}
	start := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)

	_, err := newTestClient(api).ContainerUsage(context.Background(), "ns", start, start.Add(5*time.Minute))
	if err == nil {
		t.Fatal("accepted a scalar result; want an error naming the type")
	}
	if !strings.Contains(err.Error(), "vector") {
		t.Errorf("error %q does not explain that a vector was expected", err)
	}
}

// TestContainerUsage_WarningsAreNotFailures covers Prometheus' warning channel. Warnings are
// emitted for partial results and deprecated syntax, and treating them as errors would drop a
// whole namespace's data over a deprecation notice.
func TestContainerUsage_WarningsAreNotFailures(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{
		result:   model.Vector{sample("ns", "p", "c", 0.5)},
		warnings: promv1.Warnings{"this query used a deprecated function"},
	}

	start := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	usage, err := newTestClient(api).ContainerUsage(context.Background(), "ns", start, start.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("a warning was treated as an error: %v", err)
	}
	if len(usage) != 1 {
		t.Errorf("got %d entries, want 1: the data must survive a warning", len(usage))
	}
}

// TestDescribe returns the queries an operator can paste into Prometheus' expression browser.
// "Which query produced this figure" should never require reading the source.
func TestDescribe(t *testing.T) {
	t.Parallel()

	queries := Describe("team-search", 5*time.Minute)
	if len(queries) != 2 {
		t.Fatalf("got %d queries, want 2 (cpu and memory)", len(queries))
	}
	for _, q := range queries {
		if !strings.Contains(q, `namespace="team-search"`) || !strings.Contains(q, "[300s]") {
			t.Errorf("Describe returned a query that does not match what the client issues: %s", q)
		}
	}
}
