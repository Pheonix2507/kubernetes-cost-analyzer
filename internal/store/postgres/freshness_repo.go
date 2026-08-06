package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Freshness is how current the data is, read from the data itself.
//
// WHY THIS EXISTS -- YOU CANNOT SCRAPE A PROCESS THAT HAS EXITED
// ==============================================================
// cmd/rollup is a batch job. It runs, writes, and dies, so by the time Prometheus comes to scrape it
// there is nothing listening on any port. The canonical Prometheus answer is a Pushgateway: an extra
// component whose entire purpose is holding a metric on behalf of a process that is no longer running.
//
// There is a better answer here, and it is better for a reason beyond avoiding a component. THE DATABASE
// ALREADY RECORDS WHAT HAPPENED. `max(rolled_up_at)` is when the rollup last wrote; `max(window_start)`
// is the freshest collected window. So the API reads those and exposes them as gauges, and no extra
// infrastructure exists.
//
// The deeper point is what the metric then MEANS. A Pushgateway reports "the job said it finished". This
// reports "the rows are there". Those come apart in exactly the failure mode a job's own success metric
// cannot see: a job that exits zero having written nothing. A wrong date flag, an empty range, a silently
// swallowed error -- all of them produce a happy Pushgateway and an empty table. Deriving freshness from
// the data cannot be fooled that way.
//
// The cost is one query per scrape. Both are index-only max() lookups on a leading column, so the plan is
// a backwards index scan reading a single page -- see the verification in the phase 9 notes.
type Freshness struct {
	// LastFactWindow is the newest collected window, or zero when the fact table is empty.
	LastFactWindow time.Time
	// LastRollupDay is the most recently rolled-up day, or zero when nothing has been rolled up.
	LastRollupDay time.Time
	// LastRollupWrite is when the rollup job last wrote anything, which differs from LastRollupDay: a
	// backfill run today writes rows for days months ago. Both matter -- one says how current the data is,
	// the other says whether the job is alive.
	LastRollupWrite time.Time

	// The trailing hour's cost and waste, as float64 for the Prometheus gauges.
	//
	// FLOAT64 HERE AND NOWHERE ELSE IN THIS CODEBASE, and the exception is deliberate rather than a
	// lapse. Prometheus stores float64; there is no exact-decimal option. So the choice is between not
	// exposing cost to Prometheus at all, or exposing it as the approximation Prometheus can hold.
	//
	// It is exposed, for one reason: alerting. "This cluster's spend tripled overnight" is worth knowing
	// at 09:00 and Postgres has no alerting engine. A float is fine for a threshold comparison -- the
	// question is "is this roughly 3x", not "is this exactly 4182.1900000001".
	//
	// POSTGRES REMAINS THE SYSTEM OF RECORD. Anything a human quotes, invoices against, or puts in a
	// report comes from the API, which carries the exact decimal all the way to the browser as a string.
	// This is the approximate copy that exists so a machine can shout about a change.
	LastHourCost      float64
	LastHourWastedCPU float64
	LastHourWastedMem float64
}

// FreshnessRepository reads data-currency timestamps.
type FreshnessRepository struct {
	db Querier
}

// NewFreshnessRepository wires the repository to a Querier.
func NewFreshnessRepository(db Querier) *FreshnessRepository {
	return &FreshnessRepository{db: db}
}

// Freshness reads the three timestamps in ONE round trip.
//
// One query rather than three, and not merely to save round trips: three separate queries could see
// three different snapshots, so the rollup could appear NEWER than the facts it was built from. A single
// statement observes one consistent state, which is the property a freshness metric needs to not report
// impossible orderings.
func (r *FreshnessRepository) Freshness(ctx context.Context) (Freshness, error) {
	// Scalar subqueries rather than a join: there is nothing to join on, and each is an independent
	// index-only max(). Postgres evaluates each as a backwards index scan reading one page.
	const q = `
		SELECT
		    (SELECT max(window_start)  FROM container_allocations),
		    (SELECT max(day)           FROM container_allocations_daily),
		    (SELECT max(rolled_up_at)  FROM container_allocations_daily),
		    -- A ROLLING hour: now() - 1 hour to now().
		    --
		    -- The first version used the last COMPLETE clock hour, on the reasoning that a partial hour
		    -- sawtooths -- at 09:05 it holds five minutes of cost, so the gauge would collapse at the top
		    -- of every hour and every threshold alert would fire hourly on a healthy system.
		    --
		    -- That reasoning was right about the wrong window. It applies to a truncated-to-current-hour
		    -- range, which IS partial and does sawtooth. A rolling hour has no such edge: windows leave the
		    -- trailing end at the same rate they enter the leading one, so the value is smooth.
		    --
		    -- (No backticks in this comment, deliberately. The SQL lives in a Go raw string literal, which a
		    -- backtick terminates -- the same mistake that broke a file in phase 6.)
		    --
		    -- And the complete-hour version has a real cost that only showed up on a live system: it lags
		    -- by up to two hours. Verified -- after restarting a collector that had been down, the freshness
		    -- gauge updated within a minute while this gauge sat at zero, because the fresh windows were in
		    -- the CURRENT hour and the previous complete hour was still empty. A cost gauge that reads zero
		    -- for an hour after collection resumes is a cost gauge that cannot distinguish "just recovered"
		    -- from "still broken".
		    --
		    -- COALESCE to 0 rather than NULL: a cluster that genuinely cost nothing is a real state --
		    -- everything scaled to zero -- and it should read as zero rather than failing the scan on a
		    -- NULL. Note this is why cost alerts must be GATED ON FRESHNESS: a stale cluster also reads
		    -- zero, and only the freshness metric can tell the two apart.
		    (SELECT COALESCE(sum(total_cost), 0)::float8 FROM container_allocations
		       WHERE window_start >= now() - INTERVAL '1 hour'),
		    (SELECT COALESCE(sum(
		         GREATEST(cpu_millicores_requested - cpu_millicores_used, 0)::numeric / 1000
		         * EXTRACT(EPOCH FROM (window_end - window_start)) / 3600), 0)::float8
		       FROM container_allocations
		       WHERE window_start >= now() - INTERVAL '1 hour'),
		    (SELECT COALESCE(sum(
		         GREATEST(memory_bytes_requested - memory_bytes_used, 0)::numeric / 1073741824
		         * EXTRACT(EPOCH FROM (window_end - window_start)) / 3600), 0)::float8
		       FROM container_allocations
		       WHERE window_start >= now() - INTERVAL '1 hour')`

	// NULL when a table is empty, so these are nullable destinations. Scanning a NULL into time.Time
	// fails, and an empty database is a completely normal state on a first run -- so it must not be an
	// error.
	var window, day, wrote sql.NullTime
	var cost, wastedCPU, wastedMem float64
	if err := r.db.QueryRow(ctx, q).Scan(&window, &day, &wrote, &cost, &wastedCPU, &wastedMem); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Unreachable: an aggregate over an empty table returns one NULL row rather than no rows.
			// Handled anyway, because relying on that is relying on a detail of aggregate semantics.
			return Freshness{}, nil
		}
		return Freshness{}, fmt.Errorf("read data freshness: %w", err)
	}

	f := Freshness{}
	if window.Valid {
		f.LastFactWindow = window.Time
	}
	if day.Valid {
		f.LastRollupDay = day.Time
	}
	if wrote.Valid {
		f.LastRollupWrite = wrote.Time
	}
	f.LastHourCost = cost
	f.LastHourWastedCPU = wastedCPU
	f.LastHourWastedMem = wastedMem
	return f, nil
}
