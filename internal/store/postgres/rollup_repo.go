package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// RollupRepository writes the daily rollup and the monthly statements, and reads trends from them.
type RollupRepository struct {
	db Querier
}

// NewRollupRepository wires the repository to a Querier, so it works against a pool or a transaction.
func NewRollupRepository(db Querier) *RollupRepository {
	return &RollupRepository{db: db}
}

// DayResult reports what one day's rollup produced.
type DayResult struct {
	Day Date
	// FactRows is how many fact rows were read, RollupRows how many were written. The ratio is the
	// compression, and it is returned rather than logged so a backfill can report it in aggregate.
	FactRows   int64
	RollupRows int64
	// Deleted is how many rows the rewrite removed. Non-zero means this day had been rolled up
	// before, so this run was a re-run rather than a first pass -- worth surfacing because a
	// scheduled job silently re-doing work is usually a scheduling bug.
	Deleted int64
}

// Date is a calendar day with no time and no zone, which is what the rollup is keyed on.
//
// A named type rather than time.Time, because the two are not interchangeable and mixing them is how
// a timezone bug gets in: time.Time carries a location, so passing one where a day is meant makes the
// bucket depend on where the process happens to run. Constructing this type is the moment the
// conversion to UTC is forced to be explicit.
//
// EXPORTED, and the reason is worth recording: it started unexported, which compiles fine as long as
// every caller uses type inference. It falls apart at the first INTERFACE -- internal/rollup cannot
// write a method signature naming a type it is not allowed to reference, so an unexported type here
// silently dictates that no other package may abstract over this repository. A type that appears in a
// signature is part of the API whether or not its name is capitalised.
type Date struct {
	y int
	m time.Month
	d int
}

// Day builds a date. The caller must already have decided which timezone the instant belongs to.
func Day(y int, m time.Month, d int) Date { return Date{y, m, d} }

// DayOf truncates an instant to its UTC calendar day.
//
// .UTC() before extracting the parts, always. Without it a 01:00 IST timestamp yields the wrong day,
// because 01:00 on the 5th in IST is 19:30 on the 4th in UTC -- and the rollup is defined in UTC.
func DayOf(t time.Time) Date {
	u := t.UTC()
	return Date{u.Year(), u.Month(), u.Day()}
}

// Time renders the date as midnight UTC, for passing to SQL.
func (d Date) Time() time.Time { return time.Date(d.y, d.m, d.d, 0, 0, 0, 0, time.UTC) }

// String renders ISO-8601, which is what the API and the CLI both speak.
func (d Date) String() string { return d.Time().Format("2006-01-02") }

// After reports whether d is later than other.
func (d Date) After(other Date) bool { return d.Time().After(other.Time()) }

// Before reports whether d is earlier than other.
func (d Date) Before(other Date) bool { return d.Time().Before(other.Time()) }

// No Equal method, deliberately -- an audit removed one written here with no caller.
//
// The argument for it was that == on the struct works only by coincidence of its layout. True, and
// still not a reason to ship a method nothing calls: an unused method is untested by construction, and
// the first caller would be the one to discover whether it does what the doc comment claims. Add it
// with its first real use, and a test.

// AddDays returns the date n days later.
func (d Date) AddDays(n int) Date { return DayOf(d.Time().AddDate(0, 0, n)) }

// =============================================================================
// The rollup write
// =============================================================================

// rollupSelect is the aggregation. It is shared by the daily write so there is exactly one definition
// of "what a day's cost is", and no second query that could disagree with it.
//
// EVERY MEASURE IS A SUM OR A MAX, WHICH IS NOT A COINCIDENCE
// ----------------------------------------------------------
// Those are the only aggregates that re-aggregate correctly. See the migration header: an average
// stored as an average cannot be re-averaged, and a percentile cannot be rolled up at all. Storing
// core-hours -- a sum -- means any average is recoverable by dividing by window_count at read time,
// at whatever grain the reader asked for.
//
// WASTE IS FLOORED PER ROW, INSIDE THE SUM.
// sum(GREATEST(requested - used, 0)), never GREATEST(sum(requested) - sum(used), 0). Phase 6 shipped
// the second form and it reported kube-system as having ZERO memory waste while it held 50 GiB-hours,
// because an under-requested container issued a credit against real waste in the same group. The
// rollup would bake that error into stored history, where it is far harder to notice than in a live
// query.
const rollupSelect = `
	SELECT
	    ($1::date)                          AS day,
	    cluster_name, namespace_name, team, cost_centre, environment,
	    workload_kind, workload_name, node_name, instance_type, capacity_type, container_name,

	    count(*)                            AS window_count,
	    -- The real measured wall-clock time, not count x assumed interval. The interval is
	    -- configurable, so deriving it would silently lie for any period when it differed.
	    sum(EXTRACT(EPOCH FROM (window_end - window_start)))::bigint AS observed_seconds,

	    sum(cpu_millicores_requested::numeric / 1000 * EXTRACT(EPOCH FROM (window_end - window_start)) / 3600) AS cpu_requested_core_hours,
	    sum(cpu_millicores_used::numeric      / 1000 * EXTRACT(EPOCH FROM (window_end - window_start)) / 3600) AS cpu_used_core_hours,
	    sum(cpu_millicores_billable::numeric  / 1000 * EXTRACT(EPOCH FROM (window_end - window_start)) / 3600) AS cpu_billable_core_hours,
	    sum(GREATEST(cpu_millicores_requested - cpu_millicores_used, 0)::numeric / 1000
	        * EXTRACT(EPOCH FROM (window_end - window_start)) / 3600)                                          AS wasted_cpu_core_hours,

	    sum(memory_bytes_requested::numeric / 1073741824 * EXTRACT(EPOCH FROM (window_end - window_start)) / 3600) AS memory_requested_gib_hours,
	    sum(memory_bytes_used::numeric      / 1073741824 * EXTRACT(EPOCH FROM (window_end - window_start)) / 3600) AS memory_used_gib_hours,
	    sum(memory_bytes_billable::numeric  / 1073741824 * EXTRACT(EPOCH FROM (window_end - window_start)) / 3600) AS memory_billable_gib_hours,
	    sum(GREATEST(memory_bytes_requested - memory_bytes_used, 0)::numeric / 1073741824
	        * EXTRACT(EPOCH FROM (window_end - window_start)) / 3600)                                              AS wasted_memory_gib_hours,

	    sum(cpu_cost)                       AS cpu_cost,
	    sum(memory_cost)                    AS memory_cost,

	    -- Maxima roll up. These are what let a long-range view still show a peak, and they are the
	    -- ONLY usage statistic that survives aggregation intact.
	    max(cpu_millicores_max)             AS cpu_millicores_max,
	    max(memory_bytes_max)               AS memory_bytes_max,

	    bool_or(rate_source = 'fallback')   AS estimated_rates
	FROM container_allocations
	-- A HALF-OPEN DAY: [day 00:00, day+1 00:00). The same interval discipline as every other query
	-- here. A closed range would put the midnight window in both adjacent days, and since the rollup
	-- is then summed into months, that error compounds rather than cancels.
	WHERE window_start >= $1::date
	  AND window_start <  ($1::date + INTERVAL '1 day')
	GROUP BY cluster_name, namespace_name, team, cost_centre, environment,
	         workload_kind, workload_name, node_name, instance_type, capacity_type, container_name`

// RollupDay recomputes one UTC day of the daily rollup from the fact table.
//
// WHY DELETE-THEN-INSERT RATHER THAN ON CONFLICT DO UPDATE
// -------------------------------------------------------
// Upsert is what the fact table uses, so consistency would argue for it here. It is wrong for a
// rollup, and the reason is the difference between a row and a PROJECTION.
//
// An upsert can only correct rows it is given. If a dimension tuple that existed on Monday no longer
// appears in Monday's fact rows -- a mislabelled namespace was corrected, a duplicated pod was
// cleaned up, a rollup bug produced a row from a bad join -- then re-running the rollup writes the
// new rows and LEAVES THE OLD ONE. The stale row is now permanent: nothing will ever reference its
// dimension tuple again, so nothing will ever overwrite it, and it silently inflates every total that
// sums over that day forever.
//
// DELETE-then-INSERT makes this operation "make the rollup equal the fact table for this day". The
// output is a function of the input alone, so a re-run is genuinely idempotent rather than
// approximately so, and a fixed bug actually removes the rows it produced.
//
// It also needs no unique index on the twelve dimension columns -- an index that would plausibly
// exceed the table it indexes.
//
// THE TWO STATEMENTS MUST SHARE ONE TRANSACTION. Between the DELETE and the INSERT the day does not
// exist, and a reader in that gap would see zero cost for it. Wrapping both makes the swap atomic:
// readers see the old rows or the new ones, never neither. This is why the method takes a Querier and
// the caller supplies the transaction -- the atomicity requirement belongs to the operation, and
// hiding a transaction inside a repository method makes it impossible to compose with anything else.
func (r *RollupRepository) RollupDay(ctx context.Context, day Date) (DayResult, error) {
	res := DayResult{Day: day}
	d := day.Time()

	// Scoped to the day only, not to a cluster. Every fact row for the day is reprojected together,
	// which is what makes the result a function of the input. Scoping by cluster would be necessary
	// only if different clusters were rolled up by different processes -- and then the advisory lock
	// would need to be per cluster too, so this is a deliberate pair of decisions rather than one.
	tag, err := r.db.Exec(ctx, `DELETE FROM container_allocations_daily WHERE day = $1::date`, d)
	if err != nil {
		return res, fmt.Errorf("clear rollup for %s: %w", day, err)
	}
	res.Deleted = tag.RowsAffected()

	// INSERT ... SELECT: the aggregation happens INSIDE Postgres and no row crosses the wire.
	//
	// The alternative -- SELECT the fact rows into Go, aggregate them, INSERT the results -- is worth
	// naming because it is the instinctive shape and it is badly wrong at this grain. It would move
	// 1.44 M rows per day over the network to produce 4,900, spend the memory to hold them, and
	// reimplement Postgres's aggregation in Go where it can drift from the SQL the summary endpoint
	// uses. Cost figures computed two ways eventually disagree, and then nobody knows which is right.
	insert := `
		INSERT INTO container_allocations_daily (
		    day, cluster_name, namespace_name, team, cost_centre, environment,
		    workload_kind, workload_name, node_name, instance_type, capacity_type, container_name,
		    window_count, observed_seconds,
		    cpu_requested_core_hours, cpu_used_core_hours, cpu_billable_core_hours, wasted_cpu_core_hours,
		    memory_requested_gib_hours, memory_used_gib_hours, memory_billable_gib_hours, wasted_memory_gib_hours,
		    cpu_cost, memory_cost, cpu_millicores_max, memory_bytes_max, estimated_rates
		) ` + rollupSelect

	tag, err = r.db.Exec(ctx, insert, d)
	if err != nil {
		return res, fmt.Errorf("write rollup for %s: %w", day, err)
	}
	res.RollupRows = tag.RowsAffected()

	// The denominator for the compression ratio. A separate count rather than a window function in
	// the insert, because it is diagnostic rather than part of the data -- and it reads the same
	// half-open range, so the two figures describe the same rows.
	if err := r.db.QueryRow(ctx, `
		SELECT count(*) FROM container_allocations
		WHERE window_start >= $1::date AND window_start < ($1::date + INTERVAL '1 day')`,
		d).Scan(&res.FactRows); err != nil {
		return res, fmt.Errorf("count fact rows for %s: %w", day, err)
	}

	return res, nil
}

// DaysWithFacts lists the UTC days that have fact rows, oldest first.
//
// This is what makes backfill self-describing: the caller does not have to know when collection
// started, and a gap in the middle is visible rather than silently rolled up as a zero-cost day.
func (r *RollupRepository) DaysWithFacts(ctx context.Context, from, to Date) ([]Date, error) {
	rows, err := r.db.Query(ctx, `
		SELECT DISTINCT (window_start AT TIME ZONE 'UTC')::date AS day
		FROM container_allocations
		WHERE window_start >= $1::date AND window_start < ($2::date + INTERVAL '1 day')
		ORDER BY day`, from.Time(), to.Time())
	if err != nil {
		return nil, fmt.Errorf("list days with facts: %w", err)
	}
	defer rows.Close()

	out := []Date{}
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan day: %w", err)
		}
		out = append(out, DayOf(t))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate days: %w", err)
	}
	return out, nil
}

// =============================================================================
// Trends
// =============================================================================

// Interval is a trend bucket width.
type Interval string

// The supported bucket widths.
const (
	// IntervalHour reads the FACT table. The rollup's finest grain is a day, so an hourly series
	// cannot come from it -- see TrendSource.
	IntervalHour  Interval = "hour"
	IntervalDay   Interval = "day"
	IntervalWeek  Interval = "week"
	IntervalMonth Interval = "month"
)

// intervalTrunc maps an interval to its date_trunc unit. An allow-list, for the same reason
// groupByColumns is one: the unit is interpolated into SQL and a placeholder cannot bind an
// identifier or a keyword.
var intervalTrunc = map[Interval]string{
	IntervalHour:  "hour",
	IntervalDay:   "day",
	IntervalWeek:  "week",
	IntervalMonth: "month",
}

// ValidInterval reports whether an interval is supported.
func ValidInterval(i Interval) bool { _, ok := intervalTrunc[i]; return ok }

// IntervalOptions lists the supported intervals, coarsest last. Used by the API's error message and
// by the OpenAPI drift test, so the documented set cannot diverge from the accepted one.
func IntervalOptions() []string {
	return []string{string(IntervalHour), string(IntervalDay), string(IntervalWeek), string(IntervalMonth)}
}

// TrendSource names which table answered a trend query.
//
// RETURNED TO THE CALLER ON PURPOSE, rather than kept as an implementation detail.
//
// The two sources do not answer identically: the rollup has no pod grain and no percentiles, and it
// only covers days the rollup job has processed. A caller comparing two charts needs to know whether
// they came from the same place, and an operator debugging "the trend disagrees with the summary"
// needs it as the first question rather than the last. A silent fallback between sources is how a
// system becomes unexplainable.
type TrendSource string

// The sources a trend can be served from.
const (
	TrendSourceRollup TrendSource = "daily_rollup"
	TrendSourceFacts  TrendSource = "raw_facts"
)

// TrendParams describes a trend query.
type TrendParams struct {
	From     time.Time
	To       time.Time
	Interval Interval
	GroupBy  GroupBy
	Filters  Filters
	// Limit caps the number of GROUPS, not the number of points. Capping points would truncate a
	// series in the middle of a chart, which is worse than refusing it.
	Limit int
}

// TrendPoint is one bucket of one series.
type TrendPoint struct {
	// BucketStart is the start of the bucket, half-open to the next one.
	BucketStart time.Time       `json:"bucket_start"`
	TotalCost   decimal.Decimal `json:"total_cost"`
	CPUCost     decimal.Decimal `json:"cpu_cost"`
	MemoryCost  decimal.Decimal `json:"memory_cost"`

	CPUCoreHours decimal.Decimal `json:"cpu_billable_core_hours"`
	MemGiBHours  decimal.Decimal `json:"memory_billable_gib_hours"`

	WastedCPUCoreHours decimal.Decimal `json:"wasted_cpu_core_hours"`
	WastedMemGiBHours  decimal.Decimal `json:"wasted_memory_gib_hours"`

	// Windows is how many samples the bucket aggregated. Surfaced because a bucket with a tenth of
	// its neighbours' window count is a collection gap, not a cost saving -- and on a chart those two
	// look identical.
	Windows int64 `json:"window_count"`

	EstimatedRates bool `json:"estimated_rates"`
}

// TrendSeries is one group's series through time.
type TrendSeries struct {
	// Group names the dimension values, keyed by dimension. A map here rather than the eleven
	// explicit fields CostSummaryRow uses, because a series is identified by its group rather than
	// described by it -- and a chart legend wants a label, not a struct with ten empty fields.
	Group  map[string]string `json:"group"`
	Points []TrendPoint      `json:"points"`

	// TotalCost is the sum over the returned points, so a series can be sorted and labelled without
	// the caller re-adding them -- and so the total provably matches the points beneath it.
	TotalCost decimal.Decimal `json:"total_cost"`
}

// Trend returns one series per group over the requested range.
//
// SOURCE ROUTING, WHICH IS THE POINT OF HAVING A ROLLUP AT ALL
// -----------------------------------------------------------
// A rollup is only useful if something decides when to read it. That decision is here, it is explicit,
// and it is reported back:
//
//	interval = hour        -> facts. The rollup's finest grain is a day; there is nothing to read.
//	group_by = pod         -> facts. Pod is the one dimension the rollup does not keep.
//	otherwise              -> rollup, which is ~293x less data.
//
// The alternative designs are both worse. Always reading facts wastes the rollup. Always reading the
// rollup silently returns wrong answers for the two cases above -- an hourly chart of daily buckets,
// or a pod grouping that quietly aggregates to container. Answering the question you were asked, or
// saying which table you used, is the only honest pair of options.
func (r *RollupRepository) Trend(ctx context.Context, p TrendParams) ([]TrendSeries, TrendSource, error) {
	if !ValidInterval(p.Interval) {
		return nil, "", fmt.Errorf("unsupported interval %q", p.Interval)
	}
	cols, ok := groupByColumns[p.GroupBy]
	if !ok {
		return nil, "", fmt.Errorf("unsupported grouping %q", p.GroupBy)
	}

	source := TrendSourceRollup
	switch {
	case p.Interval == IntervalHour:
		source = TrendSourceFacts
	case p.GroupBy == GroupByPod, p.GroupBy == GroupByContainer:
		// Container grouping alone would be servable from the rollup, but only WITH its workload
		// columns -- and groupByColumns already includes pod_name for that grouping, precisely so two
		// namespaces' identically-named containers are not merged. Routing both to facts keeps that
		// guarantee rather than weakening it for the sake of using the rollup.
		source = TrendSourceFacts
	}

	if source == TrendSourceFacts {
		return r.trendFromFacts(ctx, p, cols)
	}
	return r.trendFromRollup(ctx, p, cols)
}

// trendFromRollup reads pre-aggregated days. The common path.
func (r *RollupRepository) trendFromRollup(ctx context.Context, p TrendParams, cols []string) ([]TrendSeries, TrendSource, error) {
	args := []any{p.From, p.To}
	where := []string{"day >= $1::date", "day < $2::date"}
	for _, f := range p.Filters.values() {
		col, known := filterColumns[f[0]]
		if !known {
			return nil, "", fmt.Errorf("unknown filter %q", f[0])
		}
		args = append(args, f[1])
		where = append(where, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if p.Filters.EstimatedOnly {
		where = append(where, "estimated_rates")
	}

	// date_trunc on a date column returns a timestamp, which is what the bucket start should be.
	// 'day' truncation is a no-op here and kept for uniformity: one query shape for all three
	// rollup-servable intervals is one thing to get right rather than three.
	bucket := fmt.Sprintf("date_trunc('%s', day::timestamptz)", intervalTrunc[p.Interval])

	query := fmt.Sprintf(`
		SELECT %[1]s AS bucket_start, %[2]s,
		       COALESCE(sum(cpu_cost), 0)                  AS cpu_cost,
		       COALESCE(sum(memory_cost), 0)               AS memory_cost,
		       COALESCE(sum(total_cost), 0)                AS total_cost,
		       COALESCE(sum(cpu_billable_core_hours), 0)   AS cpu_core_hours,
		       COALESCE(sum(memory_billable_gib_hours), 0) AS mem_gib_hours,
		       COALESCE(sum(wasted_cpu_core_hours), 0)     AS wasted_cpu,
		       COALESCE(sum(wasted_memory_gib_hours), 0)   AS wasted_mem,
		       COALESCE(sum(window_count), 0)              AS windows,
		       bool_or(estimated_rates)                    AS estimated_rates
		FROM container_allocations_daily
		WHERE %[3]s
		GROUP BY %[1]s, %[2]s
		ORDER BY %[2]s, %[1]s`,
		bucket, strings.Join(cols, ", "), strings.Join(where, " AND "))

	return r.scanTrend(ctx, query, args, cols, TrendSourceRollup, p.Limit)
}

// trendFromFacts reads the fact table, for the two cases the rollup cannot answer.
//
// Deliberately a separate query rather than a shared one parameterised by table name. The two read
// different columns -- the fact table stores millicores per window and the rollup stores core-hours --
// so the arithmetic genuinely differs, and pretending otherwise with a table-name parameter would hide
// that behind a variable and invite a change to one path that silently skips the other.
func (r *RollupRepository) trendFromFacts(ctx context.Context, p TrendParams, cols []string) ([]TrendSeries, TrendSource, error) {
	args := []any{p.From, p.To}
	where := []string{"window_start >= $1", "window_start < $2"}
	for _, f := range p.Filters.values() {
		col, known := filterColumns[f[0]]
		if !known {
			return nil, "", fmt.Errorf("unknown filter %q", f[0])
		}
		args = append(args, f[1])
		where = append(where, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if p.Filters.EstimatedOnly {
		where = append(where, "rate_source = 'fallback'")
	}

	bucket := fmt.Sprintf("date_trunc('%s', window_start)", intervalTrunc[p.Interval])

	query := fmt.Sprintf(`
		SELECT %[1]s AS bucket_start, %[2]s,
		       COALESCE(sum(cpu_cost), 0)    AS cpu_cost,
		       COALESCE(sum(memory_cost), 0) AS memory_cost,
		       COALESCE(sum(total_cost), 0)  AS total_cost,
		       %[4]s AS cpu_core_hours,
		       %[5]s AS mem_gib_hours,
		       %[6]s AS wasted_cpu,
		       %[7]s AS wasted_mem,
		       count(*)                      AS windows,
		       bool_or(rate_source = 'fallback') AS estimated_rates
		FROM container_allocations
		WHERE %[3]s
		GROUP BY %[1]s, %[2]s
		ORDER BY %[2]s, %[1]s`,
		bucket, strings.Join(cols, ", "), strings.Join(where, " AND "),
		coreHours("cpu_millicores_billable"), gibHours("memory_bytes_billable"),
		wastedCoreHours("cpu_millicores_requested", "cpu_millicores_used"),
		wastedGibHours("memory_bytes_requested", "memory_bytes_used"))

	return r.scanTrend(ctx, query, args, cols, TrendSourceFacts, p.Limit)
}

// scanTrend turns bucket rows into per-group series.
//
// The query returns one row per (bucket, group) ordered by group then bucket, so the rows for one
// series arrive contiguously and this is a single pass with no map of slices to assemble and re-sort.
// That ordering is load-bearing: without it, points would need grouping in Go and their order within
// a series would depend on map iteration -- which is randomised in Go, so a chart would render its
// x-axis in a different order on every request.
func (r *RollupRepository) scanTrend(
	ctx context.Context, query string, args []any, cols []string, source TrendSource, limit int,
) ([]TrendSeries, TrendSource, error) {
	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, source, fmt.Errorf("query trend from %s: %w", source, err)
	}
	defer rows.Close()

	out := []TrendSeries{}
	var currentKey string
	first := true

	for rows.Next() {
		var pt TrendPoint
		groupValues := make([]string, len(cols))

		// Scan destinations built to match the SELECT order: bucket, then one per grouping column,
		// then the measures. Same technique as CostSummary -- the number of dimension columns varies
		// with the grouping, so the destination slice is assembled rather than written out.
		dest := make([]any, 0, len(cols)+10)
		dest = append(dest, &pt.BucketStart)
		for i := range groupValues {
			dest = append(dest, &groupValues[i])
		}
		dest = append(dest,
			&pt.CPUCost, &pt.MemoryCost, &pt.TotalCost,
			&pt.CPUCoreHours, &pt.MemGiBHours,
			&pt.WastedCPUCoreHours, &pt.WastedMemGiBHours,
			&pt.Windows, &pt.EstimatedRates)

		if err := rows.Scan(dest...); err != nil {
			return nil, source, fmt.Errorf("scan trend point: %w", err)
		}

		key := strings.Join(groupValues, "\x00")
		if first || key != currentKey {
			// The group changed, so a new series starts. Enforced at the SERIES boundary rather than
			// in SQL, because a LIMIT in the query would cut the last series mid-way and produce a
			// chart that simply stops partway through the range with no indication why.
			if !first && limit > 0 && len(out) >= limit {
				break
			}
			group := make(map[string]string, len(cols))
			for i, col := range cols {
				group[col] = groupValues[i]
			}
			out = append(out, TrendSeries{Group: group, Points: []TrendPoint{}, TotalCost: decimal.Zero})
			currentKey = key
			first = false
		}

		s := &out[len(out)-1]
		s.Points = append(s.Points, pt)
		s.TotalCost = s.TotalCost.Add(pt.TotalCost)
	}
	if err := rows.Err(); err != nil {
		return nil, source, fmt.Errorf("iterate trend rows: %w", err)
	}
	return out, source, nil
}

// =============================================================================
// Transaction boundaries
// =============================================================================

// TxRollupStore supplies each rollup operation with the transaction its atomicity requires.
//
// WHY THIS EXISTS RATHER THAN A TRANSACTION INSIDE RollupDay
// ---------------------------------------------------------
// RollupDay takes a Querier so it can run against a pool or a transaction, which is what makes it
// testable inside a rolled-back test transaction. But it genuinely REQUIRES atomicity: between its
// DELETE and its INSERT the day has no rows, and a reader in that gap sees the day as free.
//
// Those two facts pull in opposite directions, and this type is where they are reconciled. The
// repository states the requirement and stays composable; this adapter satisfies it for production
// callers. Beginning a transaction inside RollupDay would have made it impossible to call from within
// a larger transaction -- and impossible to test without committing.
//
// It also keeps the wiring out of main: cmd/rollup asks for a store and gets one with the right
// boundaries already decided, rather than reimplementing them next to its flag parsing.
type TxRollupStore struct {
	pool PoolQuerier
}

// PoolQuerier is a connection pool: it can both run statements and begin transactions.
//
// The composed interface exists so TxRollupStore needs no type assertion. An earlier version stored a
// TxBeginner and recovered the Querier with s.pool.(Querier) -- which compiles, and panics at runtime
// for any implementation that begins transactions without also running statements. A fake in a test is
// exactly that implementation. Requiring both up front turns a possible panic into a compile error at
// the call site, which is the only place that can actually fix it.
type PoolQuerier interface {
	Querier
	TxBeginner
}

// NewTxRollupStore wraps a pool.
func NewTxRollupStore(pool PoolQuerier) *TxRollupStore { return &TxRollupStore{pool: pool} }

// RollupDay runs the day's rewrite in a single transaction, so the DELETE and INSERT are one step.
func (s *TxRollupStore) RollupDay(ctx context.Context, day Date) (DayResult, error) {
	var res DayResult
	err := InTx(ctx, s.pool, func(q Querier) error {
		var innerErr error
		res, innerErr = NewRollupRepository(q).RollupDay(ctx, day)
		return innerErr
	})
	return res, err
}

// DaysWithFacts is a single read and needs no transaction of its own.
//
// Explicitly NOT wrapped, rather than wrapped for uniformity. A single statement already runs in an
// implicit transaction, so adding an explicit one buys nothing and costs two extra round trips --
// and uniform-looking code that hides which operations actually need atomicity is worse than code
// where the difference is visible.
func (s *TxRollupStore) DaysWithFacts(ctx context.Context, from, to Date) ([]Date, error) {
	return NewRollupRepository(s.querier()).DaysWithFacts(ctx, from, to)
}

// GenerateMonth is one INSERT ... SELECT, which is atomic on its own.
func (s *TxRollupStore) GenerateMonth(ctx context.Context, m Month) (GenerateResult, error) {
	// Wrapped anyway, because GenerateMonth issues TWO statements: the upsert and the count of
	// finalised rows it skipped. Without a transaction another process could finalise a report
	// between them, and the reported "skipped" figure would describe a state that never existed.
	var res GenerateResult
	err := InTx(ctx, s.pool, func(q Querier) error {
		var innerErr error
		res, innerErr = NewRollupRepository(q).GenerateMonth(ctx, m)
		return innerErr
	})
	return res, err
}

// FinaliseMonth is a single UPDATE.
func (s *TxRollupStore) FinaliseMonth(ctx context.Context, m Month, now time.Time) (int64, error) {
	return NewRollupRepository(s.querier()).FinaliseMonth(ctx, m, now)
}

// querier narrows the pool to a Querier for the operations that need no transaction. No assertion:
// PoolQuerier already guarantees it.
func (s *TxRollupStore) querier() Querier { return s.pool }
