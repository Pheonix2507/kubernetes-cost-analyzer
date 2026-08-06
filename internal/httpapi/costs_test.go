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

// stubReports records what the handler asked for and returns fixed data, so these tests exercise
// routing, validation, serialisation and cache headers with no database.
type stubReports struct {
	summary []postgres.CostSummaryRow
	page    postgres.AllocationsPage
	err     error

	containerStats []postgres.ContainerStats

	gotSummaryParams postgres.CostSummaryParams
	gotAllocParams   postgres.AllocationsParams
	gotStatsParams   postgres.ContainerStatsParams
}

func (s *stubReports) CostSummary(_ context.Context, p postgres.CostSummaryParams) ([]postgres.CostSummaryRow, error) {
	s.gotSummaryParams = p
	return s.summary, s.err
}

func (s *stubReports) Allocations(_ context.Context, p postgres.AllocationsParams) (postgres.AllocationsPage, error) {
	s.gotAllocParams = p
	return s.page, s.err
}

func (s *stubReports) ContainerStats(_ context.Context, p postgres.ContainerStatsParams) ([]postgres.ContainerStats, error) {
	s.gotStatsParams = p
	return s.containerStats, s.err
}

func routerWithReports(reports interface {
	Reports
	Stats
}) http.Handler {
	return NewRouter(RouterOptions{
		Log: discardLogger(), Readiness: health.NewAggregator(time.Second),
		Inventory: &stubInventory{}, Pricer: defaultStubPricer(),
		Reports: reports, Stats: reports,
		Recommender: recommend.NewEngine(recommend.DefaultThresholds()),
	})
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// =============================================================================
// Cost summary
// =============================================================================

func TestCostSummary_Defaults(t *testing.T) {
	t.Parallel()

	reports := &stubReports{}
	rec := httptest.NewRecorder()
	routerWithReports(reports).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	p := reports.gotSummaryParams
	// Namespace is the grouping a dashboard leads with, so it is the default.
	if p.GroupBy != postgres.GroupByNamespace {
		t.Errorf("GroupBy = %q, want %q by default", p.GroupBy, postgres.GroupByNamespace)
	}
	// Descending total cost: a cost report is read to find what is EXPENSIVE.
	if p.SortBy != postgres.SortByTotalCost || !p.Descending {
		t.Errorf("sort = %q descending=%v, want total_cost descending", p.SortBy, p.Descending)
	}
	// A 24-hour default range, so an unparameterised request returns something useful rather than
	// everything ever recorded.
	if d := p.To.Sub(p.From); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("default range is %v, want about 24h", d)
	}
}

// TestCostSummary_EchoesTheRangeBack covers a detail that is easy to dismiss. A client that omitted
// `from` was given a default it never chose, and a chart labelled with a range the server picked is
// the only way that range is knowable.
func TestCostSummary_EchoesTheRangeBack(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	routerWithReports(&stubReports{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/costs/summary?from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z&group_by=team", nil))

	var got summaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got.From.Format(time.RFC3339) != "2026-08-01T00:00:00Z" {
		t.Errorf("From = %s, want the requested value echoed back", got.From)
	}
	if got.GroupBy != "team" {
		t.Errorf("GroupBy = %q, want team", got.GroupBy)
	}
}

// TestCostSummary_TotalsAreSummedFromTheReturnedRows pins the decision to total in Go rather than
// with a second SQL aggregate: a separate query could see different data, so the headline figure
// could disagree with the rows beneath it.
func TestCostSummary_TotalsAreSummedFromTheReturnedRows(t *testing.T) {
	t.Parallel()

	reports := &stubReports{summary: []postgres.CostSummaryRow{
		{Namespace: "a", CPUCost: dec("0.10"), MemoryCost: dec("0.01"), TotalCost: dec("0.11")},
		{Namespace: "b", CPUCost: dec("0.20"), MemoryCost: dec("0.02"), TotalCost: dec("0.22")},
	}}

	rec := httptest.NewRecorder()
	routerWithReports(reports).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil))

	var got summaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got.Totals.TotalCost != "0.33" {
		t.Errorf("totals.total_cost = %q, want 0.33", got.Totals.TotalCost)
	}
	if got.Totals.Truncated {
		t.Error("Truncated = true for 2 rows under a limit of 100")
	}
}

// TestCostSummary_TruncatedIsFlagged covers a partial total presented as complete, which is simply a
// wrong number. Without the flag a client cannot tell the difference.
func TestCostSummary_TruncatedIsFlagged(t *testing.T) {
	t.Parallel()

	rows := make([]postgres.CostSummaryRow, 5)
	for i := range rows {
		rows[i] = postgres.CostSummaryRow{Namespace: "ns", TotalCost: dec("1")}
	}

	rec := httptest.NewRecorder()
	routerWithReports(&stubReports{summary: rows}).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary?limit=5", nil))

	var got summaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !got.Totals.Truncated {
		t.Error("Truncated = false when the row count reached the limit; a client would present " +
			"a partial total as complete")
	}
}

// TestCostSummary_EstimatedRatesPropagate covers provenance reaching the headline. A dashboard must
// be able to badge a figure as approximate without inspecting every row.
func TestCostSummary_EstimatedRatesPropagate(t *testing.T) {
	t.Parallel()

	reports := &stubReports{summary: []postgres.CostSummaryRow{
		{Namespace: "exact", TotalCost: dec("1"), EstimatedRates: false},
		{Namespace: "guessed", TotalCost: dec("1"), EstimatedRates: true},
	}}

	rec := httptest.NewRecorder()
	routerWithReports(reports).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil))

	var got summaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !got.Totals.EstimatedRates {
		t.Error("totals.estimated_rates = false when one group was priced from a fallback; " +
			"a guessed figure must not be indistinguishable from a real one")
	}
}

// TestCostSummary_MoneyIsAJSONString repeats the contract from the nodes endpoint: a JSON number is
// parsed as a double by every browser.
func TestCostSummary_MoneyIsAJSONString(t *testing.T) {
	t.Parallel()

	reports := &stubReports{summary: []postgres.CostSummaryRow{
		{Namespace: "a", TotalCost: dec("0.0000000001")},
	}}

	rec := httptest.NewRecorder()
	routerWithReports(reports).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw["items"], &items); err != nil {
		t.Fatalf("invalid items: %v", err)
	}
	if got := string(items[0]["total_cost"]); !strings.HasPrefix(got, `"`) {
		t.Errorf("total_cost = %s, want a quoted string; a bare number loses precision in every "+
			"JS client", got)
	}
}

// TestCostSummary_CacheHeadersArePrivate covers a data leak dressed as an optimisation: a shared
// proxy caching one caller's cost report and serving it to another.
func TestCostSummary_CacheHeadersArePrivate(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	routerWithReports(&stubReports{}).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil))

	cc := rec.Header().Get("Cache-Control")
	if !strings.Contains(cc, "private") {
		t.Errorf("Cache-Control = %q, want private: these responses are scoped to whoever "+
			"authenticated, and a shared cache serving one caller's report to another is a leak", cc)
	}
	if !strings.Contains(cc, "max-age=") {
		t.Errorf("Cache-Control = %q, want a max-age", cc)
	}
	if v := rec.Header().Get("Vary"); !strings.Contains(v, "Authorization") {
		t.Errorf("Vary = %q, want Authorization so a cache cannot serve one key's data to another", v)
	}
}

// =============================================================================
// Validation
// =============================================================================

// TestCostSummary_ValidationNamesEveryBadParameter is what makes a 400 actionable. "invalid request"
// gives a frontend nothing to highlight and a human nothing to fix.
func TestCostSummary_ValidationNamesEveryBadParameter(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	routerWithReports(&stubReports{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/costs/summary?group_by=nonsense&sort=nonsense&order=sideways&limit=0", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}

	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error response is not our envelope: %v", err)
	}
	if body.Error.Code != "invalid_parameter" {
		t.Errorf("code = %q, want invalid_parameter", body.Error.Code)
	}

	// ALL FOUR, not just the first: three bad parameters should cost one round trip, not three.
	named := map[string]bool{}
	for _, f := range body.Error.Fields {
		named[f.Field] = true
	}
	for _, want := range []string{"group_by", "sort", "order", "limit"} {
		if !named[want] {
			t.Errorf("field %q is not named in the response; got %+v", want, body.Error.Fields)
		}
	}
	// The message must list the valid values, or a caller has to read the source to recover.
	joined := ""
	for _, f := range body.Error.Fields {
		if f.Field == "group_by" {
			joined = f.Reason
		}
	}
	if !strings.Contains(joined, "namespace") || !strings.Contains(joined, "team") {
		t.Errorf("group_by reason %q does not list the valid options", joined)
	}
}

func TestCostSummary_RejectsInvalidRanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query string
		field string
		why   string
	}{
		{
			name: "not RFC3339", query: "from=01/02/2026", field: "from",
			why: "one unambiguous format only -- 01/02 is January 2nd or February 1st depending on " +
				"where the caller lives, and guessing shifts a report by a month",
		},
		{
			name: "no timezone", query: "from=2026-08-01T00:00:00", field: "from",
			why: "a timestamp with no zone is not a point in time",
		},
		{
			name: "reversed range", query: "from=2026-08-02T00:00:00Z&to=2026-08-01T00:00:00Z", field: "to",
			why: "returns nothing, which looks identical to 'there is no data'",
		},
		{
			name: "zero-length range", query: "from=2026-08-01T00:00:00Z&to=2026-08-01T00:00:00Z", field: "to",
			why: "half-open, so this range contains nothing",
		},
		{
			name: "range too wide", query: "from=2000-01-01T00:00:00Z&to=2026-01-01T00:00:00Z", field: "from",
			why: "aggregating billions of rows into memory is a denial of service one curl can trigger",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			routerWithReports(&stubReports{}).ServeHTTP(rec,
				httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary?"+tt.query, nil))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400\nwhy: %s", rec.Code, tt.why)
			}
			var body ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("invalid error body: %v", err)
			}
			found := false
			for _, f := range body.Error.Fields {
				if f.Field == tt.field {
					found = true
				}
			}
			if !found {
				t.Errorf("field %q not named; got %+v\nwhy: %s", tt.field, body.Error.Fields, tt.why)
			}
		})
	}
}

// TestCostSummary_LimitIsRefusedNotClamped covers a subtle client-breaking behaviour. Silently
// returning 100 rows to someone who asked for 10,000 makes them believe they have everything.
func TestCostSummary_LimitIsRefusedNotClamped(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	routerWithReports(&stubReports{}).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary?limit=100000", nil))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: an over-large limit must be refused, not silently clamped",
			rec.Code)
	}
}

// TestCostSummary_UnknownParametersAreIgnored covers the deliberate asymmetry with the pricing
// catalogue, which rejects unknown keys. Query strings are assembled by clients, proxies and
// analytics tools that append their own parameters.
func TestCostSummary_UnknownParametersAreIgnored(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	routerWithReports(&stubReports{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/costs/summary?utm_source=slack&_=1234567890", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: an unrecognised query parameter must not break a caller",
			rec.Code)
	}
}

func TestCostSummary_FiltersArePassedThrough(t *testing.T) {
	t.Parallel()

	reports := &stubReports{}
	rec := httptest.NewRecorder()
	routerWithReports(reports).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/costs/summary?namespace=team-payments&team=payments&capacity_type=spot&estimated_only=true", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	f := reports.gotSummaryParams.Filters
	if f.Namespace != "team-payments" || f.Team != "payments" || f.CapacityType != "spot" || !f.EstimatedOnly {
		t.Errorf("filters not plumbed through: %+v", f)
	}
}

func TestCostSummary_RejectsInvalidCapacityType(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	routerWithReports(&stubReports{}).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary?capacity_type=Spot", nil))

	// "Spot" would match nothing and look like an absence of spot capacity, so it is worth catching.
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for capacity_type=Spot", rec.Code)
	}
}

// =============================================================================
// Allocations and pagination
// =============================================================================

func TestAllocations_ReturnsNextCursorWhenMore(t *testing.T) {
	t.Parallel()

	reports := &stubReports{page: postgres.AllocationsPage{
		Rows:       []postgres.AllocationRow{{Namespace: "ns", PodName: "p", Container: "c"}},
		NextCursor: "abc123",
	}}

	rec := httptest.NewRecorder()
	routerWithReports(reports).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/allocations", nil))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if string(raw["next_cursor"]) != `"abc123"` {
		t.Errorf("next_cursor = %s, want abc123", raw["next_cursor"])
	}
}

// TestAllocations_OmitsCursorOnLastPage pins the "presence answers the question" contract. An empty
// string would force every client to check for both absence and emptiness.
func TestAllocations_OmitsCursorOnLastPage(t *testing.T) {
	t.Parallel()

	reports := &stubReports{page: postgres.AllocationsPage{
		Rows: []postgres.AllocationRow{{Namespace: "ns", PodName: "p", Container: "c"}},
	}}

	rec := httptest.NewRecorder()
	routerWithReports(reports).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/allocations", nil))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, present := raw["next_cursor"]; present {
		t.Error("next_cursor is present on the last page; it must be omitted so its PRESENCE " +
			"answers 'is there more?'")
	}
}

// TestAllocations_RejectsMalformedCursor covers a stale or corrupted cursor. Silently restarting
// from page one would make a looping client re-read the first page forever with nothing to explain
// why.
func TestAllocations_RejectsMalformedCursor(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"not-base64!!", "aGVsbG8", ""} {
		if bad == "" {
			continue // an absent cursor is the first page, not an error
		}
		rec := httptest.NewRecorder()
		routerWithReports(&stubReports{}).ServeHTTP(rec,
			httptest.NewRequest(http.MethodGet, "/api/v1/allocations?cursor="+bad, nil))

		if rec.Code != http.StatusBadRequest {
			t.Errorf("cursor=%q gave status %d, want 400", bad, rec.Code)
		}
		// The internal cursor format must NOT be described in the error, or clients learn to
		// depend on it.
		if strings.Contains(rec.Body.String(), "base64") || strings.Contains(rec.Body.String(), "json") {
			t.Errorf("error leaks the cursor's internal format: %s", rec.Body.String())
		}
	}
}

func TestAllocations_ValidCursorIsPassedThrough(t *testing.T) {
	t.Parallel()

	cursor, err := postgres.Cursor{
		WindowStart:   time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC),
		PodID:         42,
		ContainerName: "app",
	}.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	reports := &stubReports{}
	rec := httptest.NewRecorder()
	routerWithReports(reports).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/api/v1/allocations?cursor="+cursor, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	got := reports.gotAllocParams.Cursor
	if got == nil {
		t.Fatal("cursor was not passed through")
	}
	if got.PodID != 42 || got.ContainerName != "app" {
		t.Errorf("cursor = %+v, want PodID 42 and container app", got)
	}
}

func TestCostEndpoints_RepositoryErrorIs500(t *testing.T) {
	t.Parallel()

	reports := &stubReports{err: errors.New("relation container_allocations does not exist")}

	for _, path := range []string{"/api/v1/costs/summary", "/api/v1/allocations"} {
		rec := httptest.NewRecorder()
		routerWithReports(reports).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s gave status %d, want 500", path, rec.Code)
		}
		// The internal error must not reach the client: it names our schema.
		if strings.Contains(rec.Body.String(), "container_allocations") {
			t.Errorf("%s leaked the internal error: %s", path, rec.Body.String())
		}
	}
}

// TestCostSummary_TotalsIncludeWasteAndQuantities covers fields added in Phase 8, and the reason they
// were added is the interesting part.
//
// The dashboard needed total waste as a headline figure and COULD NOT HONESTLY COMPUTE IT. Two
// independent reasons, either sufficient: these are exact decimals of up to 26 significant digits, so
// summing them in JavaScript means parseFloat and a silent truncation that compounds across rows -- and
// when Truncated is true the returned rows are not the whole answer, so no client-side care produces the
// real figure.
//
// It was found by the frontend's TypeScript build refusing to compile against a field the OpenAPI spec
// did not declare. That is the drift chain working in the direction it was built for: SQL to Go to spec
// to TypeScript, with the last link failing loudly at compile time rather than rendering "undefined" to
// a user.
func TestCostSummary_TotalsIncludeWasteAndQuantities(t *testing.T) {
	t.Parallel()

	reports := &stubReports{summary: []postgres.CostSummaryRow{
		{
			Namespace: "team-a",
			TotalCost: dec("1.5"), CPUCost: dec("1.0"), MemoryCost: dec("0.5"),
			CPUCoreHours: dec("10"), MemGiBHours: dec("20"),
			WastedCPUCoreHours: dec("4"), WastedMemGiBHours: dec("8"),
		},
		{
			Namespace: "team-b",
			TotalCost: dec("2.5"), CPUCost: dec("2.0"), MemoryCost: dec("0.5"),
			CPUCoreHours: dec("30"), MemGiBHours: dec("40"),
			WastedCPUCoreHours: dec("6"), WastedMemGiBHours: dec("12"),
		},
	}}

	rec := httptest.NewRecorder()
	routerWithReports(reports).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/costs/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var got summaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	checks := []struct{ name, got, want string }{
		{"cpu_billable_core_hours", got.Totals.CPUCoreHours, "40"},
		{"memory_billable_gib_hours", got.Totals.MemGiBHours, "60"},
		{"wasted_cpu_core_hours", got.Totals.WastedCPUCoreHours, "10"},
		{"wasted_memory_gib_hours", got.Totals.WastedMemGiBHours, "20"},
	}
	for _, c := range checks {
		if !dec(c.got).Equal(dec(c.want)) {
			t.Errorf("%s = %s, want %s", c.name, c.got, c.want)
		}
	}
}

// TestCostSummary_WasteIsSummedNotSubtracted pins the arithmetic that Phase 6 got wrong at the SQL level
// and that this layer must not reintroduce.
//
// The rows arrive with waste already floored per fact row inside the query. Summing those is correct.
// Computing `requested - used` from the TOTALS here would let an under-requested group credit against a
// wasteful one -- the exact bug that reported kube-system as having zero memory waste while it held 50
// GiB-hours of it. The fix has to hold at every level that aggregates, not just the first.
func TestCostSummary_WasteIsSummedNotSubtracted(t *testing.T) {
	t.Parallel()

	// One wasteful group, and one whose USED exceeds its REQUESTED. If waste were derived at this level
	// the second would subtract from the first; summed per-row floored values, it contributes its own
	// floored figure and nothing else.
	reports := &stubReports{summary: []postgres.CostSummaryRow{
		{
			Namespace:             "wasteful",
			CPURequestedCoreHours: dec("100"), CPUUsedCoreHours: dec("10"),
			WastedCPUCoreHours: dec("90"),
			TotalCost:          dec("1"), CPUCost: dec("1"), MemoryCost: dec("0"),
		},
		{
			Namespace:             "under-requested",
			CPURequestedCoreHours: dec("10"), CPUUsedCoreHours: dec("1000"),
			// Floored at zero by the query, never negative.
			WastedCPUCoreHours: dec("0"),
			TotalCost:          dec("1"), CPUCost: dec("1"), MemoryCost: dec("0"),
		},
	}}

	rec := httptest.NewRecorder()
	routerWithReports(reports).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/costs/summary", nil))

	var got summaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if !dec(got.Totals.WastedCPUCoreHours).Equal(dec("90")) {
		t.Errorf("wasted_cpu_core_hours = %s, want 90.\n"+
			"0 would mean the total was derived as sum(requested) - sum(used) at this level, so the "+
			"under-requested group credited against real waste -- the Phase 6 bug, one layer up",
			got.Totals.WastedCPUCoreHours)
	}
}
