package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Month is the first day of a calendar month in UTC.
type Month struct{ t time.Time }

// NewMonth builds a Month, normalising to the first of the month at midnight UTC.
//
// Normalising in the constructor rather than trusting callers, because the CHECK constraint on
// monthly_reports.period_month enforces the same thing. Two places that must agree is one too many:
// if the constructor is the only way to build one, the constraint can only ever fire on a bug that
// bypassed it.
func NewMonth(y int, m time.Month) Month {
	return Month{time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)}
}

// MonthOf returns the month containing an instant, in UTC.
func MonthOf(t time.Time) Month {
	u := t.UTC()
	return NewMonth(u.Year(), u.Month())
}

// ParseMonth reads a YYYY-MM string, the form the CLI and the API both accept.
func ParseMonth(s string) (Month, error) {
	t, err := time.Parse("2006-01", strings.TrimSpace(s))
	if err != nil {
		return Month{}, fmt.Errorf("month must be YYYY-MM, got %q", s)
	}
	return NewMonth(t.Year(), t.Month()), nil
}

// Time is the first instant of the month, UTC.
func (m Month) Time() time.Time { return m.t }

// String renders YYYY-MM.
func (m Month) String() string { return m.t.Format("2006-01") }

// Next is the following month, which is the exclusive upper bound of this one.
func (m Month) Next() Month { return Month{m.t.AddDate(0, 1, 0)} }

// Previous is the preceding month.
//
// SAFE ON EVERY DAY OF THE MONTH, which is the whole reason it exists rather than the caller doing date
// arithmetic. AddDate(0, -1, 0) on the FIRST of a month is always the first of the previous month, and
// Month values are normalised to the first by their constructors -- so there is no 31st to overflow.
//
// Compare `date -d 'last month'` in a shell, which on 31 March returns 3 March: GNU date computes 31
// February and normalises the overflow forwards. A monthly close scheduled on the 31st would silently
// freeze the wrong month, and a finalised statement is immutable by database trigger.
func (m Month) Previous() Month { return Month{m.t.AddDate(0, -1, 0)} }

// IsComplete reports whether the month has finished, relative to now.
//
// The guard on finalising. A statement closed while its month is still running would be missing days
// and immutable -- the worst combination, because the number is both wrong and frozen.
func (m Month) IsComplete(now time.Time) bool { return !now.UTC().Before(m.Next().t) }

// The report scopes. Kept as constants matching the CHECK constraint on scope_kind, so a typo is a
// compile error here rather than a constraint violation at runtime.
const (
	ScopeCluster   = "cluster"
	ScopeNamespace = "namespace"
	ScopeTeam      = "team"
)

// GenerateResult reports what generating a month's statements did.
type GenerateResult struct {
	Month Month
	// Written is how many provisional statements were created or updated.
	Written int64
	// SkippedFinalised is how many existing statements were left alone because they are closed.
	//
	// COUNTED AND RETURNED, not silently ignored. "I generated 40 statements" and "I generated 12 and
	// refused to touch 28 that were already signed off" are completely different outcomes, and a
	// caller that cannot tell them apart cannot tell whether its regeneration did anything.
	SkippedFinalised int64
}

// GenerateMonth computes provisional statements for a month at all three scopes.
//
// WHY THIS READS THE DAILY ROLLUP AND NOT THE FACT TABLE
// -----------------------------------------------------
// Two reasons, and the second matters more.
//
// It is ~293x cheaper, which is the obvious one.
//
// The real reason is that there must be exactly ONE definition of what a month's cost is. If monthly
// statements were computed from facts while the trend endpoint read the rollup, the statement and the
// chart of the same month would be computed by two different pieces of SQL -- and eventually they
// disagree by a rounding rule or a floor placement, at which point nobody can say which is correct.
// Deriving month from day from fact makes the chain single-valued: fix the rollup and every layer
// above it becomes right together.
//
// The cost is that a month is only as complete as its rolled-up days, which is exactly why
// days_with_data is stored on the row rather than assumed.
func (r *RollupRepository) GenerateMonth(ctx context.Context, m Month) (GenerateResult, error) {
	res := GenerateResult{Month: m}

	// ONE PASS WITH GROUPING SETS, not three separate queries.
	//
	// GROUPING SETS computes several groupings in a single scan, which matters here because the three
	// scopes read identical rows and differ only in how they collapse them. Three queries would scan
	// the month three times and, worse, could see different data if rows were written between them --
	// so a cluster statement might not equal the sum of its namespace statements. One pass makes that
	// inconsistency impossible rather than unlikely.
	//
	// GROUPING(col) returns 1 when the column was NOT part of the grouping set that produced this row,
	// which is how one query labels rows from different sets.
	const q = `
		WITH month_bounds AS (
		    SELECT $1::date AS start_day,
		           ($1::date + INTERVAL '1 month')::date AS end_day,
		           EXTRACT(DAY FROM ($1::date + INTERVAL '1 month - 1 day'))::int AS days_in_month
		),
		scoped AS (
		    SELECT
		        d.cluster_name,
		        CASE WHEN GROUPING(d.namespace_name) = 0 THEN 'namespace'
		             WHEN GROUPING(d.team) = 0          THEN 'team'
		             ELSE 'cluster' END AS scope_kind,
		        CASE WHEN GROUPING(d.namespace_name) = 0 THEN d.namespace_name
		             WHEN GROUPING(d.team) = 0          THEN d.team
		             ELSE d.cluster_name END AS scope_value,
		        sum(d.cpu_cost)                  AS cpu_cost,
		        sum(d.memory_cost)               AS memory_cost,
		        sum(d.cpu_billable_core_hours)   AS cpu_billable_core_hours,
		        sum(d.memory_billable_gib_hours) AS memory_billable_gib_hours,
		        sum(d.wasted_cpu_core_hours)     AS wasted_cpu_core_hours,
		        sum(d.wasted_memory_gib_hours)   AS wasted_memory_gib_hours,
		        count(DISTINCT d.day)            AS days_with_data,
		        sum(d.window_count)::bigint      AS window_count,
		        bool_or(d.estimated_rates)       AS estimated_rates
		    FROM container_allocations_daily d, month_bounds b
		    WHERE d.day >= b.start_day AND d.day < b.end_day
		    GROUP BY GROUPING SETS (
		        (d.cluster_name),
		        (d.cluster_name, d.namespace_name),
		        (d.cluster_name, d.team)
		    )
		    -- An unlabelled container produces no TEAM statement, and that is correct rather than a
		    -- gap to paper over: a team-scoped bill needs a team, and inventing one called "" would
		    -- create a statement nobody owns while making the cluster total look fully attributed.
		    -- The cluster-scoped statement still counts that cost, so nothing is lost -- the
		    -- difference between the cluster total and the sum of team statements IS the
		    -- unattributed spend, which is a number worth being able to see.
		    HAVING CASE WHEN GROUPING(d.namespace_name) = 0 THEN d.namespace_name
		                WHEN GROUPING(d.team) = 0          THEN d.team
		                ELSE d.cluster_name END <> ''
		)
		INSERT INTO monthly_reports (
		    cluster_name, period_month, scope_kind, scope_value,
		    cpu_cost, memory_cost,
		    cpu_billable_core_hours, memory_billable_gib_hours,
		    wasted_cpu_core_hours, wasted_memory_gib_hours,
		    days_with_data, days_in_month, window_count, estimated_rates,
		    generated_at
		)
		SELECT s.cluster_name, b.start_day, s.scope_kind, s.scope_value,
		       s.cpu_cost, s.memory_cost,
		       s.cpu_billable_core_hours, s.memory_billable_gib_hours,
		       s.wasted_cpu_core_hours, s.wasted_memory_gib_hours,
		       s.days_with_data, b.days_in_month, s.window_count, s.estimated_rates,
		       now()
		FROM scoped s, month_bounds b
		ON CONFLICT (cluster_name, period_month, scope_kind, scope_value) DO UPDATE SET
		    cpu_cost                  = EXCLUDED.cpu_cost,
		    memory_cost               = EXCLUDED.memory_cost,
		    cpu_billable_core_hours   = EXCLUDED.cpu_billable_core_hours,
		    memory_billable_gib_hours = EXCLUDED.memory_billable_gib_hours,
		    wasted_cpu_core_hours     = EXCLUDED.wasted_cpu_core_hours,
		    wasted_memory_gib_hours   = EXCLUDED.wasted_memory_gib_hours,
		    days_with_data            = EXCLUDED.days_with_data,
		    days_in_month             = EXCLUDED.days_in_month,
		    window_count              = EXCLUDED.window_count,
		    estimated_rates           = EXCLUDED.estimated_rates,
		    generated_at              = now()
		-- THE IMMUTABILITY RULE, as a conditional DO UPDATE.
		--
		-- Postgres allows a WHERE on DO UPDATE, so a finalised row is simply not updated: the
		-- statement affects no row and reports nothing. That is the GRACEFUL path -- regenerating a
		-- period that contains signed-off statements succeeds and leaves them intact, rather than
		-- failing and leaving the provisional ones unwritten too.
		--
		-- The trigger in migration 000004 is not this check repeated; it is the backstop for every
		-- other writer. This clause keeps the normal path quiet, the trigger keeps the invariant true.
		WHERE monthly_reports.finalised_at IS NULL`

	tag, err := r.db.Exec(ctx, q, m.Time())
	if err != nil {
		return res, fmt.Errorf("generate monthly reports for %s: %w", m, err)
	}
	res.Written = tag.RowsAffected()

	// How many statements exist for the month that this run was not allowed to touch. Queried rather
	// than inferred from the row count, because "rows I did not write" is not derivable from "rows I
	// wrote" -- some scopes may simply have had no data at all.
	if err := r.db.QueryRow(ctx, `
		SELECT count(*) FROM monthly_reports
		WHERE period_month = $1::date AND finalised_at IS NOT NULL`,
		m.Time()).Scan(&res.SkippedFinalised); err != nil {
		return res, fmt.Errorf("count finalised reports for %s: %w", m, err)
	}

	return res, nil
}

// FinaliseMonth closes every provisional statement for a month, making them immutable.
//
// Refuses an incomplete month. A statement closed mid-month is missing days AND frozen, which is the
// one combination with no recovery path short of deliberately un-finalising it -- so the guard is here
// rather than left to whoever writes the cron entry.
func (r *RollupRepository) FinaliseMonth(ctx context.Context, m Month, now time.Time) (int64, error) {
	if !m.IsComplete(now) {
		return 0, fmt.Errorf("refusing to finalise %s: the month has not ended (now %s)",
			m, now.UTC().Format(time.RFC3339))
	}

	// finalised_at IS NULL in the predicate, so this is idempotent: running it twice closes nothing
	// the second time and returns 0 rather than rewriting timestamps on already-closed statements.
	// It also means the trigger is never tripped by the normal path.
	tag, err := r.db.Exec(ctx, `
		UPDATE monthly_reports SET finalised_at = now()
		WHERE period_month = $1::date AND finalised_at IS NULL`, m.Time())
	if err != nil {
		return 0, fmt.Errorf("finalise %s: %w", m, err)
	}
	return tag.RowsAffected(), nil
}

// =============================================================================
// Reading
// =============================================================================

// MonthlyReport is one statement.
type MonthlyReport struct {
	ClusterName string `json:"cluster"`
	// Month is rendered as YYYY-MM rather than a full timestamp: a statement is for a month, and a
	// date would invite a reader to wonder what happened at 00:00 on the 1st.
	Month      string `json:"month"`
	ScopeKind  string `json:"scope_kind"`
	ScopeValue string `json:"scope_value"`

	CPUCost    decimal.Decimal `json:"cpu_cost"`
	MemoryCost decimal.Decimal `json:"memory_cost"`
	TotalCost  decimal.Decimal `json:"total_cost"`

	CPUCoreHours decimal.Decimal `json:"cpu_billable_core_hours"`
	MemGiBHours  decimal.Decimal `json:"memory_billable_gib_hours"`

	WastedCPUCoreHours decimal.Decimal `json:"wasted_cpu_core_hours"`
	WastedMemGiBHours  decimal.Decimal `json:"wasted_memory_gib_hours"`

	DaysWithData int             `json:"days_with_data"`
	DaysInMonth  int             `json:"days_in_month"`
	WindowCount  int64           `json:"window_count"`
	Coverage     decimal.Decimal `json:"coverage"`

	EstimatedRates bool `json:"estimated_rates"`

	GeneratedAt time.Time `json:"generated_at"`
	// FinalisedAt is nil while provisional. A pointer rather than a zero time, because "not yet
	// closed" is a real state a client must be able to distinguish -- and a zero timestamp
	// serialised as 0001-01-01 is a value someone will eventually render on a page.
	FinalisedAt *time.Time `json:"finalised_at"`
}

// Finalised reports whether the statement is closed.
func (m MonthlyReport) Finalised() bool { return m.FinalisedAt != nil }

// MonthlyReportParams selects statements.
type MonthlyReportParams struct {
	// From and To bound the months inclusively at both ends, which is the one place this codebase
	// departs from half-open ranges -- deliberately. A month is a label, not an instant, and
	// "January to March" unambiguously means three months to every human who asks for it. A
	// half-open month range would make ?from=2026-01&to=2026-03 return two months, which reads as
	// an off-by-one bug in the API rather than as a convention.
	From Month
	To   Month
	// ScopeKind filters to one scope. Empty returns all three, which is rarely what a caller wants
	// but is what makes the endpoint explorable.
	ScopeKind  string
	ScopeValue string
	Limit      int
}

// MonthlyReports reads statements, newest month first.
func (r *RollupRepository) MonthlyReports(ctx context.Context, p MonthlyReportParams) ([]MonthlyReport, error) {
	args := []any{p.From.Time(), p.To.Time()}
	// Inclusive upper bound, per the note on MonthlyReportParams.From.
	where := []string{"period_month >= $1::date", "period_month <= $2::date"}

	if p.ScopeKind != "" {
		switch p.ScopeKind {
		case ScopeCluster, ScopeNamespace, ScopeTeam:
		default:
			// Checked against the same three constants the CHECK constraint uses, so an unknown scope
			// is an error rather than a query that correctly returns nothing and looks like no data.
			return nil, fmt.Errorf("unknown scope kind %q", p.ScopeKind)
		}
		args = append(args, p.ScopeKind)
		where = append(where, fmt.Sprintf("scope_kind = $%d", len(args)))
	}
	if p.ScopeValue != "" {
		args = append(args, p.ScopeValue)
		where = append(where, fmt.Sprintf("scope_value = $%d", len(args)))
	}

	limit := p.Limit
	if limit <= 0 || limit > maxSummaryRows {
		limit = maxSummaryRows
	}

	query := fmt.Sprintf(`
		SELECT cluster_name, period_month, scope_kind, scope_value,
		       cpu_cost, memory_cost, total_cost,
		       cpu_billable_core_hours, memory_billable_gib_hours,
		       wasted_cpu_core_hours, wasted_memory_gib_hours,
		       days_with_data, days_in_month, window_count, coverage,
		       estimated_rates, generated_at, finalised_at
		FROM monthly_reports
		WHERE %s
		-- Newest first, then largest cost: a statement page is read to see the latest month, and
		-- within it the biggest line. scope_kind and scope_value break the tie so the order is total
		-- and paging is stable.
		ORDER BY period_month DESC, total_cost DESC, scope_kind, scope_value
		LIMIT %d`, strings.Join(where, " AND "), limit)

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query monthly reports: %w", err)
	}
	defer rows.Close()

	out := []MonthlyReport{}
	for rows.Next() {
		var m MonthlyReport
		var period time.Time
		if err := rows.Scan(
			&m.ClusterName, &period, &m.ScopeKind, &m.ScopeValue,
			&m.CPUCost, &m.MemoryCost, &m.TotalCost,
			&m.CPUCoreHours, &m.MemGiBHours,
			&m.WastedCPUCoreHours, &m.WastedMemGiBHours,
			&m.DaysWithData, &m.DaysInMonth, &m.WindowCount, &m.Coverage,
			&m.EstimatedRates, &m.GeneratedAt, &m.FinalisedAt,
		); err != nil {
			return nil, fmt.Errorf("scan monthly report: %w", err)
		}
		m.Month = MonthOf(period).String()
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate monthly reports: %w", err)
	}
	return out, nil
}
