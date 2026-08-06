package postgres

import (
	"testing"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

// TestFreshness_EmptyDatabaseIsNotAnError is the case that would otherwise break a first install.
//
// Every aggregate here is a max() over a possibly-empty table, which returns NULL rather than no rows.
// Scanning NULL into a time.Time fails, so without nullable destinations a brand-new deployment would see
// its freshness gauges error on every tick -- and a fresh install is the most normal state there is.
//
// The 2001 range is used so this test does not depend on the collector's committed rows: reads are not
// isolated by rollback, and this repository deliberately takes no filters, because "how fresh is the data"
// is a question about the whole table.
func TestFreshness_EmptyDatabaseIsNotAnError(t *testing.T) {
	ctx, tx := withTx(t)

	// Delete everything within this transaction, so the query genuinely sees empty tables. Rolled back on
	// cleanup, so the collector's real rows are untouched.
	for _, table := range []string{"container_allocations_daily", "container_allocations"} {
		if _, err := tx.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}

	f, err := NewFreshnessRepository(tx).Freshness(ctx)
	if err != nil {
		t.Fatalf("an empty database produced an error: %v\n"+
			"max() over an empty table returns NULL, and a fresh install is a completely normal state", err)
	}

	// ZERO TIMES, not an error and not a garbage value. The caller distinguishes these from real
	// timestamps and leaves the gauge UNSET -- because 0 as a Unix time is 1970, which would make every
	// freshness alert fire instantly on a new deployment.
	if !f.LastFactWindow.IsZero() {
		t.Errorf("LastFactWindow = %v, want the zero time for an empty table", f.LastFactWindow)
	}
	if !f.LastRollupWrite.IsZero() {
		t.Errorf("LastRollupWrite = %v, want the zero time", f.LastRollupWrite)
	}
	// Cost is genuinely 0 rather than unset: a cluster with no data cost nothing, and COALESCE makes that
	// explicit rather than NULL.
	if f.LastHourCost != 0 {
		t.Errorf("LastHourCost = %v, want 0", f.LastHourCost)
	}
}

// TestFreshness_ReportsTheNewestWindow checks the gauge measures the DATA rather than a process's claim.
//
// This is the whole reason freshness is read from Postgres instead of pushed by the job that wrote it. A
// Pushgateway reports "the job said it finished"; this reports "the rows are there". They come apart in
// exactly the failure a job's own success metric cannot see -- a job that exits zero having written
// nothing, from a wrong date flag or a silently swallowed error.
func TestFreshness_ReportsTheNewestWindow(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)

	// Far future, so the collector's committed rows cannot be newer and the assertion is about our rows.
	newest := time.Date(2031, 5, 20, 14, 0, 0, 0, time.UTC)
	seedWindows(t, ctx, tx, f, newest.Add(-30*time.Minute), 6, nil)

	got, err := NewFreshnessRepository(tx).Freshness(ctx)
	if err != nil {
		t.Fatalf("Freshness: %v", err)
	}

	// The LAST of the six five-minute windows: start + 25 minutes.
	want := newest.Add(-5 * time.Minute)
	if !got.LastFactWindow.Equal(want) {
		t.Errorf("LastFactWindow = %v, want %v (the newest window START, not the earliest)",
			got.LastFactWindow, want)
	}
}

// TestFreshness_HourlyCostUsesARollingWindow pins a correction.
//
// The first version summed the last COMPLETE clock hour, on the reasoning that a partial hour sawtooths.
// That reasoning applies to a truncated-to-now range, not to a rolling one -- and the complete-hour version
// had a real cost that only appeared on a live system: after restarting a collector that had been down, the
// freshness gauge updated within a minute while the cost gauge sat at zero for another hour, because the
// fresh windows were in the CURRENT hour and the previous complete hour was still empty.
//
// A cost gauge that reads zero for an hour after collection resumes cannot distinguish "just recovered"
// from "still broken".
func TestFreshness_HourlyCostUsesARollingWindow(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)

	// Windows within the last few minutes -- so they are inside a rolling hour but would NOT be inside the
	// previous complete clock hour for most of any given hour.
	now := time.Now().UTC()
	seedWindows(t, ctx, tx, f, now.Add(-10*time.Minute), 2, func(a *domain.ContainerAllocation, _ int) {
		a.CPUCost = mustDec(t, "1.0000000000")
		a.MemoryCost = mustDec(t, "0.5000000000")
	})

	got, err := NewFreshnessRepository(tx).Freshness(ctx)
	if err != nil {
		t.Fatalf("Freshness: %v", err)
	}

	// At least our 3.0 (2 windows x 1.5). Greater-than rather than equal, because the collector's own
	// committed rows for the last hour are also counted -- this repository takes no filters by design.
	if got.LastHourCost < 3.0 {
		t.Errorf("LastHourCost = %v, want at least 3.0.\n"+
			"Windows from ten minutes ago are inside a ROLLING hour. A last-complete-clock-hour query "+
			"would return 0 here for most of any given hour, which is the lag that made it wrong",
			got.LastHourCost)
	}
	// And waste is present, floored per row.
	if got.LastHourWastedCPU < 0 {
		t.Errorf("LastHourWastedCPU = %v, want non-negative: waste is floored per row inside the sum",
			got.LastHourWastedCPU)
	}
}

// TestFreshness_IsOneRoundTrip pins the single-statement design.
//
// Three separate queries could observe three different snapshots, so the rollup could appear NEWER than the
// facts it was built from -- an impossible ordering that a reader would waste time trying to explain. One
// statement sees one consistent state.
//
// Asserted structurally rather than by counting queries: the method takes a Querier, so a test cannot see
// how many statements it issued without a proxy. What CAN be asserted is the property that matters -- the
// rollup is never reported as newer than the facts.
func TestFreshness_IsOneRoundTrip(t *testing.T) {
	ctx, tx := withTx(t)

	got, err := NewFreshnessRepository(tx).Freshness(ctx)
	if err != nil {
		t.Fatalf("Freshness: %v", err)
	}

	// The rollup is derived FROM the facts, so its newest day can never exceed the newest fact window.
	// (Both may be zero on an empty database, which satisfies this too.)
	if !got.LastRollupDay.IsZero() && !got.LastFactWindow.IsZero() {
		// Compared at DAY granularity: LastRollupDay is a date at midnight while LastFactWindow is an
		// instant within a day, so a same-day comparison of the raw values would fail spuriously.
		if got.LastRollupDay.After(got.LastFactWindow.Truncate(24 * time.Hour).Add(24 * time.Hour)) {
			t.Errorf("rollup day %v is newer than the newest fact window %v, which is impossible: "+
				"the rollup is a projection of the facts",
				got.LastRollupDay, got.LastFactWindow)
		}
	}
}
