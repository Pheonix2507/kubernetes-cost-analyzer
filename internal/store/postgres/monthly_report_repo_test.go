package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

// The isolated month, for the same reason rollup_repo_test.go uses isolated days: GenerateMonth reads
// every rollup row in the month, unfiltered, so the fixture must live where the collector never wrote.
var isolatedMonth = NewMonth(2027, 6)

// seedRolledUpMonth writes fact rows across several days of the isolated month and rolls them up, which
// is the state GenerateMonth expects to find.
func seedRolledUpMonth(t *testing.T, ctx context.Context, tx Querier, f fixture, dayCount int) {
	t.Helper()
	repo := NewRollupRepository(tx)
	for i := range dayCount {
		day := DayOf(isolatedMonth.Time()).AddDays(i)
		seedWindows(t, ctx, tx, f, day.Time().Add(9*time.Hour), 2, nil)
		if _, err := repo.RollupDay(ctx, day); err != nil {
			t.Fatalf("roll up %s: %v", day, err)
		}
	}
}

// reportsByScope indexes generated statements for assertion.
func reportsByScope(t *testing.T, ctx context.Context, repo *RollupRepository, m Month) map[string]MonthlyReport {
	t.Helper()
	items, err := repo.MonthlyReports(ctx, MonthlyReportParams{From: m, To: m, Limit: 100})
	if err != nil {
		t.Fatalf("MonthlyReports: %v", err)
	}
	out := map[string]MonthlyReport{}
	for _, r := range items {
		out[r.ScopeKind+"/"+r.ScopeValue] = r
	}
	return out
}

// =============================================================================
// Generation
// =============================================================================

// TestGenerateMonth_WritesAllThreeScopesInOnePass covers the GROUPING SETS decision.
//
// Three separate queries would scan the month three times and, worse, could see different data between
// them -- so a cluster statement might not equal the sum of its namespace statements, for no reason a
// reader could ever discover. One pass makes that inconsistency impossible rather than unlikely.
//
// The reconciliation is asserted directly below, because "one pass" is only valuable if it produces
// figures that add up.
func TestGenerateMonth_WritesAllThreeScopesInOnePass(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	seedRolledUpMonth(t, ctx, tx, f, 3)

	res, err := repo.GenerateMonth(ctx, isolatedMonth)
	if err != nil {
		t.Fatalf("GenerateMonth: %v", err)
	}
	if res.Written == 0 {
		t.Fatal("no statements were written")
	}

	byScope := reportsByScope(t, ctx, repo, isolatedMonth)

	// The fixture's namespace has team "payments", so all three scopes must appear.
	cluster, hasCluster := byScope[ScopeCluster+"/"+f.clusterName]
	namespace, hasNS := byScope[ScopeNamespace+"/"+f.namespaceName]
	team, hasTeam := byScope[ScopeTeam+"/payments"]
	if !hasCluster || !hasNS || !hasTeam {
		t.Fatalf("missing scopes: cluster=%v namespace=%v team=%v (got %v)",
			hasCluster, hasNS, hasTeam, mapKeys(byScope))
	}

	// THE RECONCILIATION. This fixture is the only thing in its cluster, so all three scopes must carry
	// the identical total. If they differ, the single pass is grouping something inconsistently and the
	// statements would not add up.
	if !cluster.TotalCost.Equal(namespace.TotalCost) || !namespace.TotalCost.Equal(team.TotalCost) {
		t.Errorf("scopes disagree: cluster=%s namespace=%s team=%s.\n"+
			"For a cluster containing one namespace owned by one team, all three are the same money "+
			"counted at three levels", cluster.TotalCost, namespace.TotalCost, team.TotalCost)
	}
}

// TestGenerateMonth_UnlabelledCostGetsNoTeamStatement pins the HAVING clause, which looks like a filter
// and is actually a policy.
//
// A container with no team label produces no team-scoped statement. Inventing one called "" would create
// a bill nobody owns while making the cluster total look fully attributed -- so the cost stays in the
// cluster statement and simply has no team line.
//
// The consequence is the useful part: the gap between the cluster total and the sum of team statements
// IS the unattributed spend, which is a number worth being able to see. On the live cluster it is
// half the bill, because kube-system and monitoring carry no team label.
func TestGenerateMonth_UnlabelledCostGetsNoTeamStatement(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)
	alloc := NewAllocationRepository(tx)

	day := DayOf(isolatedMonth.Time())

	// One labelled container and one with an EMPTY team, in the same cluster.
	labelled := baseAllocation(f, day.Time().Add(9*time.Hour))
	unlabelled := baseAllocation(f, day.Time().Add(10*time.Hour))
	unlabelled.NamespaceName = f.namespaceName + "-unowned"
	unlabelled.Team = ""
	unlabelled.ContainerName = "orphan"

	for _, a := range []domain.ContainerAllocation{labelled, unlabelled} {
		if err := alloc.Insert(ctx, a); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if _, err := repo.RollupDay(ctx, day); err != nil {
		t.Fatalf("RollupDay: %v", err)
	}
	if _, err := repo.GenerateMonth(ctx, isolatedMonth); err != nil {
		t.Fatalf("GenerateMonth: %v", err)
	}

	byScope := reportsByScope(t, ctx, repo, isolatedMonth)

	// No statement for an empty team.
	if _, exists := byScope[ScopeTeam+"/"]; exists {
		t.Error("a team statement was written with an empty scope_value; it would be a bill nobody owns")
	}
	// But the unlabelled namespace DOES get a namespace statement -- a namespace always has a name.
	if _, exists := byScope[ScopeNamespace+"/"+f.namespaceName+"-unowned"]; !exists {
		t.Errorf("the unlabelled namespace has no statement; only the TEAM scope should be skipped")
	}

	cluster := byScope[ScopeCluster+"/"+f.clusterName]
	teamTotal := byScope[ScopeTeam+"/payments"].TotalCost

	// The cluster counts BOTH containers; the team scope counts one. The difference is the
	// unattributed spend, and it must be non-zero here.
	if !cluster.TotalCost.GreaterThan(teamTotal) {
		t.Errorf("cluster total %s is not greater than the team total %s.\n"+
			"The unlabelled container's cost must still appear in the cluster statement -- the gap "+
			"between the two IS the unattributed spend", cluster.TotalCost, teamTotal)
	}
}

// TestGenerateMonth_CoverageReportsIncompleteness is the field that separates a statement from a chart.
//
// A month containing a collector outage produces a total that is confidently too low, and nothing about
// the total itself reveals that. A report handed to anyone who makes decisions must be able to say how
// complete it is.
func TestGenerateMonth_CoverageReportsIncompleteness(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	// Three days of data in a thirty-day month.
	seedRolledUpMonth(t, ctx, tx, f, 3)
	if _, err := repo.GenerateMonth(ctx, isolatedMonth); err != nil {
		t.Fatalf("GenerateMonth: %v", err)
	}

	got := reportsByScope(t, ctx, repo, isolatedMonth)[ScopeCluster+"/"+f.clusterName]

	if got.DaysWithData != 3 {
		t.Errorf("days_with_data = %d, want 3", got.DaysWithData)
	}
	// June has 30 days. Computed by the query rather than passed in, so February and leap years need no
	// special case.
	if got.DaysInMonth != 30 {
		t.Errorf("days_in_month = %d, want 30 for June", got.DaysInMonth)
	}
	// 3/30 = 0.1000, generated by the column so it can never drift from its inputs.
	if !got.Coverage.Equal(mustDec(t, "0.1")) {
		t.Errorf("coverage = %s, want 0.1000: three days of a thirty-day month", got.Coverage)
	}
	// And the window count, which is what makes a PARTIAL day visible -- coverage alone counts a
	// one-hour day as a full one, and that limitation is documented rather than hidden.
	if got.WindowCount != 6 {
		t.Errorf("window_count = %d, want 6 (3 days x 2 windows)", got.WindowCount)
	}
}

// TestGenerateMonth_ComputesDaysInMonthCorrectly checks the calendar arithmetic across the cases that
// break naive implementations.
func TestGenerateMonth_ComputesDaysInMonthCorrectly(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	tests := []struct {
		month Month
		want  int
	}{
		{NewMonth(2027, 2), 28},  // ordinary February
		{NewMonth(2028, 2), 29},  // leap February
		{NewMonth(2027, 4), 30},  // 30-day month
		{NewMonth(2027, 12), 31}, // year end
	}

	for _, tt := range tests {
		t.Run(tt.month.String(), func(t *testing.T) {
			day := DayOf(tt.month.Time())
			seedWindows(t, ctx, tx, f, day.Time().Add(9*time.Hour), 1, nil)
			if _, err := repo.RollupDay(ctx, day); err != nil {
				t.Fatalf("RollupDay: %v", err)
			}
			if _, err := repo.GenerateMonth(ctx, tt.month); err != nil {
				t.Fatalf("GenerateMonth: %v", err)
			}
			got := reportsByScope(t, ctx, repo, tt.month)[ScopeCluster+"/"+f.clusterName]
			if got.DaysInMonth != tt.want {
				t.Errorf("days_in_month for %s = %d, want %d", tt.month, got.DaysInMonth, tt.want)
			}
		})
	}
}

// TestGenerateMonth_IsIdempotent checks a provisional month can be regenerated as often as you like.
//
// Which it must be: the current month should be regenerated after every nightly rollup, so a
// non-idempotent generate would either accumulate duplicates or need a delete step nobody would
// remember to run.
func TestGenerateMonth_IsIdempotent(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	seedRolledUpMonth(t, ctx, tx, f, 2)

	first, err := repo.GenerateMonth(ctx, isolatedMonth)
	if err != nil {
		t.Fatalf("first GenerateMonth: %v", err)
	}
	before := reportsByScope(t, ctx, repo, isolatedMonth)

	second, err := repo.GenerateMonth(ctx, isolatedMonth)
	if err != nil {
		t.Fatalf("second GenerateMonth: %v", err)
	}
	after := reportsByScope(t, ctx, repo, isolatedMonth)

	if first.Written != second.Written {
		t.Errorf("wrote %d then %d statements; the second run should update the same rows",
			first.Written, second.Written)
	}
	if len(before) != len(after) {
		t.Fatalf("statement count changed from %d to %d: the upsert is duplicating rather than updating",
			len(before), len(after))
	}
	for key, b := range before {
		a, exists := after[key]
		if !exists {
			t.Errorf("%s disappeared on regeneration", key)
			continue
		}
		if !a.TotalCost.Equal(b.TotalCost) {
			t.Errorf("%s changed from %s to %s on regeneration", key, b.TotalCost, a.TotalCost)
		}
	}
}

// =============================================================================
// Immutability
// =============================================================================

// TestFinaliseMonth_RefusesAnIncompleteMonth is the guard that prevents the one unrecoverable state.
//
// A statement closed mid-month is missing days AND frozen. Every other mistake here is fixable by
// regenerating; this one needs a deliberate un-finalise, so it is refused at the source rather than left
// to whoever writes the cron entry.
func TestFinaliseMonth_RefusesAnIncompleteMonth(t *testing.T) {
	ctx, tx := withTx(t)
	repo := NewRollupRepository(tx)

	month := NewMonth(2027, 6)
	// "Now" is inside the month.
	now := time.Date(2027, 6, 15, 12, 0, 0, 0, time.UTC)

	if _, err := repo.FinaliseMonth(ctx, month, now); err == nil {
		t.Fatal("finalising a month that has not ended was allowed; the statements would be both " +
			"incomplete and immutable")
	}

	// The first instant of the following month makes it complete -- the same half-open boundary used
	// everywhere else.
	if _, err := repo.FinaliseMonth(ctx, month, month.Next().Time()); err != nil {
		t.Errorf("refused to finalise a month that HAS ended: %v", err)
	}
}

// TestFinaliseMonth_FreezesAgainstEveryWriter is the point of the triggers.
//
// The upsert already declines to touch a finalised row via its WHERE clause, so the normal path is safe.
// This asserts the property holds against writers that do not go through that path at all -- a future
// endpoint, a migration, an operator with psql. "A finalised statement is never rewritten" is a property
// of the DATA, and an invariant enforced only by the one function that currently writes the table lasts
// exactly until the second writer appears.
func TestFinaliseMonth_FreezesAgainstEveryWriter(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	seedRolledUpMonth(t, ctx, tx, f, 2)
	if _, err := repo.GenerateMonth(ctx, isolatedMonth); err != nil {
		t.Fatalf("GenerateMonth: %v", err)
	}

	closed, err := repo.FinaliseMonth(ctx, isolatedMonth, isolatedMonth.Next().Time())
	if err != nil {
		t.Fatalf("FinaliseMonth: %v", err)
	}
	if closed == 0 {
		t.Fatal("nothing was finalised")
	}

	frozen := reportsByScope(t, ctx, repo, isolatedMonth)[ScopeCluster+"/"+f.clusterName]
	if !frozen.Finalised() {
		t.Fatal("the statement is not marked finalised")
	}

	// A DIRECT UPDATE, bypassing the repository entirely. This is the writer the trigger exists for.
	_, err = tx.Exec(ctx, `UPDATE monthly_reports SET cpu_cost = 999 WHERE id = (
		SELECT id FROM monthly_reports WHERE period_month = $1::date AND scope_kind = 'cluster' LIMIT 1)`,
		isolatedMonth.Time())
	if err == nil {
		t.Error("a finalised statement was rewritten by a direct UPDATE. The conditional upsert " +
			"protects only the code path that uses it; the invariant has to live in the database")
	} else if !strings.Contains(err.Error(), "finalised") {
		t.Errorf("the UPDATE failed for the wrong reason: %v", err)
	}

	// A DELETE, too. A statement that can be deleted and regenerated is not immutable, merely
	// inconvenient to change.
	_, err = tx.Exec(ctx,
		`DELETE FROM monthly_reports WHERE period_month = $1::date AND scope_kind = 'cluster'`,
		isolatedMonth.Time())
	if err == nil {
		t.Error("a finalised statement was deleted")
	}
}

// TestGenerateMonth_LeavesFinalisedStatementsAlone is the graceful path.
//
// Regenerating a period that contains signed-off statements must SUCCEED and leave them intact. Failing
// would be worse than useless: it would leave the provisional statements in the same month unwritten
// too, so one frozen row would block the whole month from ever updating again.
func TestGenerateMonth_LeavesFinalisedStatementsAlone(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	seedRolledUpMonth(t, ctx, tx, f, 2)
	if _, err := repo.GenerateMonth(ctx, isolatedMonth); err != nil {
		t.Fatalf("GenerateMonth: %v", err)
	}
	if _, err := repo.FinaliseMonth(ctx, isolatedMonth, isolatedMonth.Next().Time()); err != nil {
		t.Fatalf("FinaliseMonth: %v", err)
	}

	before := reportsByScope(t, ctx, repo, isolatedMonth)[ScopeCluster+"/"+f.clusterName]

	// MORE data arrives and the month is regenerated. The frozen figures must not move.
	extra := DayOf(isolatedMonth.Time()).AddDays(5)
	seedWindows(t, ctx, tx, f, extra.Time().Add(9*time.Hour), 4, nil)
	if _, err := repo.RollupDay(ctx, extra); err != nil {
		t.Fatalf("RollupDay: %v", err)
	}

	res, err := repo.GenerateMonth(ctx, isolatedMonth)
	if err != nil {
		t.Fatalf("regeneration failed instead of skipping the frozen statements: %v", err)
	}
	if res.Written != 0 {
		t.Errorf("wrote %d statements over a fully finalised month, want 0", res.Written)
	}
	if res.SkippedFinalised == 0 {
		t.Error("SkippedFinalised = 0. Reporting it is what distinguishes 'nothing to do' from " +
			"'refused to touch signed-off statements'")
	}

	after := reportsByScope(t, ctx, repo, isolatedMonth)[ScopeCluster+"/"+f.clusterName]
	if !after.TotalCost.Equal(before.TotalCost) {
		t.Errorf("a frozen statement changed from %s to %s after new data arrived. That is the whole "+
			"reason this table exists rather than the figure being computed on demand",
			before.TotalCost, after.TotalCost)
	}
}

// TestFinaliseMonth_IsIdempotent checks a second close is a no-op rather than a rewrite.
//
// Which matters because the trigger would otherwise fire: an UPDATE that set finalised_at on an
// already-finalised row is exactly what the trigger blocks, so the predicate has to exclude them.
func TestFinaliseMonth_IsIdempotent(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	seedRolledUpMonth(t, ctx, tx, f, 1)
	if _, err := repo.GenerateMonth(ctx, isolatedMonth); err != nil {
		t.Fatalf("GenerateMonth: %v", err)
	}

	first, err := repo.FinaliseMonth(ctx, isolatedMonth, isolatedMonth.Next().Time())
	if err != nil {
		t.Fatalf("first FinaliseMonth: %v", err)
	}
	if first == 0 {
		t.Fatal("nothing was finalised on the first call")
	}

	second, err := repo.FinaliseMonth(ctx, isolatedMonth, isolatedMonth.Next().Time())
	if err != nil {
		t.Fatalf("a second finalise errored rather than doing nothing: %v\n"+
			"The WHERE finalised_at IS NULL predicate is what keeps the trigger out of the normal path", err)
	}
	if second != 0 {
		t.Errorf("the second finalise closed %d statements, want 0", second)
	}
}

// =============================================================================
// Reading
// =============================================================================

// TestMonthlyReports_MonthRangeIsInclusiveAtBothEnds pins the one place this codebase departs from
// half-open ranges, so the departure is deliberate rather than an accident somebody later "fixes".
//
// A month is a label, not an instant. "January to March" means three months to every human who asks for
// it, so a half-open month range would make from=2026-01&to=2026-03 return two -- which reads as an
// off-by-one bug in the API rather than as a convention.
func TestMonthlyReports_MonthRangeIsInclusiveAtBothEnds(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewRollupRepository(tx)

	// Three consecutive months, one day of data each.
	months := []Month{NewMonth(2027, 6), NewMonth(2027, 7), NewMonth(2027, 8)}
	for _, m := range months {
		day := DayOf(m.Time())
		seedWindows(t, ctx, tx, f, day.Time().Add(9*time.Hour), 1, nil)
		if _, err := repo.RollupDay(ctx, day); err != nil {
			t.Fatalf("RollupDay: %v", err)
		}
		if _, err := repo.GenerateMonth(ctx, m); err != nil {
			t.Fatalf("GenerateMonth(%s): %v", m, err)
		}
	}

	items, err := repo.MonthlyReports(ctx, MonthlyReportParams{
		From: months[0], To: months[2], ScopeKind: ScopeCluster, ScopeValue: f.clusterName, Limit: 100,
	})
	if err != nil {
		t.Fatalf("MonthlyReports: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d months for a June..August request, want 3. A half-open upper bound would "+
			"return 2 and read as an off-by-one bug", len(items))
	}

	// Newest first: a statement page is read to see the latest month.
	if items[0].Month != "2027-08" {
		t.Errorf("first month = %s, want 2027-08 (newest first)", items[0].Month)
	}
	if items[2].Month != "2027-06" {
		t.Errorf("last month = %s, want 2027-06", items[2].Month)
	}
}

// TestMonthlyReports_RejectsUnknownScopeKind checks the read path validates against the same three
// constants the CHECK constraint uses.
//
// An unknown scope must be an error rather than a query that correctly returns nothing -- an empty
// result for scope_kind=Team looks exactly like "this team has no statements".
func TestMonthlyReports_RejectsUnknownScopeKind(t *testing.T) {
	ctx, tx := withTx(t)
	repo := NewRollupRepository(tx)

	for _, bad := range []string{"Team", "TEAM", "workload", "cluster; DROP TABLE monthly_reports"} {
		if _, err := repo.MonthlyReports(ctx, MonthlyReportParams{
			From: isolatedMonth, To: isolatedMonth, ScopeKind: bad, Limit: 10,
		}); err == nil {
			t.Errorf("scope_kind %q was accepted; an unknown scope returns nothing and looks like "+
				"'no data' rather than 'wrong parameter'", bad)
		}
	}

	// Empty is allowed and means all three, which is what makes the endpoint explorable.
	if _, err := repo.MonthlyReports(ctx, MonthlyReportParams{
		From: isolatedMonth, To: isolatedMonth, Limit: 10,
	}); err != nil {
		t.Errorf("an empty scope_kind was rejected: %v", err)
	}
}

// TestMonthlyReports_EmptyIsAnEmptySlice guards the JSON contract, as everywhere else.
func TestMonthlyReports_EmptyIsAnEmptySlice(t *testing.T) {
	ctx, tx := withTx(t)
	repo := NewRollupRepository(tx)

	items, err := repo.MonthlyReports(ctx, MonthlyReportParams{
		From: NewMonth(2001, 1), To: NewMonth(2001, 1), Limit: 10,
	})
	if err != nil {
		t.Fatalf("MonthlyReports: %v", err)
	}
	if items == nil {
		t.Error("returned nil; want an empty slice so it marshals to [] rather than null")
	}
}

// =============================================================================
// Month arithmetic
// =============================================================================

// TestMonth_ParseAndNormalise covers the type that makes a "month" unambiguous.
func TestMonth_ParseAndNormalise(t *testing.T) {
	t.Parallel()

	t.Run("normalises to the first of the month", func(t *testing.T) {
		t.Parallel()
		// MonthOf must discard the day and time, or the CHECK constraint on period_month would be the
		// thing that catches it -- at runtime, in production, on a write.
		got := MonthOf(time.Date(2026, 8, 27, 23, 59, 59, 0, time.UTC))
		if got.String() != "2026-08" {
			t.Errorf("MonthOf = %s, want 2026-08", got)
		}
		if !got.Time().Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("Time() = %s, want the first of the month at midnight UTC", got.Time())
		}
	})

	t.Run("converts to UTC before extracting the month", func(t *testing.T) {
		t.Parallel()
		// 1 August 2026 at 02:00 IST is 31 JULY at 20:30 UTC. The rollup is defined in UTC, so this
		// instant belongs to July -- and a version that read the local month would file it under August,
		// putting one evening of cost in the wrong statement every month.
		ist := time.FixedZone("IST", 5*3600+1800)
		got := MonthOf(time.Date(2026, 8, 1, 2, 0, 0, 0, ist))
		if got.String() != "2026-07" {
			t.Errorf("MonthOf(1 Aug 02:00 IST) = %s, want 2026-07: that instant is 31 July in UTC", got)
		}
	})

	t.Run("parses YYYY-MM and rejects anything else", func(t *testing.T) {
		t.Parallel()
		if m, err := ParseMonth("2026-08"); err != nil || m.String() != "2026-08" {
			t.Errorf("ParseMonth(2026-08) = %s, %v", m, err)
		}
		for _, bad := range []string{"2026", "08-2026", "2026-13", "2026-08-04", "august", ""} {
			if _, err := ParseMonth(bad); err == nil {
				t.Errorf("ParseMonth(%q) was accepted", bad)
			}
		}
	})

	t.Run("IsComplete uses the half-open boundary", func(t *testing.T) {
		t.Parallel()
		m := NewMonth(2026, 8)
		cases := []struct {
			now  time.Time
			want bool
			why  string
		}{
			{time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC), false, "one second left in the month"},
			{time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), true, "the first instant after it -- complete"},
			{time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), false, "the month has not started"},
		}
		for _, c := range cases {
			if got := m.IsComplete(c.now); got != c.want {
				t.Errorf("IsComplete(%s) = %v, want %v: %s", c.now.Format(time.RFC3339), got, c.want, c.why)
			}
		}
	})
}

// mapKeys lists a map's keys for an error message.
func mapKeys(m map[string]MonthlyReport) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
