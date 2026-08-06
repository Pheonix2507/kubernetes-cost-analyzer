package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// ReportRepository serves the read side: aggregated cost and paginated raw allocations.
//
// SEPARATE FROM AllocationRepository ON PURPOSE
// ---------------------------------------------
// That one writes; this one reads. They have almost nothing in common: the write path cares
// about batching and conflict resolution, the read path about filters, sorting and pagination.
// Keeping them together would produce one type whose methods share no state and no concerns.
//
// It also mirrors how they will diverge. A read replica is the obvious first scaling move for a
// reporting workload, and when that happens this repository takes a different pool while the
// writer keeps the primary. That is a constructor change here and nothing else.
type ReportRepository struct {
	db Querier
}

// NewReportRepository returns a repository bound to db.
func NewReportRepository(db Querier) *ReportRepository {
	return &ReportRepository{db: db}
}

// =============================================================================
// The allow-lists
// =============================================================================

// GroupBy names a dimension cost can be aggregated by.
//
// WHY AN ENUM AND A MAP RATHER THAN A COLUMN NAME FROM THE QUERY STRING
// --------------------------------------------------------------------
// SQL placeholders bind VALUES, not IDENTIFIERS. There is no `GROUP BY $1`, so a
// caller-chosen grouping column must be interpolated into the statement -- and interpolating a
// query-string value into SQL is the textbook injection.
//
// So the request carries a symbolic name, this package maps it to columns it owns, and an
// unrecognised name is a 400 rather than a query. The user's string never reaches the SQL.
// The same reasoning applies to every identifier interpolated into SQL in this package.
type GroupBy string

// The dimensions cost may be grouped by.
const (
	GroupByNamespace    GroupBy = "namespace"
	GroupByTeam         GroupBy = "team"
	GroupByEnvironment  GroupBy = "environment"
	GroupByCostCentre   GroupBy = "cost_centre"
	GroupByWorkload     GroupBy = "workload"
	GroupByNode         GroupBy = "node"
	GroupByInstanceType GroupBy = "instance_type"
	GroupByCapacityType GroupBy = "capacity_type"
	GroupByPod          GroupBy = "pod"
	GroupByContainer    GroupBy = "container"
)

// groupByColumns maps each grouping to the columns it selects and groups by.
//
// Some groupings need SEVERAL columns to be meaningful. Two namespaces can each have a
// Deployment called "api", so grouping by workload name alone would merge unrelated services
// into one row and silently sum their cost -- which is a wrong answer that looks entirely
// plausible. The same applies to pods and containers.
var groupByColumns = map[GroupBy][]string{
	GroupByNamespace:    {"namespace_name"},
	GroupByTeam:         {"team"},
	GroupByEnvironment:  {"environment"},
	GroupByCostCentre:   {"cost_centre"},
	GroupByWorkload:     {"namespace_name", "workload_kind", "workload_name"},
	GroupByNode:         {"node_name"},
	GroupByInstanceType: {"instance_type"},
	GroupByCapacityType: {"capacity_type"},
	GroupByPod:          {"namespace_name", "pod_name"},
	GroupByContainer:    {"namespace_name", "pod_name", "container_name"},
}

// ValidGroupBy reports whether g is a recognised grouping, and is what the HTTP layer calls
// before building a query.
func ValidGroupBy(g GroupBy) bool {
	_, ok := groupByColumns[g]
	return ok
}

// GroupByOptions returns every valid grouping, sorted, for error messages and the OpenAPI spec.
//
// Generated from the same map the query uses, so a new grouping cannot be added without
// appearing in the documentation and the error text. Two hand-maintained lists would drift.
func GroupByOptions() []string {
	out := make([]string, 0, len(groupByColumns))
	for g := range groupByColumns {
		out = append(out, string(g))
	}
	sortStrings(out)
	return out
}

// SortField names a column results may be ordered by.
type SortField string

// The fields a summary may be sorted by.
const (
	SortByTotalCost    SortField = "total_cost"
	SortByCPUCost      SortField = "cpu_cost"
	SortByMemoryCost   SortField = "memory_cost"
	SortByCPUCoreHours SortField = "cpu_core_hours"
	SortByWasteCPU     SortField = "wasted_cpu_core_hours"
	SortByDimension    SortField = "dimension"
)

// sortExpressions maps a sort field to the SQL it orders by.
//
// These are EXPRESSIONS rather than plain column names, because a summary's sortable values are
// aggregates that do not exist as columns. They are still fixed strings owned by this package,
// never caller input.
var sortExpressions = map[SortField]string{
	SortByTotalCost:    "total_cost",
	SortByCPUCost:      "cpu_cost",
	SortByMemoryCost:   "memory_cost",
	SortByCPUCoreHours: "cpu_core_hours",
	SortByWasteCPU:     "wasted_cpu_core_hours",
	// dimension sorts by the grouping columns, resolved per query since they vary.
	SortByDimension: "",
}

// ValidSortField reports whether s is recognised.
func ValidSortField(s SortField) bool {
	_, ok := sortExpressions[s]
	return ok
}

// SortFieldOptions returns every valid sort field, sorted.
func SortFieldOptions() []string {
	out := make([]string, 0, len(sortExpressions))
	for s := range sortExpressions {
		out = append(out, string(s))
	}
	sortStrings(out)
	return out
}

// Filters narrows a query. Every field is optional; the zero value matches everything.
//
// Each is compared against a DENORMALISED column on the fact table, so none of these needs a
// join. That is the payoff for the denormalisation decision in Phase 2: the dashboard's main
// query touches one table.
type Filters struct {
	// Cluster scopes a query to one cluster, and it is listed first because it is the broadest
	// scope: every other filter narrows within a cluster, never across them.
	//
	// WHY THERE IS NO INDEX LEADING WITH cluster_name ON THE FACT TABLE
	// ---------------------------------------------------------------
	// Every existing index leads with namespace_name, team, workload or node. Adding
	// (cluster_name, ...) variants is the obvious next step and is deliberately NOT taken yet:
	// with a single cluster the predicate matches 100% of rows, so an index on it can only cost
	// write throughput on the hottest table in the schema while saving nothing.
	//
	// The trigger for revisiting it is concrete rather than a feeling: when a second cluster's
	// data lands and EXPLAIN shows a cluster-scoped query reading substantially more rows than it
	// returns. At that point the right change is probably to make cluster_name the LEADING column
	// of the existing composites rather than to add new indexes beside them, since in a fleet
	// every query is cluster-scoped and a scan of one cluster's namespaces is the common case.
	Cluster      string
	Namespace    string
	Team         string
	Environment  string
	CostCentre   string
	WorkloadKind string
	WorkloadName string
	Node         string
	InstanceType string
	CapacityType string
	// EstimatedOnly restricts results to rows priced from a fallback guess, which answers
	// "how much of this bill is not real?" -- see rate_source in migration 000002.
	EstimatedOnly bool
}

// filterColumns maps each filter to its column. Fixed strings, never caller input.
var filterColumns = map[string]string{
	"cluster":       "cluster_name",
	"namespace":     "namespace_name",
	"team":          "team",
	"environment":   "environment",
	"cost_centre":   "cost_centre",
	"workload_kind": "workload_kind",
	"workload_name": "workload_name",
	"node":          "node_name",
	"instance_type": "instance_type",
	"capacity_type": "capacity_type",
}

// FilterOptions returns every filter name, sorted, for the OpenAPI spec.
func FilterOptions() []string {
	out := make([]string, 0, len(filterColumns))
	for f := range filterColumns {
		out = append(out, f)
	}
	sortStrings(out)
	return out
}

// values returns the non-empty filters as name/value pairs, in a stable order.
//
// Stable ORDER matters: it makes the generated SQL deterministic for a given filter set, which
// means Postgres can reuse a prepared-statement plan instead of re-planning an identically
// shaped query whose predicates happen to be listed differently.
func (f Filters) values() [][2]string {
	pairs := [][2]string{
		{"cluster", f.Cluster},
		{"namespace", f.Namespace},
		{"team", f.Team},
		{"environment", f.Environment},
		{"cost_centre", f.CostCentre},
		{"workload_kind", f.WorkloadKind},
		{"workload_name", f.WorkloadName},
		{"node", f.Node},
		{"instance_type", f.InstanceType},
		{"capacity_type", f.CapacityType},
	}
	out := make([][2]string, 0, len(pairs))
	for _, p := range pairs {
		if p[1] != "" {
			out = append(out, p)
		}
	}
	return out
}

// =============================================================================
// Cost summary
// =============================================================================

// CostSummaryParams describes a summary query.
type CostSummaryParams struct {
	// From and To bound a HALF-OPEN range [From, To), matching the window semantics. Using a
	// closed range would double-count the boundary window in two adjacent reports.
	From time.Time
	To   time.Time

	GroupBy GroupBy
	Filters Filters
	SortBy  SortField
	// Descending puts the largest first, which is what a cost report wants by default: the
	// question is almost always "what is expensive", not "what is cheap".
	Descending bool
	Limit      int
}

// CostSummaryRow is one aggregated row.
//
// The dimension fields are all omitempty and only those relevant to the requested grouping are
// populated. Explicit fields rather than a map[string]string: a map is more flexible and gives
// a client no schema at all, whereas these appear in the OpenAPI spec and in a generated
// TypeScript type.
type CostSummaryRow struct {
	Namespace    string `json:"namespace,omitempty"`
	Team         string `json:"team,omitempty"`
	Environment  string `json:"environment,omitempty"`
	CostCentre   string `json:"cost_centre,omitempty"`
	WorkloadKind string `json:"workload_kind,omitempty"`
	WorkloadName string `json:"workload_name,omitempty"`
	Node         string `json:"node,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
	CapacityType string `json:"capacity_type,omitempty"`
	PodName      string `json:"pod_name,omitempty"`
	Container    string `json:"container_name,omitempty"`

	// Quantities, as exact decimals rendered to JSON strings. See the note on nodeRates in
	// internal/httpapi: a JSON number is parsed as a double by every browser.
	CPURequestedCoreHours decimal.Decimal `json:"cpu_requested_core_hours"`
	CPUUsedCoreHours      decimal.Decimal `json:"cpu_used_core_hours"`
	CPUCoreHours          decimal.Decimal `json:"cpu_billable_core_hours"`
	MemRequestedGiBHours  decimal.Decimal `json:"memory_requested_gib_hours"`
	MemUsedGiBHours       decimal.Decimal `json:"memory_used_gib_hours"`
	MemGiBHours           decimal.Decimal `json:"memory_billable_gib_hours"`

	// WastedCPUCoreHours is requested minus used, floored at zero.
	//
	// FLOORED because "waste" cannot be negative: a container using more than it requested is
	// not saving money, it is under-requested -- a reliability problem reported separately.
	// Letting it go negative would silently offset genuine waste elsewhere in a SUM, so a team
	// with one under-requested pod would appear more efficient than it is.
	WastedCPUCoreHours decimal.Decimal `json:"wasted_cpu_core_hours"`
	WastedMemGiBHours  decimal.Decimal `json:"wasted_memory_gib_hours"`

	CPUCost    decimal.Decimal `json:"cpu_cost"`
	MemoryCost decimal.Decimal `json:"memory_cost"`
	TotalCost  decimal.Decimal `json:"total_cost"`

	// Containers is how many container-windows were aggregated. A sanity check for the caller:
	// an unexpectedly small number usually means a filter matched less than intended.
	Containers int64 `json:"container_windows"`

	// EstimatedRates is true when ANY row in this group was priced from a fallback guess.
	//
	// Surfaced per group rather than only per request, because a report where one team's cost is
	// estimated and the rest are exact should say which. Aggregating away the provenance would
	// waste the entire point of tracking it.
	EstimatedRates bool `json:"estimated_rates"`
}

// CostSummary aggregates cost over a time range, grouped by one dimension.
//
// This is the endpoint the dashboard leads with, and the reason for the
// (namespace_name, window_start DESC) and (team, window_start DESC) indexes.
func (r *ReportRepository) CostSummary(ctx context.Context, p CostSummaryParams) ([]CostSummaryRow, error) {
	cols, ok := groupByColumns[p.GroupBy]
	if !ok {
		// Defensive: the HTTP layer validates first, so reaching here means a caller bypassed
		// it. Failing loudly beats defaulting to some grouping the caller did not ask for.
		return nil, fmt.Errorf("unknown group_by %q", p.GroupBy)
	}
	if !ValidSortField(p.SortBy) {
		return nil, fmt.Errorf("unknown sort field %q", p.SortBy)
	}

	// args grows as predicates are added, and each placeholder number is derived from its
	// position -- so the SQL and the argument list cannot drift apart.
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
	if p.Filters.EstimatedOnly {
		where = append(where, "rate_source = 'fallback'")
	}

	// The ORDER BY expression comes from the allow-list or from the grouping columns; the
	// direction is one of two literals. Neither is caller text.
	orderExpr := sortExpressions[p.SortBy]
	if p.SortBy == SortByDimension {
		orderExpr = strings.Join(cols, ", ")
	}
	direction := "ASC"
	if p.Descending {
		direction = "DESC"
	}

	limit := p.Limit
	if limit <= 0 || limit > maxSummaryRows {
		limit = maxSummaryRows
	}

	// A CTE for readability, so the aggregate expressions are named once and the ORDER BY can
	// refer to them. Postgres inlines a non-recursive CTE, so this costs nothing at runtime --
	// unlike before version 12, when a CTE was an optimisation fence.
	query := fmt.Sprintf(`
		WITH aggregated AS (
			SELECT %[1]s,
			       %[2]s AS cpu_requested_core_hours,
			       %[3]s AS cpu_used_core_hours,
			       %[4]s AS cpu_core_hours,
			       %[5]s AS memory_requested_gib_hours,
			       %[6]s AS memory_used_gib_hours,
			       %[7]s AS memory_gib_hours,
			       COALESCE(sum(cpu_cost), 0)    AS cpu_cost,
			       COALESCE(sum(memory_cost), 0) AS memory_cost,
			       COALESCE(sum(total_cost), 0)  AS total_cost,
			       count(*)                      AS container_windows,
			       -- bool_or rather than a count: the caller needs to know IF any rate was
			       -- guessed, not how many were.
			       bool_or(rate_source = 'fallback') AS estimated_rates,
			       -- WASTE IS FLOORED PER ROW, INSIDE THE SUM. See wastedCoreHours.
			       %[9]s  AS wasted_cpu_core_hours,
			       %[10]s AS wasted_memory_gib_hours
			FROM container_allocations
			WHERE %[8]s
			GROUP BY %[1]s
		)
		SELECT * FROM aggregated
		ORDER BY %[11]s %[12]s, %[1]s
		LIMIT %[13]d`,
		strings.Join(cols, ", "),
		coreHours("cpu_millicores_requested"),
		coreHours("cpu_millicores_used"),
		coreHours("cpu_millicores_billable"),
		gibHours("memory_bytes_requested"),
		gibHours("memory_bytes_used"),
		gibHours("memory_bytes_billable"),
		strings.Join(where, " AND "),
		wastedCoreHours("cpu_millicores_requested", "cpu_millicores_used"),
		wastedGibHours("memory_bytes_requested", "memory_bytes_used"),
		orderExpr, direction,
		limit,
	)

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying cost summary: %w", err)
	}
	defer rows.Close()

	out := []CostSummaryRow{}
	for rows.Next() {
		var row CostSummaryRow

		// Scan destinations are built to MATCH the dynamic column list. The grouping columns
		// come first and vary in number, so their destinations are assembled the same way the
		// SELECT was -- from the same slice, in the same order. Hardcoding a fixed destination
		// list would silently misalign the moment a grouping used a different column count.
		groupValues := make([]string, len(cols))
		dest := make([]any, 0, len(cols)+15)
		for i := range groupValues {
			dest = append(dest, &groupValues[i])
		}
		dest = append(dest,
			&row.CPURequestedCoreHours, &row.CPUUsedCoreHours, &row.CPUCoreHours,
			&row.MemRequestedGiBHours, &row.MemUsedGiBHours, &row.MemGiBHours,
			&row.CPUCost, &row.MemoryCost, &row.TotalCost,
			&row.Containers, &row.EstimatedRates,
			&row.WastedCPUCoreHours, &row.WastedMemGiBHours,
		)

		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scanning cost summary row: %w", err)
		}
		assignGroupValues(&row, cols, groupValues)
		out = append(out, row)
	}
	// MUST be checked: rows.Next() returns false both for "finished" and for "failed
	// mid-stream". Skipping it returns a TRUNCATED report as though it were complete, which for
	// cost data means quietly under-reporting the bill.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating cost summary: %w", err)
	}
	return out, nil
}

// maxSummaryRows caps a summary response.
//
// A grouping by container on a large cluster has hundreds of thousands of groups, and an
// unbounded response would try to serialise all of them into memory. The cap is a backstop
// rather than pagination: a summary is meant to be read, and a caller wanting every container
// wants /allocations.
const maxSummaryRows = 1000

// coreHours renders millicores-over-a-window as core-hours.
//
// The window length is read from each row rather than assumed, because windows are not
// necessarily uniform: the interval is configurable and a restart can produce a short one.
// Assuming a fixed five minutes would misprice every row the moment someone changed the
// setting.
func coreHours(column string) string {
	return fmt.Sprintf(
		`COALESCE(sum(%s::numeric / 1000 * EXTRACT(EPOCH FROM (window_end - window_start)) / 3600), 0)`,
		column)
}

// gibHours renders bytes-over-a-window as GiB-hours. 2^30, the binary gibibyte Kubernetes
// reports -- not 10^9, which is what clouds usually mean by "GB". Mixing them is a silent 7%
// error.
func gibHours(column string) string {
	return fmt.Sprintf(
		`COALESCE(sum(%s::numeric / 1073741824 * EXTRACT(EPOCH FROM (window_end - window_start)) / 3600), 0)`,
		column)
}

// wastedCoreHours sums per-row waste, floored at zero BEFORE aggregation.
//
// WHY THE FLOOR HAS TO BE INSIDE THE SUM
// --------------------------------------
// This was previously GREATEST(sum(requested) - sum(used), 0) applied after the GROUP BY, and that is
// wrong in a way that reads as correct. The floor stopped the FINAL figure being negative; it did not
// stop the cancellation, because the cancellation happens inside the two sums before the subtraction.
//
// A group with one container wasting 90 millicores and one under-requested by 990 aggregated to
// 90 - 990 = -900, which floored to 0. So a namespace containing real, reclaimable waste reported
// ZERO waste -- and the more under-requested a team's workloads were, the more efficient it looked.
// The metric rewarded the riskier configuration, which is the opposite of the intent written in the
// comment on WastedCPUCoreHours.
//
// sum(GREATEST(per-row, 0)) instead: each row's waste is floored on its own, so an under-requested
// container contributes zero rather than a credit. This is one of those bugs where the doc comment
// described the correct behaviour and the SQL beneath it did something else -- worth remembering that
// a comment asserting a property is not a test of it.
func wastedCoreHours(requested, used string) string {
	return fmt.Sprintf(
		`COALESCE(sum(GREATEST(%s - %s, 0)::numeric / 1000 `+
			`* EXTRACT(EPOCH FROM (window_end - window_start)) / 3600), 0)`,
		requested, used)
}

// wastedGibHours is wastedCoreHours for memory. See there for why the floor is per row.
func wastedGibHours(requested, used string) string {
	return fmt.Sprintf(
		`COALESCE(sum(GREATEST(%s - %s, 0)::numeric / 1073741824 `+
			`* EXTRACT(EPOCH FROM (window_end - window_start)) / 3600), 0)`,
		requested, used)
}

// assignGroupValues copies the scanned grouping values onto their named fields.
func assignGroupValues(row *CostSummaryRow, cols, values []string) {
	for i, col := range cols {
		if i >= len(values) {
			return
		}
		switch col {
		case "namespace_name":
			row.Namespace = values[i]
		case "team":
			row.Team = values[i]
		case "environment":
			row.Environment = values[i]
		case "cost_centre":
			row.CostCentre = values[i]
		case "workload_kind":
			row.WorkloadKind = values[i]
		case "workload_name":
			row.WorkloadName = values[i]
		case "node_name":
			row.Node = values[i]
		case "instance_type":
			row.InstanceType = values[i]
		case "capacity_type":
			row.CapacityType = values[i]
		case "pod_name":
			row.PodName = values[i]
		case "container_name":
			row.Container = values[i]
		}
	}
}

// sortStrings is a tiny insertion sort, so this file needs no sort import for two call sites
// that run once at startup.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
