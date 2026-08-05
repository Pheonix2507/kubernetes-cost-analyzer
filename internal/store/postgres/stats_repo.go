package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// ContainerStats summarises one container's behaviour over a range, and is the only input the
// recommendation engine needs.
//
// WHY A DEDICATED QUERY RATHER THAN REUSING CostSummary
// ----------------------------------------------------
// A summary answers "what did this cost", which is a SUM. A recommendation answers "what should this
// be", which needs percentiles, peaks, replica counts and an observation span. Bolting those onto the
// summary would make every dashboard query compute statistics it does not use, and would leave the
// summary's aggregate expressions serving two incompatible purposes.
type ContainerStats struct {
	Namespace    string
	WorkloadKind string
	WorkloadName string
	Container    string

	// Replicas is how many distinct pods carried this container in the range.
	//
	// Derived from the DATA rather than read from the Deployment's spec, deliberately. The spec says
	// what was asked for; this says what actually ran, which is what was actually paid for. A
	// Deployment stuck part-way through a rollout differs from its own spec, and the bill follows
	// reality.
	Replicas int

	// WindowCount and the observation span are the CONFIDENCE inputs.
	//
	// Without them the engine would happily recommend deleting a service from twenty minutes of
	// data collected at 3am. Every rule checks these before firing; see recommend.minWindows.
	WindowCount  int
	ObservedFrom time.Time
	ObservedTo   time.Time

	// PeakCoverage is the fraction of windows carrying peak data, between 0 and 1.
	//
	// Rows written before migration 000003 have max = 0. Treating those as a genuine zero peak would
	// make every historical container look idle and produce a flood of confident deletion advice --
	// so a rule needing peaks checks coverage first and stays silent rather than guessing.
	PeakCoverage float64

	// Requests are the MAXIMUM observed across the range.
	//
	// Maximum rather than latest or average: if a request changed mid-range, the larger figure is the
	// conservative basis for a reduction recommendation. Recommending down from the smaller one could
	// suggest a value below what the workload is currently configured to have.
	CPURequestedMillicores int64
	MemRequestedBytes      int64

	// Average usage. The cost-relevant statistic, kept for context in the evidence.
	CPUAvgMillicores int64
	MemAvgBytes      int64

	// P95 of the per-window PEAKS -- the right-sizing signal.
	//
	// A percentile over peaks, not over averages. p95 of five-minute averages has already had the
	// bursts smoothed out of it, so it understates the true peak and any request derived from it
	// throttles the workload. p95 rather than max because one pathological spike -- a startup burst,
	// a one-off batch job -- should not size the container forever.
	CPUP95Millicores int64
	MemP95Bytes      int64

	// The absolute peak seen. Used for the idle test, where "never exceeded a trivial floor" is a far
	// stronger claim than "averaged near zero".
	CPUMaxMillicores int64
	MemMaxBytes      int64

	TotalCost decimal.Decimal
	// CPUCostPerCoreHour and MemCostPerGiBHour are the latest observed rates, needed to convert a
	// proposed reduction into money.
	CPUCostPerCoreHour decimal.Decimal
	MemCostPerGiBHour  decimal.Decimal

	QoSClass       string
	EstimatedRates bool
}

// Duration returns the observation span.
func (s ContainerStats) Duration() time.Duration { return s.ObservedTo.Sub(s.ObservedFrom) }

// ContainerStatsParams bounds a stats query.
type ContainerStatsParams struct {
	From time.Time
	To   time.Time
	// Filters narrows which containers are analysed, reusing the summary's allow-list.
	Filters Filters
	// Limit caps how many containers are returned, ordered by cost so the most expensive -- and
	// therefore the most worth recommending on -- come first.
	Limit int
}

// maxStatsRows bounds a stats query. Recommendations are read by a human, and a thousand of them is
// the same as none.
const maxStatsRows = 1000

// ContainerStats computes per-container statistics for the recommendation engine.
func (r *ReportRepository) ContainerStats(ctx context.Context, p ContainerStatsParams) ([]ContainerStats, error) {
	args := []any{p.From, p.To}
	where := []string{"window_start >= $1", "window_start < $2"}

	for _, f := range p.Filters.values() {
		col, known := filterColumns[f[0]]
		if !known {
			return nil, fmt.Errorf("unknown filter %q", f[0])
		}
		args = append(args, f[1])
		where = append(where, fmt.Sprintf("%s = $%d", col, len(args)))
	}

	limit := p.Limit
	if limit <= 0 || limit > maxStatsRows {
		limit = maxStatsRows
	}

	// percentile_cont interpolates between samples; percentile_disc would return an actual observed
	// value. Interpolation is the better choice here: with few windows, the discrete p95 collapses to
	// the maximum, which would size every container to its worst-ever spike.
	query := fmt.Sprintf(`
		SELECT namespace_name, workload_kind, workload_name, container_name,
		       count(DISTINCT pod_name)                       AS replicas,
		       count(*)                                       AS window_count,
		       min(window_start)                              AS observed_from,
		       max(window_end)                                AS observed_to,
		       -- Coverage keyed on MEMORY, not CPU, and that distinction was a real bug.
		       --
		       -- An earlier version tested cpu_millicores_max > 0, which CONFLATES two different
		       -- things: "this row predates migration 000003 so no peak was recorded" and "this
		       -- container genuinely uses no measurable CPU".
		       --
		       -- The consequence was the opposite of the intent. The check exists to stop the engine
		       -- inventing idle findings from missing data -- but a genuinely idle container has a CPU
		       -- peak that rounds to zero, so it failed the coverage test and was EXCLUDED. The guard
		       -- against false positives was suppressing the true positives, and the idle-service
		       -- fixture was skipped for being too idle.
		       --
		       -- Memory is the reliable signal: every RUNNING container holds some, so a zero memory
		       -- peak really does mean "not recorded". Even 'sleep infinity' holds a few hundred KiB.
		       (count(*) FILTER (WHERE memory_bytes_max > 0))::float8 / count(*) AS peak_coverage,

		       max(cpu_millicores_requested)                  AS cpu_requested,
		       max(memory_bytes_requested)                    AS mem_requested,

		       -- avg() is ALREADY per-container, and that is worth being explicit about because it
		       -- looks like it might not be. The GROUP BY collapses every replica and every window
		       -- into one row, so avg divides by (replicas x windows) -- giving the mean for a
		       -- SINGLE container, not the workload total. A sum() here would be the workload's
		       -- total and would recommend requesting six times what one pod needs.
		       (avg(cpu_millicores_used))::bigint             AS cpu_avg,
		       (avg(memory_bytes_used))::bigint               AS mem_avg,

		       -- p95 over the per-window PEAKS. Rows with no peak data are excluded rather than
		       -- counted as zero, which would drag the percentile down and understate the request.
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (
		           ORDER BY NULLIF(cpu_millicores_max, 0)), 0)::bigint AS cpu_p95,
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (
		           ORDER BY NULLIF(memory_bytes_max, 0)), 0)::bigint   AS mem_p95,

		       max(cpu_millicores_max)                        AS cpu_max,
		       max(memory_bytes_max)                          AS mem_max,

		       COALESCE(sum(total_cost), 0)                   AS total_cost,
		       -- The LATEST rate, not an average: converting a proposed reduction into money must use
		       -- the price that applies now, not one blended across a period in which it changed.
		       COALESCE((array_agg(cpu_cost_per_core_hour ORDER BY window_start DESC))[1], 0)   AS cpu_rate,
		       COALESCE((array_agg(memory_cost_per_gib_hour ORDER BY window_start DESC))[1], 0) AS mem_rate,
		       COALESCE((array_agg(qos_class ORDER BY window_start DESC))[1], '')               AS qos_class,
		       bool_or(rate_source = 'fallback')              AS estimated_rates
		FROM container_allocations
		WHERE %s
		GROUP BY namespace_name, workload_kind, workload_name, container_name
		ORDER BY total_cost DESC
		LIMIT %d`,
		joinAnd(where), limit)

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying container stats: %w", err)
	}
	defer rows.Close()

	out := []ContainerStats{}
	for rows.Next() {
		var s ContainerStats
		if err := rows.Scan(
			&s.Namespace, &s.WorkloadKind, &s.WorkloadName, &s.Container,
			&s.Replicas, &s.WindowCount, &s.ObservedFrom, &s.ObservedTo, &s.PeakCoverage,
			&s.CPURequestedMillicores, &s.MemRequestedBytes,
			&s.CPUAvgMillicores, &s.MemAvgBytes,
			&s.CPUP95Millicores, &s.MemP95Bytes,
			&s.CPUMaxMillicores, &s.MemMaxBytes,
			&s.TotalCost, &s.CPUCostPerCoreHour, &s.MemCostPerGiBHour,
			&s.QoSClass, &s.EstimatedRates,
		); err != nil {
			return nil, fmt.Errorf("scanning container stats: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating container stats: %w", err)
	}
	return out, nil
}

// joinAnd joins predicates. A named helper because both this file and its siblings build WHERE
// clauses the same way, and strings.Join(where, " AND ") read inline obscures what is happening.
func joinAnd(predicates []string) string {
	out := ""
	for i, p := range predicates {
		if i > 0 {
			out += " AND "
		}
		out += p
	}
	return out
}
