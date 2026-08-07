package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

// THE ISOLATED DAYS, and why every test here uses one.
//
// RollupDay is deliberately NOT scoped to a namespace or a cluster: it reprojects the WHOLE day, which
// is what makes its output a function of its input and re-running it genuinely idempotent. That is the
// right design and it costs these tests the isolation trick the other repository tests rely on.
//
// Rollback isolates writes but not reads, so a test transaction sees every fact row the collector has
// committed -- and on a day the collector actually ran, that is over ten thousand rows. Three of these
// tests were written against 2026-08-04 and failed immediately with "read 10662 fact rows, want 6".
//
// Filtering is not available, so the fixture goes somewhere the collector has never been. 2027 is past
// the seeded monthly partitions, so these rows land in container_allocations_default -- which also
// quietly exercises the default partition, the one that catches anything the seeded range misses.
var (
	isolatedDay  = Day(2027, 6, 15)
	isolatedDay2 = Day(2027, 6, 16)
)

// rollupOneDay rolls up a day and fails the test on error.
func rollupOneDay(t *testing.T, ctx context.Context, repo *RollupRepository, day Date) DayResult {
	t.Helper()
	res, err := repo.RollupDay(ctx, day)
	if err != nil {
		t.Fatalf("RollupDay(%s): %v", day, err)
	}
	return res
}

// =============================================================================
// Correctness: the rollup must equal the fact table
// =============================================================================

// TestRollupDay_TotalsEqualTheFactTable is the test the entire phase rests on.
//
// A rollup that is 293x cheaper and disagrees with the source by a rounding error is worse than no
// rollup, because two views of the same month now give two answers and neither is obviously wrong. So
// this asserts EXACT equality on exact decimals rather than approximate equality within a tolerance --
// which is possible only because money and core-hours are numeric all the way through. Had any step
// used a float, this test would be the one that could not be written.
func TestRollupDay_TotalsEqualTheFactTable(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	day := isolatedDay
	start := day.Time().Add(9 * time.Hour)

	// Varied figures across several containers and windows, including an UNDER-REQUESTED container, so
	// the per-row waste floor is exercised rather than assumed.
	seedWindows(t, ctx, tx, f, start, 6, func(a *domain.ContainerAllocation, i int) {
		a.ContainerName = []string{"app", "sidecar", "greedy"}[i%3]
		a.CPUMillicoresRequested = int64(100 * (i + 1))
		a.CPUMillicoresUsed = int64(10 * (i + 1))
		a.MemoryBytesRequested = int64(i+1) * 64 * testMiB
		a.MemoryBytesUsed = int64(i+1) * 16 * testMiB
		if a.ContainerName == "greedy" {
			// Uses more than it requests: contributes ZERO waste, never a credit.
			a.CPUMillicoresUsed = a.CPUMillicoresRequested * 3
			a.MemoryBytesUsed = a.MemoryBytesRequested * 3
		}
	})

	res := rollupOneDay(t, ctx, repo, day)
	if res.RollupRows == 0 {
		t.Fatal("the rollup wrote no rows")
	}
	if res.FactRows != 6 {
		t.Fatalf("read %d fact rows, want 6", res.FactRows)
	}

	// Every measure, compared between the two tables in one query so a mismatch names the column.
	rows, err := tx.Query(ctx, `
		WITH facts AS (
		    SELECT sum(total_cost) AS total_cost,
		           sum(cpu_millicores_billable::numeric/1000 * EXTRACT(EPOCH FROM (window_end-window_start))/3600) AS cpu_h,
		           sum(memory_bytes_billable::numeric/1073741824 * EXTRACT(EPOCH FROM (window_end-window_start))/3600) AS mem_h,
		           sum(GREATEST(cpu_millicores_requested - cpu_millicores_used, 0)::numeric/1000
		               * EXTRACT(EPOCH FROM (window_end-window_start))/3600) AS wasted_cpu,
		           sum(GREATEST(memory_bytes_requested - memory_bytes_used, 0)::numeric/1073741824
		               * EXTRACT(EPOCH FROM (window_end-window_start))/3600) AS wasted_mem,
		           count(*) AS windows,
		           max(cpu_millicores_max) AS cpu_max
		    FROM container_allocations
		    WHERE namespace_name = $1 AND window_start >= $2::date AND window_start < ($2::date + INTERVAL '1 day')
		), roll AS (
		    SELECT sum(total_cost) AS total_cost, sum(cpu_billable_core_hours) AS cpu_h,
		           sum(memory_billable_gib_hours) AS mem_h,
		           sum(wasted_cpu_core_hours) AS wasted_cpu, sum(wasted_memory_gib_hours) AS wasted_mem,
		           sum(window_count) AS windows, max(cpu_millicores_max) AS cpu_max
		    FROM container_allocations_daily
		    WHERE namespace_name = $1 AND day = $2::date
		)
		SELECT 'total_cost', f.total_cost::text, r.total_cost::text FROM facts f, roll r
		  WHERE f.total_cost IS DISTINCT FROM r.total_cost
		UNION ALL SELECT 'cpu_core_hours', f.cpu_h::text, r.cpu_h::text FROM facts f, roll r
		  WHERE f.cpu_h IS DISTINCT FROM r.cpu_h
		UNION ALL SELECT 'memory_gib_hours', f.mem_h::text, r.mem_h::text FROM facts f, roll r
		  WHERE f.mem_h IS DISTINCT FROM r.mem_h
		UNION ALL SELECT 'wasted_cpu', f.wasted_cpu::text, r.wasted_cpu::text FROM facts f, roll r
		  WHERE f.wasted_cpu IS DISTINCT FROM r.wasted_cpu
		UNION ALL SELECT 'wasted_memory', f.wasted_mem::text, r.wasted_mem::text FROM facts f, roll r
		  WHERE f.wasted_mem IS DISTINCT FROM r.wasted_mem
		UNION ALL SELECT 'window_count', f.windows::text, r.windows::text FROM facts f, roll r
		  WHERE f.windows IS DISTINCT FROM r.windows
		UNION ALL SELECT 'cpu_millicores_max', f.cpu_max::text, r.cpu_max::text FROM facts f, roll r
		  WHERE f.cpu_max IS DISTINCT FROM r.cpu_max`,
		f.namespaceName, day.Time())
	if err != nil {
		t.Fatalf("compare query: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var column, fromFacts, fromRollup string
		if err := rows.Scan(&column, &fromFacts, &fromRollup); err != nil {
			t.Fatalf("scan mismatch: %v", err)
		}
		t.Errorf("%s DISAGREES: facts=%s rollup=%s.\n"+
			"A rollup that does not equal its source is worse than no rollup: two views of the same "+
			"day now give two answers", column, fromFacts, fromRollup)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate mismatches: %v", err)
	}
}

// TestRollupDay_WasteIsFlooredPerRow guards the Phase 6 bug against being baked into stored history.
//
// A live query computing waste wrongly is bad and fixable by editing SQL. A ROLLUP computing it wrongly
// writes the error into a table, where every month built on top inherits it and nobody can tell by
// looking. The same floor-inside-the-sum rule therefore has to hold here, and be tested here.
func TestRollupDay_WasteIsFlooredPerRow(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)
	alloc := NewAllocationRepository(tx)

	day := isolatedDay
	start := day.Time().Add(9 * time.Hour)

	// One container wasting 90m, one under-requested by 990m. Aggregated first, these cancel to -900
	// and floor to zero -- the exact failure mode.
	wasteful := baseAllocation(f, start)
	wasteful.CPUMillicoresRequested, wasteful.CPUMillicoresUsed = 100, 10

	greedy := baseAllocation(f, start)
	greedy.ContainerName = "greedy"
	greedy.CPUMillicoresRequested, greedy.CPUMillicoresUsed = 10, 1000

	for _, a := range []domain.ContainerAllocation{wasteful, greedy} {
		if err := alloc.Insert(ctx, a); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	rollupOneDay(t, ctx, repo, day)

	var wasted string
	if err := tx.QueryRow(ctx, `
		SELECT sum(wasted_cpu_core_hours)::text FROM container_allocations_daily
		WHERE namespace_name = $1 AND day = $2::date`, f.namespaceName, day.Time()).Scan(&wasted); err != nil {
		t.Fatalf("read waste: %v", err)
	}

	// 90 millicores for 5 minutes = 0.0075 core-hours. The greedy container contributes 0.
	if got := mustDec(t, wasted); !got.Equal(mustDec(t, "0.0075")) {
		t.Errorf("wasted_cpu_core_hours = %s, want 0.0075.\n"+
			"0 means the under-requested container issued a credit against the real waste beside it, "+
			"and the rollup has now stored that error permanently", got)
	}
}

// =============================================================================
// Idempotency and the day boundary
// =============================================================================

// TestRollupDay_IsIdempotent is the property that makes the job safe to re-run, retry and backfill.
//
// It also checks the specific thing DELETE-then-INSERT buys over ON CONFLICT DO UPDATE: a dimension
// tuple that DISAPPEARS from the fact table must disappear from the rollup. An upsert would write the
// new rows and silently leave the old one behind forever -- nothing would ever reference its tuple
// again, so nothing would ever overwrite it, and it would inflate every total over that day for good.
func TestRollupDay_IsIdempotent(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	day := isolatedDay
	start := day.Time().Add(9 * time.Hour)
	seedWindows(t, ctx, tx, f, start, 4, func(a *domain.ContainerAllocation, i int) {
		a.ContainerName = []string{"app", "doomed"}[i%2]
	})

	first := rollupOneDay(t, ctx, repo, day)
	if first.Deleted != 0 {
		t.Errorf("first run deleted %d rows; there was nothing to delete", first.Deleted)
	}

	// Re-run unchanged: same rows, same totals.
	before := readDayTotals(t, ctx, tx, f.namespaceName, day)
	second := rollupOneDay(t, ctx, repo, day)
	if second.Deleted != first.RollupRows {
		t.Errorf("re-run deleted %d rows but the first wrote %d; the rewrite is not replacing the "+
			"whole day", second.Deleted, first.RollupRows)
	}
	after := readDayTotals(t, ctx, tx, f.namespaceName, day)
	if before != after {
		t.Errorf("re-running changed the day: %+v -> %+v", before, after)
	}

	// NOW REMOVE a container from the fact table and re-run. Its rollup row must vanish.
	if _, err := tx.Exec(ctx,
		`DELETE FROM container_allocations WHERE namespace_name = $1 AND container_name = 'doomed'`,
		f.namespaceName); err != nil {
		t.Fatalf("delete facts: %v", err)
	}
	rollupOneDay(t, ctx, repo, day)

	var stale int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM container_allocations_daily
		WHERE namespace_name = $1 AND day = $2::date AND container_name = 'doomed'`,
		f.namespaceName, day.Time()).Scan(&stale); err != nil {
		t.Fatalf("count stale rows: %v", err)
	}
	if stale != 0 {
		t.Errorf("%d rows survive for a container that no longer exists in the fact table.\n"+
			"This is precisely what ON CONFLICT DO UPDATE would leave behind: an orphan that nothing "+
			"will ever reference again, so nothing will ever correct it", stale)
	}
}

// dayTotals is a comparable snapshot of a rolled-up day.
type dayTotals struct {
	Rows        int
	TotalCost   string
	WindowCount int64
}

func readDayTotals(t *testing.T, ctx context.Context, db Querier, namespace string, day Date) dayTotals {
	t.Helper()
	var d dayTotals
	if err := db.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(total_cost), 0)::text, COALESCE(sum(window_count), 0)
		FROM container_allocations_daily WHERE namespace_name = $1 AND day = $2::date`,
		namespace, day.Time()).Scan(&d.Rows, &d.TotalCost, &d.WindowCount); err != nil {
		t.Fatalf("read day totals: %v", err)
	}
	return d
}

// TestRollupDay_DayIsHalfOpen checks the boundary, which compounds rather than cancels here.
//
// Every other query in this codebase uses half-open ranges, and a rollup that did not would put the
// midnight window in BOTH adjacent days. That error then propagates: the day is summed into a month, the
// month into a statement, and one duplicated window becomes a permanently overstated bill.
func TestRollupDay_DayIsHalfOpen(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)
	alloc := NewAllocationRepository(tx)

	day := isolatedDay

	// Three windows: the last of the 3rd, the FIRST of the 4th (exactly midnight), and the last of the
	// 4th. Only the middle two belong to the 4th.
	for _, ts := range []time.Time{
		day.Time().Add(-5 * time.Minute), // the previous day, 23:55
		day.Time(),                       // 00:00 exactly -- INCLUDED
		day.Time().Add(23*time.Hour + 55*time.Minute), // 23:55 -- included
		day.AddDays(1).Time(),                         // the next day at 00:00 -- EXCLUDED
	} {
		a := baseAllocation(f, ts)
		if err := alloc.Insert(ctx, a); err != nil {
			t.Fatalf("insert at %s: %v", ts, err)
		}
	}

	res := rollupOneDay(t, ctx, repo, day)
	if res.FactRows != 2 {
		t.Errorf("the day matched %d fact rows, want 2.\n"+
			"3 means the NEXT day's 00:00 window was included, so both days count it and "+
			"every month built from these days is overstated", res.FactRows)
	}

	got := readDayTotals(t, ctx, tx, f.namespaceName, day)
	if got.WindowCount != 2 {
		t.Errorf("window_count = %d, want 2", got.WindowCount)
	}
}

// TestDaysWithFacts_ListsOnlyDaysThatHaveData is what makes backfill self-describing.
func TestDaysWithFacts_ListsOnlyDaysThatHaveData(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)
	alloc := NewAllocationRepository(tx)

	// The 1st and the 4th, with a gap. Written out of order so a passing test is not an accident of
	// insertion order.
	for _, day := range []Date{isolatedDay2, isolatedDay} {
		a := baseAllocation(f, day.Time().Add(9*time.Hour))
		if err := alloc.Insert(ctx, a); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	days, err := repo.DaysWithFacts(ctx, isolatedDay, isolatedDay.AddDays(5))
	if err != nil {
		t.Fatalf("DaysWithFacts: %v", err)
	}

	// The isolated days keep the collector's committed rows out of the range, so this CAN assert the
	// exact list. The method deliberately takes no filters -- "which days need rolling up" is a
	// question about the whole table -- which is exactly why the fixture has to live somewhere the
	// collector never wrote.
	if len(days) != 2 {
		t.Fatalf("got %d days, want exactly 2 (the gap between them must not be listed): %v", len(days), days)
	}
	found := map[string]bool{}
	for i, d := range days {
		found[d.String()] = true
		if i > 0 && !days[i-1].Before(d) {
			t.Errorf("days are not ascending: %s then %s", days[i-1], d)
		}
	}
	for _, want := range []string{isolatedDay.String(), isolatedDay2.String()} {
		if !found[want] {
			t.Errorf("%s has fact rows but was not listed", want)
		}
	}
	// The upper bound is inclusive, so a day outside the range must not appear.
	narrow, err := repo.DaysWithFacts(ctx, isolatedDay, isolatedDay)
	if err != nil {
		t.Fatalf("DaysWithFacts narrow: %v", err)
	}
	for _, d := range narrow {
		if d.After(isolatedDay) {
			t.Errorf("%s is outside the requested range", d)
		}
	}
}

// =============================================================================
// Source routing
// =============================================================================

// TestTrend_SourceRouting pins the decision that makes a rollup worth having.
//
// A rollup only pays off if something chooses to read it, and only stays CORRECT if that something
// declines to read it for the questions it cannot answer. Both halves are asserted, because getting
// either wrong is silent: always reading facts wastes the rollup, and always reading the rollup
// returns an hourly chart made of daily buckets.
func TestTrend_SourceRouting(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	day := isolatedDay
	seedWindows(t, ctx, tx, f, day.Time().Add(9*time.Hour), 4, nil)
	rollupOneDay(t, ctx, repo, day)

	tests := []struct {
		name     string
		interval Interval
		groupBy  GroupBy
		want     TrendSource
		why      string
	}{
		{"daily by namespace", IntervalDay, GroupByNamespace, TrendSourceRollup,
			"the common case, and the whole point: ~293x less data"},
		{"weekly by team", IntervalWeek, GroupByTeam, TrendSourceRollup,
			"a week is a whole number of days, so days aggregate into it exactly"},
		{"monthly by namespace", IntervalMonth, GroupByNamespace, TrendSourceRollup,
			"same, and the cheapest query of all"},
		{"hourly", IntervalHour, GroupByNamespace, TrendSourceFacts,
			"the rollup's finest grain is a DAY. There is no hourly data in it to read, and returning " +
				"daily buckets for an hourly request would be a wrong answer rather than a slow one"},
		{"by pod", IntervalDay, GroupByPod, TrendSourceFacts,
			"pod is the one groupable dimension the rollup drops, because a pod name is not a stable " +
				"identity across a rollout"},
		{"by container", IntervalDay, GroupByContainer, TrendSourceFacts,
			"container grouping includes pod_name so two namespaces' identically-named containers are " +
				"not merged -- keeping that guarantee matters more than using the rollup"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from := day.Time()
			to := day.AddDays(1).Time()
			_, source, err := repo.Trend(ctx, TrendParams{
				From: from, To: to, Interval: tt.interval, GroupBy: tt.groupBy,
				Filters: Filters{Namespace: f.namespaceName}, Limit: 10,
			})
			if err != nil {
				t.Fatalf("Trend: %v", err)
			}
			if source != tt.want {
				t.Errorf("source = %q, want %q\nwhy: %s", source, tt.want, tt.why)
			}
		})
	}
}

// TestTrend_RollupAndFactsAgree is the test that makes the routing safe.
//
// Routing between two tables is only defensible if they give the same answer for a question both can
// serve. If they disagree, the source field turns from useful provenance into an explanation of why the
// numbers are inconsistent -- and the routing becomes a bug generator rather than an optimisation.
func TestTrend_RollupAndFactsAgree(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	day := isolatedDay
	seedWindows(t, ctx, tx, f, day.Time().Add(9*time.Hour), 6, func(a *domain.ContainerAllocation, i int) {
		a.CPUMillicoresRequested = int64(100 * (i + 1))
		a.CPUMillicoresUsed = int64(7 * (i + 1))
	})
	rollupOneDay(t, ctx, repo, day)

	params := TrendParams{
		From: day.Time(), To: day.AddDays(1).Time(),
		Interval: IntervalDay, GroupBy: GroupByNamespace,
		Filters: Filters{Namespace: f.namespaceName}, Limit: 10,
	}

	fromRollup, src1, err := repo.Trend(ctx, params)
	if err != nil {
		t.Fatalf("Trend from rollup: %v", err)
	}
	if src1 != TrendSourceRollup {
		t.Fatalf("expected the rollup path, got %s", src1)
	}

	// The same question forced down the fact-table path, by grouping in a way that routes there... which
	// would change the grouping. So the fact query is issued directly instead, with the same predicates.
	var factCost, factCPUHours, factWasted string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(total_cost),0)::text,
		       `+coreHours("cpu_millicores_billable")+`::text,
		       `+wastedCoreHours("cpu_millicores_requested", "cpu_millicores_used")+`::text
		FROM container_allocations
		WHERE namespace_name = $1 AND window_start >= $2 AND window_start < $3`,
		f.namespaceName, params.From, params.To).Scan(&factCost, &factCPUHours, &factWasted); err != nil {
		t.Fatalf("fact query: %v", err)
	}

	if len(fromRollup) != 1 {
		t.Fatalf("got %d series, want 1", len(fromRollup))
	}
	pts := fromRollup[0].Points
	if len(pts) != 1 {
		t.Fatalf("got %d points for a single day, want 1", len(pts))
	}

	checks := []struct {
		name       string
		fromFacts  string
		fromRollup string
	}{
		{"total_cost", factCost, pts[0].TotalCost.String()},
		{"cpu_billable_core_hours", factCPUHours, pts[0].CPUCoreHours.String()},
		{"wasted_cpu_core_hours", factWasted, pts[0].WastedCPUCoreHours.String()},
	}
	for _, c := range checks {
		if !mustDec(t, c.fromFacts).Equal(mustDec(t, c.fromRollup)) {
			t.Errorf("%s: facts=%s rollup=%s.\n"+
				"The two sources must agree for any question both can answer, or the routing silently "+
				"changes the answer depending on which parameters were used", c.name, c.fromFacts, c.fromRollup)
		}
	}
}

// TestTrend_PointsAreOrderedWithinASeries pins an ordering that Go's map iteration would otherwise
// randomise.
//
// The query orders by group then bucket precisely so scanTrend can assemble series in one pass. Without
// it, points would have to be grouped in Go and their order would depend on map iteration -- which Go
// deliberately randomises, so a chart would draw its x-axis differently on every request.
func TestTrend_PointsAreOrderedWithinASeries(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	// Four days, rolled up in a deliberately jumbled order.
	for _, d := range []Date{isolatedDay.AddDays(2), isolatedDay, isolatedDay.AddDays(3), isolatedDay.AddDays(1)} {
		seedWindows(t, ctx, tx, f, d.Time().Add(9*time.Hour), 2, nil)
		rollupOneDay(t, ctx, repo, d)
	}

	series, _, err := repo.Trend(ctx, TrendParams{
		From: isolatedDay.Time(), To: isolatedDay.AddDays(4).Time(),
		Interval: IntervalDay, GroupBy: GroupByNamespace,
		Filters: Filters{Namespace: f.namespaceName}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Trend: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1", len(series))
	}
	pts := series[0].Points
	if len(pts) != 4 {
		t.Fatalf("got %d points, want 4", len(pts))
	}
	for i := 1; i < len(pts); i++ {
		if !pts[i].BucketStart.After(pts[i-1].BucketStart) {
			t.Errorf("point %d (%s) does not follow point %d (%s): buckets must be ascending or a "+
				"chart draws its x-axis out of order",
				i, pts[i].BucketStart, i-1, pts[i-1].BucketStart)
		}
	}

	// And the series total must equal the sum of its own points, so a legend cannot disagree with the
	// line it labels.
	sum := pts[0].TotalCost
	for _, p := range pts[1:] {
		sum = sum.Add(p.TotalCost)
	}
	if !series[0].TotalCost.Equal(sum) {
		t.Errorf("series total %s does not equal the sum of its points %s",
			series[0].TotalCost, sum)
	}
}

// TestTrend_LimitCutsSeriesNotPoints checks where the cap is applied.
//
// A LIMIT in the SQL would truncate mid-series, producing a chart that simply stops partway through the
// range with nothing to say why. Capping at the series boundary means every returned line is complete.
func TestTrend_LimitCutsSeriesNotPoints(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	// Three workloads over three days: 3 series of 3 points each.
	for i, wl := range []string{"alpha", "beta", "gamma"} {
		for d := range 3 {
			day := isolatedDay.AddDays(d)
			seedWindows(t, ctx, tx, f, day.Time().Add(time.Duration(9+i)*time.Hour), 1,
				func(a *domain.ContainerAllocation, _ int) { a.WorkloadName = wl })
		}
	}
	for d := range 3 {
		rollupOneDay(t, ctx, repo, isolatedDay.AddDays(d))
	}

	series, _, err := repo.Trend(ctx, TrendParams{
		From: isolatedDay.Time(), To: isolatedDay.AddDays(3).Time(),
		Interval: IntervalDay, GroupBy: GroupByWorkload,
		Filters: Filters{Namespace: f.namespaceName}, Limit: 2,
	})
	if err != nil {
		t.Fatalf("Trend: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("got %d series, want 2 (the limit)", len(series))
	}
	// Each returned series must be WHOLE. A SQL LIMIT of 2 would have returned two POINTS.
	for i, s := range series {
		if len(s.Points) != 3 {
			t.Errorf("series %d has %d points, want all 3: the limit must cut series, not truncate "+
				"one mid-range", i, len(s.Points))
		}
	}
}

// TestTrend_RejectsUnknownIntervalAndGrouping is the injection control for the two identifiers
// interpolated into the trend SQL.
//
// date_trunc's unit is a string literal and the grouping is a column list, and neither can be a bound
// parameter. So both go through allow-lists, and this test tries to get past them.
func TestTrend_RejectsUnknownIntervalAndGrouping(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	base := TrendParams{
		From: isolatedDay.Time(), To: isolatedDay.AddDays(4).Time(),
		Interval: IntervalDay, GroupBy: GroupByNamespace,
		Filters: Filters{Namespace: f.namespaceName}, Limit: 10,
	}

	t.Run("interval", func(t *testing.T) {
		p := base
		p.Interval = Interval("day'); DROP TABLE container_allocations_daily; --")
		if _, _, err := repo.Trend(ctx, p); err == nil {
			t.Error("an unknown interval was accepted; it must be refused before reaching the SQL")
		}
	})
	t.Run("grouping", func(t *testing.T) {
		p := base
		p.GroupBy = GroupBy("namespace_name; DELETE FROM monthly_reports")
		if _, _, err := repo.Trend(ctx, p); err == nil {
			t.Error("an unknown grouping was accepted")
		}
	})

	// Both tables still exist, which is why this runs against a real database rather than asserting on
	// a SQL string.
	for _, table := range []string{"container_allocations_daily", "monthly_reports"} {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE false`).Scan(&n); err != nil {
			t.Errorf("%s is not queryable after the rejected input: %v", table, err)
		}
	}
}

// TestTrend_LimitPicksTheLARGESTGroups is a regression test for a bug that shipped and was found by
// looking at a screenshot rather than by any test.
//
// `limit` was applied in scanTrend, which stops after it has seen N distinct groups. The rows arrive
// ORDER BY the grouping columns, because a series' points have to be contiguous to be assembled --
// so "the first N groups" meant the first N ALPHABETICALLY. The dashboard's headline chart asks for
// `limit: 1` under the title "largest namespace" and got whichever namespace sorted first.
//
// It hid for as long as it did because the alphabetically first namespace on this cluster,
// kube-system, was also genuinely near the top. Deploying the app into its own `kca` namespace
// introduced a name sorting earlier still, and the chart started plotting 0.09 while labelling it the
// largest, beside a namespace costing 1.01.
//
// The lesson is in what the existing trend tests DID assert: source routing, point ordering within a
// series, and rollup-versus-facts agreement. All of them checked the SHAPE of the result. None
// checked WHICH groups came back, so the selection could be arbitrary and every test stayed green.
func TestTrend_LimitPicksTheLARGESTGroups(t *testing.T) {
	ctx, tx := withTx(t)
	repo := NewRollupRepository(tx)

	day := isolatedDay
	cluster := "limit-test-" + t.Name()

	// Names chosen so alphabetical order and cost order are OPPOSITE. If they agreed, the broken
	// implementation would pass and the test would prove nothing -- which is exactly how the real
	// bug survived.
	rows := []struct {
		namespace string
		cost      string
	}{
		{"aaa-cheapest", "0.01"},
		{"mmm-middle", "1.00"},
		{"zzz-dearest", "9.99"},
	}
	for _, r := range rows {
		_, err := tx.Exec(ctx, `
			INSERT INTO container_allocations_daily (
				day, cluster_name, namespace_name, container_name,
				window_count, observed_seconds,
				cpu_requested_core_hours, cpu_used_core_hours, cpu_billable_core_hours,
				wasted_cpu_core_hours,
				memory_requested_gib_hours, memory_used_gib_hours, memory_billable_gib_hours,
				wasted_memory_gib_hours,
				cpu_cost, memory_cost
			) VALUES ($1::date, $2, $3, 'app',
				1, 300,
				1, 1, 1, 0,
				1, 1, 1, 0,
				$4::numeric, 0)`, // total_cost is a GENERATED column: cpu_cost + memory_cost
			day.Time(), cluster, r.namespace, r.cost)
		if err != nil {
			t.Fatalf("seeding %s: %v", r.namespace, err)
		}
	}

	get := func(limit int) []string {
		series, _, err := repo.Trend(ctx, TrendParams{
			From:     day.Time(),
			To:       day.AddDays(1).Time(),
			Interval: IntervalDay,
			GroupBy:  GroupByNamespace,
			Filters:  Filters{Cluster: cluster},
			Limit:    limit,
		})
		if err != nil {
			t.Fatalf("Trend(limit=%d): %v", limit, err)
		}
		out := make([]string, 0, len(series))
		for _, s := range series {
			out = append(out, s.Group["namespace_name"])
		}
		return out
	}

	got := get(1)
	if len(got) != 1 || got[0] != "zzz-dearest" {
		t.Errorf("limit=1 returned %v, want [zzz-dearest]; the most EXPENSIVE namespace, not the "+
			"alphabetically first", got)
	}

	// Two groups must be the two dearest, and the cheapest must not appear at all.
	two := get(2)
	if len(two) != 2 {
		t.Fatalf("limit=2 returned %d series, want 2: %v", len(two), two)
	}
	for _, name := range two {
		if name == "aaa-cheapest" {
			t.Errorf("limit=2 included the cheapest namespace: %v", two)
		}
	}

	// A limit above the group count returns everything rather than erroring or truncating.
	if all := get(10); len(all) != 3 {
		t.Errorf("limit=10 returned %d series, want all 3: %v", len(all), all)
	}
}
