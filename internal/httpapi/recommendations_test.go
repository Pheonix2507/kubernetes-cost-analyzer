package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/recommend"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// THE SPLIT BETWEEN THESE TESTS AND internal/recommend's
// -----------------------------------------------------
// internal/recommend/recommend_test.go grades the RULES: which container gets which verdict, at what
// severity, with what proposal. Repeating any of that here would double the maintenance and test the
// same code twice.
//
// This file tests the HTTP layer's own decisions, which are genuinely separate:
//   - the 7-day default range, which differs from every other endpoint on purpose
//   - the totals, which the handler computes and the engine knows nothing about
//   - whether savings and required increases stay separate rather than netting out
//   - cache headers, validation, error masking, and an empty result being 200 rather than 404
//
// The rule of thumb: if changing a threshold in DefaultThresholds would break a test here, that test
// is in the wrong file.

const testMiB = 1024 * 1024

// statsFixture is a container stats row that clears every evidence gate, so a test that sees no
// recommendation has failed on the thing it is testing and not on insufficient data.
func statsFixture(mutate func(*postgres.ContainerStats)) postgres.ContainerStats {
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	s := postgres.ContainerStats{
		Namespace:              "team-payments",
		WorkloadKind:           "Deployment",
		WorkloadName:           "api",
		Container:              "api",
		Replicas:               2,
		WindowCount:            2016,
		ObservedFrom:           from,
		ObservedTo:             from.Add(7 * 24 * time.Hour),
		PeakCoverage:           1.0,
		QoSClass:               "Burstable",
		CPURequestedMillicores: 500,
		MemRequestedBytes:      512 * testMiB,
		CPUAvgMillicores:       5,
		CPUP95Millicores:       8,
		CPUMaxMillicores:       10,
		MemAvgBytes:            20 * testMiB,
		MemP95Bytes:            24 * testMiB,
		MemMaxBytes:            26 * testMiB,
		CPUCostPerCoreHour:     dec("0.0371"),
		MemCostPerGiBHour:      dec("0.003975"),
		TotalCost:              dec("1.00"),
	}
	if mutate != nil {
		mutate(&s)
	}
	return s
}

// getRecommendations is the request every test here makes, so each one varies only its query string.
func getRecommendations(t *testing.T, reports *stubReports, query string) (*httptest.ResponseRecorder, recommendationsResponse) {
	t.Helper()

	rec := httptest.NewRecorder()
	routerWithReports(reports).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/recommendations"+query, nil))

	var body recommendationsResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("200 response is not a recommendationsResponse: %v\nbody: %s", err, rec.Body.String())
		}
	}
	return rec, body
}

// =============================================================================
// The seven-day default
// =============================================================================

// TestRecommendations_DefaultsToSevenDays pins the one parameter decision unique to this endpoint.
//
// Every other endpoint defaults to 24 hours. This one defaults to a week, and the difference is not
// a tuning preference: a cost figure for yesterday is a complete answer on its own, but a
// RECOMMENDATION from one day of data cannot see a weekly pattern. A batch job that runs on Sundays
// looks abandoned on a Tuesday, and the tool would confidently advise deleting it.
//
// A caller cannot be expected to know that, so the default has to.
func TestRecommendations_DefaultsToSevenDays(t *testing.T) {
	t.Parallel()

	reports := &stubReports{}
	rec, _ := getRecommendations(t, reports, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	got := reports.gotStatsParams.To.Sub(reports.gotStatsParams.From)
	if got < 7*24*time.Hour-time.Hour || got > 7*24*time.Hour+time.Hour {
		t.Errorf("default range is %v, want about 168h (7 days).\n"+
			"A recommendation from 24h of data cannot see a weekly pattern, so a weekly batch job "+
			"would look idle and be recommended for deletion", got)
	}
}

// TestRecommendations_ExplicitRangeIsHonoured confirms the wider default is a DEFAULT and not a
// floor. Someone investigating a spike from this morning is entitled to ask for this morning, and
// the evidence gate inside the engine is what protects them from acting on too little data.
func TestRecommendations_ExplicitRangeIsHonoured(t *testing.T) {
	t.Parallel()

	reports := &stubReports{}
	rec, body := getRecommendations(t, reports,
		"?from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	if d := reports.gotStatsParams.To.Sub(reports.gotStatsParams.From); d != 24*time.Hour {
		t.Errorf("range = %v, want exactly the requested 24h", d)
	}
	// Echoed back, so a report can be labelled with the window it actually covers. Advice without
	// its observation window is unauditable.
	if body.From.Format(time.RFC3339) != "2026-08-01T00:00:00Z" {
		t.Errorf("From = %s, want the requested value echoed back", body.From)
	}
}

// =============================================================================
// Totals: the handler's own arithmetic
// =============================================================================

// TestRecommendations_SavingsAndIncreasesNeverNet is the most important test in this file.
//
// A single "net saving" figure would let a large right-sizing win cancel out a memory increase that
// someone MUST make to stop a container being OOMKilled. The page would read "net saving $28" and
// the reliability fix inside it would be invisible.
//
// That is the difference between a tool that reports the system and a tool that optimises its own
// headline number. They are separate fields, and this test is what keeps them separate.
func TestRecommendations_SavingsAndIncreasesNeverNet(t *testing.T) {
	t.Parallel()

	reports := &stubReports{containerStats: []postgres.ContainerStats{
		// Over-provisioned: a large positive saving.
		statsFixture(nil),
		// Under-requested on memory: uses 3x its request, so the fix COSTS money.
		statsFixture(func(s *postgres.ContainerStats) {
			s.WorkloadName, s.Container = "memory-hoarder", "hoarder"
			s.CPURequestedMillicores, s.MemRequestedBytes = 50, 64*testMiB
			s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 0, 0, 0
			s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 201*testMiB, 200*testMiB, 201*testMiB
		}),
	}}

	rec, body := getRecommendations(t, reports, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	saving := dec(body.Totals.PotentialMonthlySaving)
	increase := dec(body.Totals.RequiredMonthlyIncrease)

	if !saving.IsPositive() {
		t.Errorf("potential_monthly_saving = %s, want positive: the over-provisioned container "+
			"reserves 500m and peaks at 10m", saving)
	}
	if !increase.IsPositive() {
		t.Errorf("required_monthly_increase = %s, want positive: memory-hoarder uses 3x its "+
			"request and raising it must cost money", increase)
	}

	// Both reported POSITIVE. An increase rendered as "-0.5044" invites a reader to add the two
	// columns and get the netted figure the split exists to prevent.
	if increase.IsNegative() {
		t.Errorf("required_monthly_increase = %s, want it expressed as a positive magnitude", increase)
	}

	// And the sum of the item-level figures must reconcile with the two totals, or the headline
	// disagrees with the rows beneath it -- the same failure mode the cost summary avoids by
	// totalling in Go rather than with a second query.
	wantSaving, wantIncrease := decimal.Zero, decimal.Zero
	for _, item := range body.Items {
		if item.EstimatedMonthlySaving.IsNegative() {
			wantIncrease = wantIncrease.Add(item.EstimatedMonthlySaving.Neg())
		} else {
			wantSaving = wantSaving.Add(item.EstimatedMonthlySaving)
		}
	}
	if !saving.Equal(wantSaving) {
		t.Errorf("potential_monthly_saving = %s, but the items sum to %s", saving, wantSaving)
	}
	if !increase.Equal(wantIncrease) {
		t.Errorf("required_monthly_increase = %s, but the items sum to %s", increase, wantIncrease)
	}
}

// TestRecommendations_SeverityCountsMatchTheItems checks the summary a dashboard renders as three
// badges. A count that disagreed with the list would send someone hunting for a critical finding
// that is not there.
func TestRecommendations_SeverityCountsMatchTheItems(t *testing.T) {
	t.Parallel()

	reports := &stubReports{containerStats: []postgres.ContainerStats{
		statsFixture(nil),
		statsFixture(func(s *postgres.ContainerStats) {
			s.WorkloadName, s.Container = "memory-hoarder", "hoarder"
			s.MemRequestedBytes = 64 * testMiB
			s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 201*testMiB, 200*testMiB, 201*testMiB
		}),
		statsFixture(func(s *postgres.ContainerStats) {
			s.WorkloadName, s.Container = "no-requests-at-all", "freeloader"
			s.CPURequestedMillicores, s.MemRequestedBytes = 0, 0
			s.QoSClass = "BestEffort"
		}),
	}}

	_, body := getRecommendations(t, reports, "")

	counted := map[recommend.Severity]int{}
	for _, item := range body.Items {
		counted[item.Severity]++
	}
	if body.Totals.Critical != counted[recommend.SeverityCritical] {
		t.Errorf("totals.critical = %d, items contain %d", body.Totals.Critical, counted[recommend.SeverityCritical])
	}
	if body.Totals.Warning != counted[recommend.SeverityWarning] {
		t.Errorf("totals.warning = %d, items contain %d", body.Totals.Warning, counted[recommend.SeverityWarning])
	}
	if body.Totals.Info != counted[recommend.SeverityInfo] {
		t.Errorf("totals.info = %d, items contain %d", body.Totals.Info, counted[recommend.SeverityInfo])
	}
	if body.Count != len(body.Items) {
		t.Errorf("count = %d, items = %d", body.Count, len(body.Items))
	}
}

// TestRecommendations_AnalysedContainersIsTheDenominator covers a field that is easy to dismiss as
// decoration.
//
// "3 recommendations" means something completely different across 4 containers than across 4,000.
// Without the denominator a reader cannot tell a healthy cluster from one the collector has barely
// scraped, and the second case looks like good news.
//
// Note it counts containers EXAMINED, not containers flagged, so it stays honest when nothing fires.
func TestRecommendations_AnalysedContainersIsTheDenominator(t *testing.T) {
	t.Parallel()

	// Three containers, one of them healthy: the denominator must still be 3.
	reports := &stubReports{containerStats: []postgres.ContainerStats{
		statsFixture(nil),
		statsFixture(func(s *postgres.ContainerStats) {
			s.WorkloadName, s.Container = "another", "api"
		}),
		statsFixture(func(s *postgres.ContainerStats) {
			// The control: p95 is 96% of its request, so nothing should fire for it.
			s.WorkloadName, s.Container = "right-sized-worker", "worker"
			s.CPURequestedMillicores, s.MemRequestedBytes = 50, 32*testMiB
			s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 42, 38, 51
			s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 20*testMiB, 20*testMiB, 21*testMiB
		}),
	}}

	_, body := getRecommendations(t, reports, "")
	if body.AnalysedContainers != 3 {
		t.Errorf("analysed_containers = %d, want 3: it counts containers EXAMINED, so it must "+
			"include the healthy one that produced no finding", body.AnalysedContainers)
	}
}

// TestRecommendations_EstimatedRatesPropagates checks the honesty flag survives the trip.
//
// When a node's instance type is missing from the pricing catalogue we fall back to a synthetic
// rate, so a saving computed from it is an estimate built on an estimate. Presenting that with the
// same confidence as a catalogue-priced figure would be the tool lying about what it knows.
func TestRecommendations_EstimatedRatesPropagates(t *testing.T) {
	t.Parallel()

	t.Run("false when every rate is real", func(t *testing.T) {
		t.Parallel()
		_, body := getRecommendations(t, &stubReports{
			containerStats: []postgres.ContainerStats{statsFixture(nil)},
		}, "")
		if body.Totals.EstimatedRates {
			t.Error("estimated_rates = true with no fallback rate anywhere")
		}
	})

	t.Run("true when any single rate is a fallback", func(t *testing.T) {
		t.Parallel()
		// ANY, not all: one estimated row is enough to make the TOTAL an estimate, and rounding that
		// away would be the most tempting possible place to hide it.
		_, body := getRecommendations(t, &stubReports{containerStats: []postgres.ContainerStats{
			statsFixture(nil),
			statsFixture(func(s *postgres.ContainerStats) {
				s.WorkloadName, s.Container = "on-an-unpriced-node", "api"
				s.EstimatedRates = true
			}),
		}}, "")
		if !body.Totals.EstimatedRates {
			t.Error("estimated_rates = false, but one recommendation rests on a fallback rate: " +
				"the total is an estimate the moment any part of it is")
		}
	})
}

// =============================================================================
// Ordering, empty results, errors
// =============================================================================

// TestRecommendations_MostUrgentFirst pins the serialised ORDER, not just the contents.
//
// JSON arrays are ordered and clients render them top-down. A caller that has to sort our output
// before displaying it will eventually forget to, and the finding that matters will be at the
// bottom of a long list. Sorting is the server's job because the server is the thing that knows
// what severity means.
func TestRecommendations_MostUrgentFirst(t *testing.T) {
	t.Parallel()

	reports := &stubReports{containerStats: []postgres.ContainerStats{
		// Deliberately supplied least-urgent first, so passing cannot be an accident of input order.
		statsFixture(nil),
		statsFixture(func(s *postgres.ContainerStats) {
			s.WorkloadName, s.Container = "no-requests-at-all", "freeloader"
			s.CPURequestedMillicores, s.MemRequestedBytes = 0, 0
			s.QoSClass = "BestEffort"
		}),
		statsFixture(func(s *postgres.ContainerStats) {
			s.WorkloadName, s.Container = "memory-hoarder", "hoarder"
			s.MemRequestedBytes = 64 * testMiB
			s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 201*testMiB, 200*testMiB, 201*testMiB
		}),
	}}

	_, body := getRecommendations(t, reports, "")
	if len(body.Items) < 2 {
		t.Fatalf("got %d recommendations, want at least 2 to test ordering", len(body.Items))
	}

	rank := map[recommend.Severity]int{
		recommend.SeverityCritical: 0,
		recommend.SeverityWarning:  1,
		recommend.SeverityInfo:     2,
	}
	for i := 1; i < len(body.Items); i++ {
		prev, cur := body.Items[i-1], body.Items[i]
		if rank[prev.Severity] > rank[cur.Severity] {
			t.Errorf("item %d is %s but item %d is %s: severity must be non-increasing",
				i-1, prev.Severity, i, cur.Severity)
		}
		// Within a severity, larger savings first: the reader's next action is the biggest win.
		if prev.Severity == cur.Severity && prev.EstimatedMonthlySaving.LessThan(cur.EstimatedMonthlySaving) {
			t.Errorf("within %s, item %d saves %s but item %d saves more (%s)",
				cur.Severity, i-1, prev.EstimatedMonthlySaving, i, cur.EstimatedMonthlySaving)
		}
	}
}

// TestRecommendations_NoFindingsIs200WithAnEmptyList covers the good news case.
//
// A 404 would be wrong twice over: the resource exists, and "your cluster has no waste" is a
// successful answer to the question asked. `items` must also serialise as [] rather than null, or
// every client has to nil-check before iterating -- and one of them will not.
func TestRecommendations_NoFindingsIs200WithAnEmptyList(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	routerWithReports(&stubReports{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/recommendations", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: no findings is a successful answer, not a missing resource", rec.Code)
	}

	var raw struct {
		Items *[]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if raw.Items == nil {
		t.Error("items serialised as null; want [] so clients can iterate without a nil check")
	}

	var body recommendationsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Count != 0 || body.AnalysedContainers != 0 {
		t.Errorf("count = %d, analysed = %d, want 0 and 0", body.Count, body.AnalysedContainers)
	}
	// Zero must still be a well-formed decimal string, not "" -- a client parsing it should not have
	// to special-case the empty case.
	if body.Totals.PotentialMonthlySaving == "" || dec(body.Totals.PotentialMonthlySaving).IsPositive() {
		t.Errorf("potential_monthly_saving = %q, want a zero decimal string",
			body.Totals.PotentialMonthlySaving)
	}
}

// TestRecommendations_DatabaseErrorIsMasked checks the error path leaks nothing.
//
// A driver error can carry the query, the schema and sometimes the connection string. That belongs
// in our logs, keyed by request ID, and never in a response body.
func TestRecommendations_DatabaseErrorIsMasked(t *testing.T) {
	t.Parallel()

	reports := &stubReports{err: errors.New(`pq: relation "container_allocations_2026_08" does not exist`)}
	rec := httptest.NewRecorder()
	routerWithReports(reports).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/recommendations", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error response is not our envelope: %v", err)
	}
	if body.Error.Code != "internal_error" {
		t.Errorf("code = %q, want internal_error", body.Error.Code)
	}
	for _, leak := range []string{"container_allocations_2026_08", "relation", "pq:"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the driver error leaked %q into the response body: %s", leak, rec.Body.String())
		}
	}
	// The request ID is what makes the masked error diagnosable: it turns "it returned a 500" into
	// one log query.
	if body.Error.RequestID == "" {
		t.Error("no request_id in the error body, so a user cannot quote anything actionable")
	}
}

// TestRecommendations_ValidationIsSharedWithTheCostEndpoints confirms this endpoint did not grow its
// own parameter handling. from/to/limit/filters behave identically everywhere, because a caller who
// has learned one endpoint should not have to relearn the next.
func TestRecommendations_ValidationIsSharedWithTheCostEndpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		query string
		field string
		why   string
	}{
		{"?from=yesterday", "from", "RFC3339 only: 01/02/2026 is ambiguous by a month"},
		{"?from=2026-08-02T00:00:00Z&to=2026-08-01T00:00:00Z", "to", "a reversed range returns nothing, which looks like no data"},
		{"?limit=0", "limit", "zero rows is never what anyone meant"},
		{"?limit=100000", "limit", "refused, not clamped: silently returning 1000 rows looks like the whole set"},
		{"?capacity_type=Spot", "capacity_type", "wrong case matches nothing and looks like an absence of spot capacity"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			routerWithReports(&stubReports{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				"/api/v1/recommendations"+tt.query, nil))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400\nwhy: %s", rec.Code, tt.why)
			}
			var body ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("invalid error envelope: %v", err)
			}
			named := false
			for _, f := range body.Error.Fields {
				if f.Field == tt.field {
					named = true
				}
			}
			if !named {
				t.Errorf("field %q is not named; got %+v", tt.field, body.Error.Fields)
			}
		})
	}
}

// TestRecommendations_FiltersReachTheQuery confirms the filters are not silently dropped.
//
// A team asking for its own recommendations and receiving the whole cluster's would act on another
// team's workloads, which is worse than an error.
func TestRecommendations_FiltersReachTheQuery(t *testing.T) {
	t.Parallel()

	reports := &stubReports{}
	getRecommendations(t, reports, "?namespace=team-search&team=search&environment=prod&limit=25")

	f := reports.gotStatsParams.Filters
	if f.Namespace != "team-search" || f.Team != "search" || f.Environment != "prod" {
		t.Errorf("filters = %+v, want namespace/team/environment all forwarded", f)
	}
	if reports.gotStatsParams.Limit != 25 {
		t.Errorf("limit = %d, want 25", reports.gotStatsParams.Limit)
	}
}

// TestRecommendations_IsCacheablePrivately checks the same caching decision the cost endpoints make.
//
// PRIVATE matters more here than anywhere else: a recommendation names a team's workloads and their
// waste. A shared proxy caching one team's list and serving it to another would be a data leak
// dressed as an optimisation.
func TestRecommendations_IsCacheablePrivately(t *testing.T) {
	t.Parallel()

	rec, _ := getRecommendations(t, &stubReports{}, "")

	if cc := rec.Header().Get("Cache-Control"); cc != "private, max-age=60" {
		t.Errorf("Cache-Control = %q, want private with a 60s max-age", cc)
	}
	if v := rec.Header().Get("Vary"); v != "Authorization" {
		t.Errorf("Vary = %q, want Authorization: two keys may be entitled to different data", v)
	}
}
