package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/recommend"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// stubTrends records what the handler asked for and returns fixed data.
type stubTrends struct {
	series []postgres.TrendSeries
	source postgres.TrendSource
	err    error

	reports    []postgres.MonthlyReport
	reportsErr error

	// calls records every Trend request in order, which is what the comparison tests assert on: the
	// period-over-period figure is produced by a SECOND call with a shifted range, and the only way to
	// check that range is to look at what was asked for.
	calls []postgres.TrendParams
	// prevSeries, when non-nil, is returned for the second call, so current and previous can differ.
	prevSeries []postgres.TrendSeries

	gotReportParams postgres.MonthlyReportParams
}

func (s *stubTrends) Trend(_ context.Context, p postgres.TrendParams) ([]postgres.TrendSeries, postgres.TrendSource, error) {
	s.calls = append(s.calls, p)
	if s.err != nil {
		return nil, "", s.err
	}
	if len(s.calls) > 1 && s.prevSeries != nil {
		return s.prevSeries, s.source, nil
	}
	return s.series, s.source, nil
}

func (s *stubTrends) MonthlyReports(_ context.Context, p postgres.MonthlyReportParams) ([]postgres.MonthlyReport, error) {
	s.gotReportParams = p
	return s.reports, s.reportsErr
}

func routerWithTrends(trends Trends) http.Handler {
	return NewRouter(RouterOptions{
		Log: discardLogger(), Readiness: health.NewAggregator(time.Second),
		Inventory: &stubInventory{}, Pricer: defaultStubPricer(),
		Reports: &stubReports{}, Stats: &stubReports{}, Trends: trends,
		Recommender: recommend.NewEngine(recommend.DefaultThresholds()),
	})
}

// series builds one series with the given daily costs, starting at `from`.
func seriesWith(group string, from time.Time, costs []string, windows int64) postgres.TrendSeries {
	s := postgres.TrendSeries{
		Group:     map[string]string{"namespace_name": group},
		Points:    []postgres.TrendPoint{},
		TotalCost: decimal.Zero,
	}
	for i, c := range costs {
		cost := decimal.RequireFromString(c)
		s.Points = append(s.Points, postgres.TrendPoint{
			BucketStart: from.AddDate(0, 0, i),
			TotalCost:   cost,
			Windows:     windows,
		})
		s.TotalCost = s.TotalCost.Add(cost)
	}
	return s
}

func getTrend(t *testing.T, trends *stubTrends, query string) (*httptest.ResponseRecorder, trendResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	routerWithTrends(trends).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/costs/trend"+query, nil))

	var body trendResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("200 response is not a trendResponse: %v\nbody: %s", err, rec.Body.String())
		}
	}
	return rec, body
}

// =============================================================================
// Defaults
// =============================================================================

// TestTrend_DefaultsToThirtyDaysOfDailyBuckets pins this endpoint's defaults, which differ from every
// other endpoint's on purpose.
//
// /costs/summary defaults to 24 hours, /recommendations to 7 days, and this to 30. Each answers the
// question its endpoint is for: a cost figure is about now, a recommendation needs a week to see a weekly
// pattern, and a trend with two points is not a trend.
//
// Day buckets are also the grain the rollup stores, so the default is simultaneously the most useful and
// the cheapest answer.
func TestTrend_DefaultsToThirtyDaysOfDailyBuckets(t *testing.T) {
	t.Parallel()

	trends := &stubTrends{source: postgres.TrendSourceRollup}
	rec, body := getTrend(t, trends, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	if len(trends.calls) != 1 {
		t.Fatalf("made %d Trend calls, want 1 without compare", len(trends.calls))
	}
	p := trends.calls[0]
	if got := p.To.Sub(p.From); got < 29*24*time.Hour || got > 31*24*time.Hour {
		t.Errorf("default range = %v, want about 30 days", got)
	}
	if p.Interval != postgres.IntervalDay {
		t.Errorf("interval = %q, want day: it matches the rollup's grain, so the default is also the "+
			"cheap path", p.Interval)
	}
	if p.GroupBy != postgres.GroupByNamespace {
		t.Errorf("group_by = %q, want namespace", p.GroupBy)
	}
	if body.Comparison != nil {
		t.Error("a comparison was computed without compare=previous_period being asked for")
	}
}

// TestTrend_SourceIsEchoedToTheCaller is the field that keeps the routing explainable.
//
// The rollup and the fact table do not answer identically: the rollup has no pod grain, no percentiles,
// and only covers days the rollup job has processed -- so a series from it can legitimately stop short of
// today. "The trend disagrees with the summary" must be answerable from the response rather than by
// reading our source.
func TestTrend_SourceIsEchoedToTheCaller(t *testing.T) {
	t.Parallel()

	for _, source := range []postgres.TrendSource{postgres.TrendSourceRollup, postgres.TrendSourceFacts} {
		t.Run(string(source), func(t *testing.T) {
			t.Parallel()
			_, body := getTrend(t, &stubTrends{source: source}, "")
			if body.Source != string(source) {
				t.Errorf("source = %q, want %q. A silent choice between two tables that answer "+
					"differently is how a system becomes unexplainable", body.Source, source)
			}
		})
	}
}

// TestTrend_EchoesTheRequestBack covers the fields a chart needs to label itself. A line with no stated
// range or interval is a line nobody can interpret.
func TestTrend_EchoesTheRequestBack(t *testing.T) {
	t.Parallel()

	_, body := getTrend(t, &stubTrends{source: postgres.TrendSourceRollup},
		"?from=2026-07-01T00:00:00Z&to=2026-08-01T00:00:00Z&interval=week&group_by=team")

	if body.From.Format(time.RFC3339) != "2026-07-01T00:00:00Z" {
		t.Errorf("From = %s, want the requested value", body.From)
	}
	if body.Interval != "week" || body.GroupBy != "team" {
		t.Errorf("interval=%q group_by=%q, want week and team", body.Interval, body.GroupBy)
	}
}

// =============================================================================
// Period-over-period
// =============================================================================

// TestTrend_ComparisonShiftsBackByTheSameSpan pins the comparison window.
//
// Equal length, immediately preceding -- not "last month". Calendar-aligned comparison sounds friendlier
// and is ambiguous: a 10-day range has no obvious previous month, and 31 days of August against 30 of
// September makes September look 3% cheaper for no reason at all.
func TestTrend_ComparisonShiftsBackByTheSameSpan(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC) // a 10-day span

	trends := &stubTrends{
		source:     postgres.TrendSourceRollup,
		series:     []postgres.TrendSeries{seriesWith("team-a", from, []string{"1", "2", "3"}, 288)},
		prevSeries: []postgres.TrendSeries{seriesWith("team-a", from.AddDate(0, 0, -10), []string{"1", "1", "2"}, 288)},
	}

	_, body := getTrend(t, trends,
		"?from=2026-08-01T00:00:00Z&to=2026-08-11T00:00:00Z&compare=previous_period")

	if len(trends.calls) != 2 {
		t.Fatalf("made %d Trend calls, want 2 (current and previous)", len(trends.calls))
	}
	prev := trends.calls[1]

	// The previous period ENDS where the current one begins, and is exactly as long.
	if !prev.To.Equal(from) {
		t.Errorf("previous To = %s, want %s (the current From)", prev.To, from)
	}
	if got := prev.To.Sub(prev.From); got != to.Sub(from) {
		t.Errorf("previous span = %v, want %v (identical to the current span)", got, to.Sub(from))
	}
	// Same interval, grouping and filters, or the two totals would not be comparable.
	if prev.Interval != trends.calls[0].Interval || prev.GroupBy != trends.calls[0].GroupBy {
		t.Errorf("the comparison changed the interval or grouping: %+v vs %+v", prev, trends.calls[0])
	}

	if body.Comparison == nil {
		t.Fatal("no comparison in the response")
	}
	c := body.Comparison
	// 6 now against 4 before: +2, a ratio of 0.5.
	if dec(c.Current).String() != "6" || dec(c.Previous).String() != "4" {
		t.Errorf("current=%s previous=%s, want 6 and 4", c.Current, c.Previous)
	}
	if dec(c.Change).String() != "2" {
		t.Errorf("change = %s, want 2", c.Change)
	}
	if c.ChangeRatio == nil || dec(*c.ChangeRatio).String() != "0.5" {
		t.Errorf("change_ratio = %v, want 0.5", c.ChangeRatio)
	}
}

// TestTrend_ComparisonOmitsTheRatioWhenThereIsNothingToDivideBy covers the case a client would otherwise
// render as "+Inf%" or, worse, "+100%".
//
// "Cost went from 0 to 5" is real and common -- a new workload -- and it has no percentage increase. The
// ratio is NULL and a note says why, rather than the API inventing a number the data does not support.
func TestTrend_ComparisonOmitsTheRatioWhenThereIsNothingToDivideBy(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	trends := &stubTrends{
		source:     postgres.TrendSourceRollup,
		series:     []postgres.TrendSeries{seriesWith("new-workload", from, []string{"5"}, 288)},
		prevSeries: []postgres.TrendSeries{}, // nothing existed before
	}

	_, body := getTrend(t, trends,
		"?from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z&compare=previous_period")

	c := body.Comparison
	if c == nil {
		t.Fatal("no comparison")
	}
	if c.ChangeRatio != nil {
		t.Errorf("change_ratio = %q, want null: dividing by a zero previous period is undefined, and "+
			"a client rendering the result would be stating something the data does not support",
			*c.ChangeRatio)
	}
	if c.Comparable {
		t.Error("comparable = true against an empty previous period")
	}
	if c.Note == "" {
		t.Error("no note explaining why the ratio is absent")
	}
	// The absolute change is still useful and still reported.
	if dec(c.Change).String() != "5" {
		t.Errorf("change = %s, want 5", c.Change)
	}
}

// TestTrend_ComparisonFlagsMaterialCoverageDifferences is the guard against the single most misleading
// thing a trend endpoint can do.
//
// A period during which collection started shows a huge apparent increase that is entirely an artefact of
// when the collector was deployed. This fired on the real cluster the first time it ran: cost was up 68%
// between two 5-day windows, and the earlier one had 3 days of data.
//
// Compared on WINDOW COUNTS rather than cost, because cost legitimately varies between periods and window
// count should not -- so a large gap in windows means missing data rather than a real change.
func TestTrend_ComparisonFlagsMaterialCoverageDifferences(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name           string
		curWindows     int64
		prevWindows    int64
		wantComparable bool
		why            string
	}{
		{"equal coverage", 288, 288, true, "the same number of samples, so the difference is real cost"},
		{"minor churn", 288, 270, true,
			"6% fewer windows is pods starting and stopping, which is normal and not worth a warning"},
		{"previous period barely collected", 288, 100, false,
			"THE REAL CASE: collection started mid-period, so most of the apparent increase is missing data"},
		{"current period barely collected", 100, 288, false,
			"the reverse -- an apparent saving that is really a collection outage, which is worse " +
				"because it looks like good news"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			trends := &stubTrends{
				source:     postgres.TrendSourceRollup,
				series:     []postgres.TrendSeries{seriesWith("a", from, []string{"10"}, tt.curWindows)},
				prevSeries: []postgres.TrendSeries{seriesWith("a", from.AddDate(0, 0, -1), []string{"5"}, tt.prevWindows)},
			}
			_, body := getTrend(t, trends,
				"?from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z&compare=previous_period")

			if body.Comparison == nil {
				t.Fatal("no comparison")
			}
			if body.Comparison.Comparable != tt.wantComparable {
				t.Errorf("comparable = %v, want %v\nwhy: %s",
					body.Comparison.Comparable, tt.wantComparable, tt.why)
			}
			if !tt.wantComparable && body.Comparison.Note == "" {
				t.Error("comparable is false with no note saying why")
			}
		})
	}
}

// TestTrend_ComparisonFailureDegradesRatherThanFailing pins the decision to keep the data we already have.
//
// The trend itself succeeded. Failing the whole request because an optional enrichment failed would deny
// the caller a result that is sitting in memory.
func TestTrend_ComparisonFailureDegradesRatherThanFailing(t *testing.T) {
	t.Parallel()

	// A stub that answers the first call and fails the second.
	trends := &failSecondCall{
		series: []postgres.TrendSeries{
			seriesWith("a", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), []string{"3"}, 288),
		},
	}

	rec := httptest.NewRecorder()
	routerWithTrends(trends).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/costs/trend?compare=previous_period", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the trend succeeded and only the comparison failed", rec.Code)
	}
	var body trendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body.Comparison != nil {
		t.Error("a comparison was returned despite the query failing")
	}
	if body.Count != 1 {
		t.Errorf("count = %d, want 1: the series must survive a failed comparison", body.Count)
	}
}

// failSecondCall answers once then errors, which is what a transient failure on the comparison query
// looks like.
type failSecondCall struct {
	series []postgres.TrendSeries
	calls  int
}

func (f *failSecondCall) Trend(_ context.Context, _ postgres.TrendParams) ([]postgres.TrendSeries, postgres.TrendSource, error) {
	f.calls++
	if f.calls > 1 {
		return nil, "", errors.New("connection reset")
	}
	return f.series, postgres.TrendSourceRollup, nil
}

func (f *failSecondCall) MonthlyReports(_ context.Context, _ postgres.MonthlyReportParams) ([]postgres.MonthlyReport, error) {
	return nil, nil
}

// =============================================================================
// Validation
// =============================================================================

// TestTrend_RejectsHourlyOverLongRanges covers the guard that stops the one query this whole phase exists
// to avoid.
//
// Hourly routes to the FACT table, so hourly over a year would scan the raw rows for 8,760 buckets. It is
// also unreadable: 336 points is already the practical ceiling for a line chart.
func TestTrend_RejectsHourlyOverLongRanges(t *testing.T) {
	t.Parallel()

	rec, _ := getTrend(t, &stubTrends{source: postgres.TrendSourceFacts},
		"?interval=hour&from=2026-01-01T00:00:00Z&to=2026-12-31T00:00:00Z")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: hourly over a year reads the fact table for 8,760 buckets", rec.Code)
	}

	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid error envelope: %v", err)
	}
	named := false
	for _, f := range body.Error.Fields {
		if f.Field == "interval" {
			named = true
			// The message must say what to do instead, or the caller has to guess.
			if !strings.Contains(f.Reason, "interval=day") {
				t.Errorf("reason %q does not suggest the alternative", f.Reason)
			}
		}
	}
	if !named {
		t.Errorf("interval is not named in the fields: %+v", body.Error.Fields)
	}

	// And 14 days IS allowed, so the bound is a bound rather than a ban.
	ok, _ := getTrend(t, &stubTrends{source: postgres.TrendSourceFacts},
		"?interval=hour&from=2026-08-01T00:00:00Z&to=2026-08-14T00:00:00Z")
	if ok.Code != http.StatusOK {
		t.Errorf("13 days of hourly buckets was refused with %d; the limit is 14", ok.Code)
	}
}

// TestTrend_ValidationNamesEveryBadParameter keeps the 400 actionable, as everywhere else.
func TestTrend_ValidationNamesEveryBadParameter(t *testing.T) {
	t.Parallel()

	rec, _ := getTrend(t, &stubTrends{}, "?interval=fortnight&group_by=nonsense&compare=maybe&limit=0")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid error envelope: %v", err)
	}
	named := map[string]string{}
	for _, f := range body.Error.Fields {
		named[f.Field] = f.Reason
	}
	for _, want := range []string{"interval", "group_by", "compare", "limit"} {
		if _, ok := named[want]; !ok {
			t.Errorf("field %q is not named; got %+v", want, body.Error.Fields)
		}
	}
	// The interval message must list the valid values, from the same list the repository enforces.
	if r := named["interval"]; !strings.Contains(r, "day") || !strings.Contains(r, "month") {
		t.Errorf("interval reason %q does not list the valid options", r)
	}
}

// TestTrend_LimitCapsSeries documents that the limit's unit is series, not points.
func TestTrend_LimitCapsSeries(t *testing.T) {
	t.Parallel()

	trends := &stubTrends{source: postgres.TrendSourceRollup}
	getTrend(t, trends, "?limit=7")
	if len(trends.calls) != 1 {
		t.Fatalf("made %d calls, want 1", len(trends.calls))
	}
	if trends.calls[0].Limit != 7 {
		t.Errorf("Limit = %d, want 7", trends.calls[0].Limit)
	}

	// The ceiling is lower than the summary's 1000, because each series carries a point per bucket:
	// 100 series x 400 daily buckets is already 40,000 points.
	rec, _ := getTrend(t, trends, "?limit=500")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a limit above the 100-series ceiling", rec.Code)
	}
}

// TestTrend_DatabaseErrorIsMasked keeps driver detail out of the response.
func TestTrend_DatabaseErrorIsMasked(t *testing.T) {
	t.Parallel()

	rec, _ := getTrend(t, &stubTrends{
		err: errors.New(`pq: relation "container_allocations_daily" does not exist`),
	}, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	for _, leak := range []string{"container_allocations_daily", "relation", "pq:"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the driver error leaked %q into the response: %s", leak, rec.Body.String())
		}
	}
}

// TestTrend_IsCacheablePrivately matches the cost endpoints: a trend names a team's workloads and their
// spend, so a shared proxy serving one team's chart to another would be a data leak.
func TestTrend_IsCacheablePrivately(t *testing.T) {
	t.Parallel()

	rec, _ := getTrend(t, &stubTrends{source: postgres.TrendSourceRollup}, "")
	if cc := rec.Header().Get("Cache-Control"); cc != "private, max-age=60" {
		t.Errorf("Cache-Control = %q, want private with a 60s max-age", cc)
	}
	if v := rec.Header().Get("Vary"); v != "Authorization" {
		t.Errorf("Vary = %q, want Authorization", v)
	}
}

// =============================================================================
// Monthly statements
// =============================================================================

func monthlyReport(month, scopeKind, scopeValue, cost, coverage string, finalised bool) postgres.MonthlyReport {
	r := postgres.MonthlyReport{
		ClusterName: "kca-dev", Month: month,
		ScopeKind: scopeKind, ScopeValue: scopeValue,
		TotalCost:   decimal.RequireFromString(cost),
		Coverage:    decimal.RequireFromString(coverage),
		DaysInMonth: 31,
	}
	if finalised {
		at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		r.FinalisedAt = &at
	}
	return r
}

func getReports(t *testing.T, trends *stubTrends, query string) (*httptest.ResponseRecorder, monthlyReportsResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	routerWithTrends(trends).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/reports/monthly"+query, nil))

	var body monthlyReportsResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("200 response is not a monthlyReportsResponse: %v\nbody: %s", err, rec.Body.String())
		}
	}
	return rec, body
}

// TestMonthlyReports_SurfacesTheWorstCoverage hoists the number that is easiest to overlook and most
// dangerous to miss.
//
// A month with 40% coverage produces a total that is confidently too low, and nothing about the total
// itself reveals that. A reader scanning a page of statements should not have to check every row to
// discover one of them is a tenth complete.
func TestMonthlyReports_SurfacesTheWorstCoverage(t *testing.T) {
	t.Parallel()

	trends := &stubTrends{reports: []postgres.MonthlyReport{
		monthlyReport("2026-08", "namespace", "team-a", "10", "1.0000", false),
		monthlyReport("2026-07", "namespace", "team-a", "3", "0.0968", true),
		monthlyReport("2026-06", "namespace", "team-a", "9", "0.9000", true),
	}}

	rec, body := getReports(t, trends, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body.LowestCoverage != "0.0968" {
		t.Errorf("lowest_coverage = %q, want 0.0968 -- the WORST of the three, not the newest or an "+
			"average", body.LowestCoverage)
	}
	// And the provisional count, which tells a reader whether the page can still change.
	if body.Provisional != 1 {
		t.Errorf("provisional_count = %d, want 1", body.Provisional)
	}
	if body.Count != 3 {
		t.Errorf("count = %d, want 3", body.Count)
	}
}

// TestMonthlyReports_EmptyPageClaimsNoCoverage covers a default that would otherwise read as a real
// measurement.
//
// An empty page reporting lowest_coverage 1.0000 would be claiming perfect coverage of nothing.
func TestMonthlyReports_EmptyPageClaimsNoCoverage(t *testing.T) {
	t.Parallel()

	rec, body := getReports(t, &stubTrends{}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: no statements is a valid answer", rec.Code)
	}
	if body.LowestCoverage != "0.0000" {
		t.Errorf("lowest_coverage = %q on an empty page, want 0.0000. 1.0000 would be claiming "+
			"perfect coverage of nothing", body.LowestCoverage)
	}

	// items must be [] rather than null, as everywhere else.
	var raw struct {
		Items *[]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if raw.Items == nil {
		t.Error("items serialised as null; want []")
	}
}

// TestMonthlyReports_DefaultsToTwelveMonths pins the default window: the range a year-on-year comparison
// needs, and small enough that the default response stays readable.
func TestMonthlyReports_DefaultsToTwelveMonths(t *testing.T) {
	t.Parallel()

	trends := &stubTrends{}
	getReports(t, trends, "")

	p := trends.gotReportParams
	span := p.To.Time().Sub(p.From.Time())
	// Eleven months back plus the current one is twelve months inclusive.
	if span < 330*24*time.Hour || span > 340*24*time.Hour {
		t.Errorf("default span = %v (%s to %s), want about 11 months so the inclusive range covers 12",
			span, p.From, p.To)
	}
}

// TestMonthlyReports_ParsesMonthsNotTimestamps pins the parameter format.
//
// A statement is for a calendar month, so the parameter is YYYY-MM and there is no way to ask for half of
// one -- which removes the question of what a partial month's statement would mean.
func TestMonthlyReports_ParsesMonthsNotTimestamps(t *testing.T) {
	t.Parallel()

	trends := &stubTrends{}
	rec, _ := getReports(t, trends, "?from=2026-06&to=2026-08")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := trends.gotReportParams.From.String(); got != "2026-06" {
		t.Errorf("From = %s, want 2026-06", got)
	}
	if got := trends.gotReportParams.To.String(); got != "2026-08" {
		t.Errorf("To = %s, want 2026-08", got)
	}

	// An RFC3339 timestamp is REFUSED rather than truncated to its month, because silently discarding
	// the day means answering a different question from the one asked.
	for _, bad := range []string{"2026-08-04T00:00:00Z", "2026-08-04", "august", "2026", "2026-13"} {
		rec, _ := getReports(t, &stubTrends{}, "?from="+bad)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("from=%q gave status %d, want 400", bad, rec.Code)
		}
	}
}

// TestMonthlyReports_RejectsAReversedRange catches the typo before it returns an empty page that looks
// like an absence of statements.
func TestMonthlyReports_RejectsAReversedRange(t *testing.T) {
	t.Parallel()

	rec, _ := getReports(t, &stubTrends{}, "?from=2026-08&to=2026-06")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a reversed month range", rec.Code)
	}
}

// TestMonthlyReports_ValidatesScopeKind checks the read path uses the same three values the CHECK
// constraint does.
func TestMonthlyReports_ValidatesScopeKind(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"", "cluster", "namespace", "team"} {
		rec, _ := getReports(t, &stubTrends{}, "?scope_kind="+ok)
		if rec.Code != http.StatusOK {
			t.Errorf("scope_kind=%q gave %d, want 200", ok, rec.Code)
		}
	}
	for _, bad := range []string{"Team", "workload", "pod"} {
		rec, _ := getReports(t, &stubTrends{}, "?scope_kind="+bad)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("scope_kind=%q gave %d, want 400: an unknown scope would return nothing and "+
				"look like 'no statements' rather than 'wrong parameter'", bad, rec.Code)
		}
	}
}

// TestMonthlyReports_FinalisedAtDistinguishesFrozenFromProvisional pins the serialisation of the field
// that makes a statement quotable.
//
// A pointer rather than a zero time, because "not yet closed" is a real state -- and a zero timestamp
// rendered as 0001-01-01 is a value someone will eventually put on a page.
func TestMonthlyReports_FinalisedAtDistinguishesFrozenFromProvisional(t *testing.T) {
	t.Parallel()

	trends := &stubTrends{reports: []postgres.MonthlyReport{
		monthlyReport("2026-08", "cluster", "kca-dev", "10", "0.5000", false),
		monthlyReport("2026-07", "cluster", "kca-dev", "20", "1.0000", true),
	}}

	rec, _ := getReports(t, trends, "")
	var raw struct {
		Items []struct {
			Month       string  `json:"month"`
			FinalisedAt *string `json:"finalised_at"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(raw.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(raw.Items))
	}
	if raw.Items[0].FinalisedAt != nil {
		t.Errorf("the provisional statement has finalised_at = %q, want null", *raw.Items[0].FinalisedAt)
	}
	if raw.Items[1].FinalisedAt == nil {
		t.Error("the frozen statement has finalised_at = null, so a client cannot tell it is immutable")
	}
	// The month is YYYY-MM, not a timestamp: a statement is for a month, and a date would invite a
	// reader to wonder what happened at 00:00 on the 1st.
	if raw.Items[0].Month != "2026-08" {
		t.Errorf("month = %q, want 2026-08", raw.Items[0].Month)
	}
}

// TestMonthlyReports_DatabaseErrorIsMasked keeps driver detail out of the response.
func TestMonthlyReports_DatabaseErrorIsMasked(t *testing.T) {
	t.Parallel()

	rec, _ := getReports(t, &stubTrends{
		reportsErr: errors.New(`pq: column "coverage" does not exist`),
	}, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "coverage\" does not exist") {
		t.Errorf("the driver error leaked into the response: %s", rec.Body.String())
	}
}
