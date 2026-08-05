package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

// WHY THIS FILE EXISTS AT ALL
// ---------------------------
// CostSummary, Allocations and ContainerStats are the three queries that actually serve the API, and
// until now none of them had a repository test. The only half-open-boundary test in the repo was
// attached to CostByNamespace -- a Phase 2 method that Phase 5 superseded and which nothing outside
// the tests ever called.
//
// So the most-exercised SQL in the project was untested while a dead method was carefully guarded.
// That is a specific kind of rot worth naming: tests accumulate around the code that was easy to
// test first, and the code that grows on top of it inherits the confidence without earning it.
//
// EVERY TEST HERE FILTERS BY THE FIXTURE'S NAMESPACE. Rollback isolates writes, not reads -- see the
// note on the fixture struct. A test that omits the filter is asserting on whatever the collector
// last committed.

// seedWindows inserts n consecutive five-minute windows starting at `start`, one container each,
// and returns the times inserted.
func seedWindows(t *testing.T, ctx context.Context, db Querier, f fixture, start time.Time, n int,
	mutate func(*domain.ContainerAllocation, int),
) []time.Time {
	t.Helper()
	repo := NewAllocationRepository(db)

	times := make([]time.Time, 0, n)
	for i := range n {
		ts := start.Add(time.Duration(i) * 5 * time.Minute)
		a := baseAllocation(f, ts)
		if mutate != nil {
			mutate(&a, i)
		}
		if err := repo.Insert(ctx, a); err != nil {
			t.Fatalf("insert window %d at %s: %v", i, ts, err)
		}
		times = append(times, ts)
	}
	return times
}

// onlyRow asserts the query matched exactly the fixture and returns that row.
func onlyRow(t *testing.T, rows []CostSummaryRow) CostSummaryRow {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want exactly 1 (the namespace filter should have isolated the "+
			"fixture): %+v", len(rows), rows)
	}
	return rows[0]
}

// =============================================================================
// The half-open interval
// =============================================================================

// TestCostSummary_RangeIsHalfOpen is the boundary test, ported from the deleted CostByNamespace
// suite because the property it guards moved rather than went away.
//
// Windows are [start, end). A range query must be >= from AND < to. BETWEEN would be closed on both
// ends and would include the window sitting exactly on `to` -- which is also the FIRST window of the
// next period. August and September would each count the 1 September 00:00 window, and twelve
// monthly reports would sum to more than the year. The error is small, permanent, and invisible
// unless you go looking for it.
func TestCostSummary_RangeIsHalfOpen(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	// Three windows: one before `from`, one inside, one exactly ON `to`.
	base := time.Date(2026, 8, 4, 8, 55, 0, 0, time.UTC)
	seedWindows(t, ctx, tx, f, base, 3, func(a *domain.ContainerAllocation, _ int) {
		a.CPUCost = mustDec(t, "1.0000000000")
		a.MemoryCost = decimal.Zero
	})

	from := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 4, 9, 5, 0, 0, time.UTC)

	rows, err := repo.CostSummary(ctx, CostSummaryParams{
		From: from, To: to,
		GroupBy: GroupByNamespace,
		Filters: Filters{Namespace: f.namespaceName},
		SortBy:  SortByTotalCost, Descending: true, Limit: 100,
	})
	if err != nil {
		t.Fatalf("CostSummary: %v", err)
	}

	row := onlyRow(t, rows)
	if !row.TotalCost.Equal(mustDec(t, "1.0000000000")) {
		t.Errorf("TotalCost = %s, want exactly 1.0 -- one window in [09:00, 09:05).\n"+
			"2.0 means the upper bound was included, so every pair of adjacent reports "+
			"double-counts their shared boundary window", row.TotalCost)
	}
	if row.Containers != 1 {
		t.Errorf("container_windows = %d, want 1", row.Containers)
	}
}

// TestCostSummary_EmptyRangeIsAnEmptySlice guards the JSON contract at the source.
//
// A nil slice marshals to `null` and an empty one to `[]`. Every client then has to nil-check before
// iterating, and one of them will not -- so "no cost in this window" becomes a crash rather than an
// empty chart. Fixing it in the handler would leave the next caller of the repository exposed.
func TestCostSummary_EmptyRangeIsAnEmptySlice(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	rows, err := repo.CostSummary(ctx, CostSummaryParams{
		From: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2001, 1, 2, 0, 0, 0, 0, time.UTC),
		// A range in 2001, which has no partition and no data.
		GroupBy: GroupByNamespace,
		Filters: Filters{Namespace: f.namespaceName},
		SortBy:  SortByTotalCost, Limit: 100,
	})
	if err != nil {
		t.Fatalf("CostSummary over an empty range: %v", err)
	}
	if rows == nil {
		t.Error("returned a nil slice; want an empty one so it marshals to [] rather than null")
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows for a range with no data", len(rows))
	}
}

// =============================================================================
// The arithmetic
// =============================================================================

// TestCostSummary_CoreHourMaths checks the unit conversion, which is done in SQL rather than Go so
// Postgres can do it during aggregation instead of shipping every row over the wire.
//
// The conversion is where a cost report goes quietly wrong: a factor-of-1000 slip between cores and
// millicores, or 3600 between seconds and hours, produces a number that is plausible, consistent and
// wrong. Nobody notices until it is compared against a real invoice.
func TestCostSummary_CoreHourMaths(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	a := baseAllocation(f, start)
	a.WindowEnd = start.Add(time.Hour) // a full hour, so the arithmetic is checkable by eye
	a.CPUMillicoresRequested = 500
	a.CPUMillicoresUsed = 100
	a.MemoryBytesRequested = 2 * 1024 * 1024 * 1024 // 2 GiB
	a.MemoryBytesUsed = 1024 * 1024 * 1024          // 1 GiB

	if err := NewAllocationRepository(tx).Insert(ctx, a); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := repo.CostSummary(ctx, CostSummaryParams{
		From: start, To: start.Add(2 * time.Hour),
		GroupBy: GroupByNamespace,
		Filters: Filters{Namespace: f.namespaceName},
		SortBy:  SortByTotalCost, Limit: 100,
	})
	if err != nil {
		t.Fatalf("CostSummary: %v", err)
	}
	row := onlyRow(t, rows)

	checks := []struct {
		name string
		got  decimal.Decimal
		want string
		why  string
	}{
		{"cpu_requested_core_hours", row.CPURequestedCoreHours, "0.5",
			"500 millicores for 1 hour = 0.5 core-hours"},
		{"cpu_used_core_hours", row.CPUUsedCoreHours, "0.1",
			"100 millicores for 1 hour = 0.1 core-hours"},
		{"cpu_billable_core_hours", row.CPUCoreHours, "0.5",
			"billable is max(requested, used), and 500 > 100"},
		{"memory_requested_gib_hours", row.MemRequestedGiBHours, "2",
			"2 GiB for 1 hour = 2 GiB-hours"},
		{"memory_used_gib_hours", row.MemUsedGiBHours, "1", "1 GiB for 1 hour"},
		{"wasted_cpu_core_hours", row.WastedCPUCoreHours, "0.4",
			"requested minus used: 0.5 - 0.1"},
		{"wasted_memory_gib_hours", row.WastedMemGiBHours, "1", "2 GiB reserved, 1 GiB used"},
	}
	for _, c := range checks {
		if !c.got.Equal(mustDec(t, c.want)) {
			t.Errorf("%s = %s, want %s (%s)", c.name, c.got, c.want, c.why)
		}
	}
}

// TestCostSummary_WasteIsFlooredAtZero pins a decision that looks like a rounding detail and is not.
//
// A container using MORE than it requested is not saving anyone money -- it is under-requested, which
// is a reliability problem reported separately. If waste were allowed to go negative it would offset
// genuine waste inside a SUM, so a team with one under-requested pod would look more efficient than a
// team with none. The metric would reward the riskier configuration.
//
// THIS TEST FOUND THAT EXACT BUG ON ITS FIRST RUN. The floor was GREATEST(sum(req) - sum(used), 0)
// applied after the GROUP BY, which prevents a negative TOTAL without preventing the cancellation --
// that happens inside the two sums, before the subtraction. Measured against the real fact rows at the
// time of the fix, memory waste was understated by:
//
//	kube-system     0.00 GiB-hours reported vs 50.07 actual   <- the whole namespace floored to zero
//	monitoring     35.01 reported vs 69.04 actual             (97% understated)
//	team-search    18.55 reported vs 41.97 actual             (126% understated)
//
// kube-system is the one to sit with. A tool built to find memory waste reported that a namespace
// holding 50 GiB-hours of it had NONE, because its under-requested containers cancelled the rest out.
// The doc comment on WastedCPUCoreHours described the correct behaviour the entire time.
func TestCostSummary_WasteIsFlooredAtZero(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)

	// Two windows: one wasteful, one under-requested by MORE than the first wasted.
	wasteful := baseAllocation(f, start)
	wasteful.CPUMillicoresRequested, wasteful.CPUMillicoresUsed = 100, 10

	over := baseAllocation(f, start.Add(5*time.Minute))
	over.ContainerName = "greedy"
	over.CPUMillicoresRequested, over.CPUMillicoresUsed = 10, 1000

	repo2 := NewAllocationRepository(tx)
	for _, a := range []domain.ContainerAllocation{wasteful, over} {
		if err := repo2.Insert(ctx, a); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	rows, err := repo.CostSummary(ctx, CostSummaryParams{
		From: start, To: start.Add(time.Hour),
		GroupBy: GroupByNamespace,
		Filters: Filters{Namespace: f.namespaceName},
		SortBy:  SortByTotalCost, Limit: 100,
	})
	if err != nil {
		t.Fatalf("CostSummary: %v", err)
	}
	row := onlyRow(t, rows)

	if row.WastedCPUCoreHours.IsNegative() {
		t.Fatalf("wasted_cpu_core_hours = %s: waste cannot be negative", row.WastedCPUCoreHours)
	}
	// 90 millicores wasted for 5 minutes = 0.0075 core-hours. The under-requested container
	// contributes ZERO waste, not -0.0825.
	if !row.WastedCPUCoreHours.Equal(mustDec(t, "0.0075")) {
		t.Errorf("wasted_cpu_core_hours = %s, want 0.0075.\n"+
			"The under-requested container must contribute 0 rather than a negative figure that "+
			"cancels out the genuine waste beside it", row.WastedCPUCoreHours)
	}
}

// =============================================================================
// Grouping and provenance
// =============================================================================

// TestCostSummary_GroupingPopulatesOnlyItsOwnDimensions checks the response shape per grouping.
//
// The row has eleven dimension fields and each grouping fills a different subset. A grouping that
// left a stale field populated would produce a row claiming to be grouped by team while carrying a
// pod name, and a client rendering "team / pod" would show a nonsense pair.
func TestCostSummary_GroupingPopulatesOnlyItsOwnDimensions(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	seedWindows(t, ctx, tx, f, start, 2, nil)

	tests := []struct {
		groupBy  GroupBy
		wantSet  func(CostSummaryRow) bool
		wantZero func(CostSummaryRow) bool
		why      string
	}{
		{GroupByNamespace,
			func(r CostSummaryRow) bool { return r.Namespace != "" },
			func(r CostSummaryRow) bool { return r.PodName == "" && r.Container == "" },
			"grouping by namespace must not carry a pod name up from the rows it aggregated"},
		{GroupByTeam,
			func(r CostSummaryRow) bool { return r.Team != "" },
			func(r CostSummaryRow) bool { return r.Namespace == "" },
			"a team spans namespaces, so naming one of them would be arbitrary"},
		{GroupByWorkload,
			func(r CostSummaryRow) bool {
				return r.Namespace != "" && r.WorkloadKind != "" && r.WorkloadName != ""
			},
			func(r CostSummaryRow) bool { return r.PodName == "" },
			"a workload is identified by all three: two namespaces may both have a Deployment/api"},
		{GroupByNode,
			func(r CostSummaryRow) bool { return r.Node != "" },
			func(r CostSummaryRow) bool { return r.Namespace == "" },
			"a node hosts many namespaces"},
	}

	for _, tt := range tests {
		t.Run(string(tt.groupBy), func(t *testing.T) {
			rows, err := repo.CostSummary(ctx, CostSummaryParams{
				From: start, To: start.Add(time.Hour),
				GroupBy: tt.groupBy,
				Filters: Filters{Namespace: f.namespaceName},
				SortBy:  SortByTotalCost, Limit: 100,
			})
			if err != nil {
				t.Fatalf("CostSummary group_by=%s: %v", tt.groupBy, err)
			}
			row := onlyRow(t, rows)
			if !tt.wantSet(row) {
				t.Errorf("group_by=%s left its own dimension empty: %+v", tt.groupBy, row)
			}
			if !tt.wantZero(row) {
				t.Errorf("group_by=%s populated a dimension it does not group by: %+v\nwhy: %s",
					tt.groupBy, row, tt.why)
			}
		})
	}
}

// TestCostSummary_EstimatedRatesIsPerGroup checks the provenance flag survives aggregation.
//
// Aggregating it away would waste the entire point of recording rate_source. A report where one
// team's cost came from a fallback guess and the rest came from the catalogue should say WHICH -- a
// single request-level flag would tell a reader their whole report is suspect when only one row is.
func TestCostSummary_EstimatedRatesIsPerGroup(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	// Two windows, ONE of them fallback-priced. Any is enough to taint the group.
	seedWindows(t, ctx, tx, f, start, 2, func(a *domain.ContainerAllocation, i int) {
		if i == 1 {
			a.RateSource = "fallback"
		} else {
			a.RateSource = "catalogue"
		}
	})

	rows, err := repo.CostSummary(ctx, CostSummaryParams{
		From: start, To: start.Add(time.Hour),
		GroupBy: GroupByNamespace,
		Filters: Filters{Namespace: f.namespaceName},
		SortBy:  SortByTotalCost, Limit: 100,
	})
	if err != nil {
		t.Fatalf("CostSummary: %v", err)
	}
	if !onlyRow(t, rows).EstimatedRates {
		t.Error("estimated_rates = false, but one of the two aggregated windows was fallback-priced: " +
			"a group is an estimate the moment any part of it is")
	}

	// And the EstimatedOnly filter must select exactly that row rather than the whole group.
	only, err := repo.CostSummary(ctx, CostSummaryParams{
		From: start, To: start.Add(time.Hour),
		GroupBy: GroupByNamespace,
		Filters: Filters{Namespace: f.namespaceName, EstimatedOnly: true},
		SortBy:  SortByTotalCost, Limit: 100,
	})
	if err != nil {
		t.Fatalf("CostSummary estimated_only: %v", err)
	}
	if got := onlyRow(t, only).Containers; got != 1 {
		t.Errorf("estimated_only matched %d container-windows, want 1: it answers 'how much of this "+
			"bill is not real?', so it must exclude the catalogue-priced window", got)
	}
}

// TestCostSummary_FiltersAreAnd confirms filters compose rather than replace one another.
//
// Combining namespace and team must narrow. If they were OR-ed, a team asking for its own namespace
// would receive every namespace belonging to the team plus every team in the namespace -- more data
// than it asked for, which is the worse direction for a filter to fail in.
func TestCostSummary_FiltersAreAnd(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	seedWindows(t, ctx, tx, f, start, 1, nil)

	// The fixture's team is "payments". A matching namespace with a NON-matching team must return
	// nothing, which only holds if the predicates are AND-ed.
	rows, err := repo.CostSummary(ctx, CostSummaryParams{
		From: start, To: start.Add(time.Hour),
		GroupBy: GroupByNamespace,
		Filters: Filters{Namespace: f.namespaceName, Team: "not-the-fixtures-team"},
		SortBy:  SortByTotalCost, Limit: 100,
	})
	if err != nil {
		t.Fatalf("CostSummary: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows for a matching namespace and a non-matching team; filters must be "+
			"AND-ed, so this must be empty: %+v", len(rows), rows)
	}
}

// TestCostSummary_RejectsUnknownGroupingAndSort is the injection control, tested as a control.
//
// A column NAME cannot be a bound parameter -- placeholders bind values, not identifiers -- so
// group_by and sort are resolved through allow-list maps. That makes the allow-list the only thing
// standing between a query string and the SQL, which is worth a test that tries to get past it
// rather than a comment saying it is safe.
func TestCostSummary_RejectsUnknownGroupingAndSort(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	base := CostSummaryParams{
		From:    time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		To:      time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC),
		GroupBy: GroupByNamespace, SortBy: SortByTotalCost,
		Filters: Filters{Namespace: f.namespaceName}, Limit: 100,
	}

	t.Run("grouping", func(t *testing.T) {
		p := base
		p.GroupBy = GroupBy("namespace_name; DROP TABLE container_allocations")
		if _, err := repo.CostSummary(ctx, p); err == nil {
			t.Error("an unknown grouping was accepted; it must be refused before reaching the SQL")
		}
	})
	t.Run("sort", func(t *testing.T) {
		p := base
		p.SortBy = SortField("total_cost) --")
		if _, err := repo.CostSummary(ctx, p); err == nil {
			t.Error("an unknown sort field was accepted")
		}
	})

	// The table must still be there, which is the point of running this against a real database
	// rather than asserting on a SQL string.
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM container_allocations WHERE namespace_name = $1`,
		f.namespaceName).Scan(&n); err != nil {
		t.Fatalf("the fact table is not queryable after the rejected input: %v", err)
	}
}
