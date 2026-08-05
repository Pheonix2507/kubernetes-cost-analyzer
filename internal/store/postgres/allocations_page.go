package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Cursor is an opaque position in a result set.
//
// WHY KEYSET PAGINATION AND NOT OFFSET
// ------------------------------------
// `LIMIT 50 OFFSET 5000` is simpler and wrong for this table, in two independent ways:
//
//	PERFORMANCE. Postgres must produce and DISCARD 5,000 rows to return 50. Cost is O(offset),
//	so page 100 is a hundred times more expensive than page 1 -- and the fact table grows
//	forever, so the deep pages get slower every month.
//
//	CORRECTNESS, which matters more. Offset assumes a stable result set. The collector appends
//	rows every five minutes, so between fetching page 1 and page 2 the data has SHIFTED: rows
//	move across the boundary and the client silently sees a duplicate or misses one entirely.
//	Nothing about the response indicates it happened.
//
// Keyset pagination asks "give me the rows after this exact position" instead of "skip this
// many". The predicate is an index range scan, so cost is O(log n) regardless of depth, and it
// is stable under concurrent writes because a newly inserted row does not move any existing
// row's key.
//
// The trade-off, stated plainly: you cannot jump to page N, and you cannot show a total page
// count without a separate COUNT. A cost API does not need either -- users scroll or they
// filter.
//
// THE CURSOR IS OPAQUE TO CLIENTS, deliberately. It is base64-encoded JSON, so the internal
// keyset can change -- adding a tie-breaker column, or reversing the sort -- without breaking
// anyone who stored one. A client that parses a cursor has coupled itself to our primary key.
type Cursor struct {
	// These three ARE the fact table's primary key, in its order. That is not a coincidence:
	// paginating on anything else would mean a sort the index cannot serve.
	WindowStart   time.Time `json:"w"`
	PodID         int64     `json:"p"`
	ContainerName string    `json:"c"`
}

// Encode renders the cursor as a URL-safe string.
//
// base64.RawURLEncoding: URL-safe alphabet so it needs no percent-encoding in a query string,
// and raw (unpadded) so there is no trailing "=" for a client or proxy to mangle.
func (c Cursor) Encode() (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encoding cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeCursor parses a cursor produced by Encode.
//
// A malformed cursor is a 400, not a 500 and not a silent reset to the first page. Silently
// restarting would make a client with a stale cursor loop forever over the first page without
// ever being told why.
func DecodeCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, fmt.Errorf("cursor is not valid base64: %w", err)
	}
	var c Cursor
	// DisallowUnknownFields is not used: a cursor issued by an OLDER version of this service
	// may carry fields this one has dropped, and rejecting it would break pagination across a
	// rolling deploy -- exactly when both versions are serving.
	if err := json.Unmarshal(raw, &c); err != nil {
		return Cursor{}, fmt.Errorf("cursor is not valid: %w", err)
	}
	if c.WindowStart.IsZero() || c.ContainerName == "" {
		return Cursor{}, fmt.Errorf("cursor is incomplete")
	}
	return c, nil
}

// AllocationsParams describes a page request.
type AllocationsParams struct {
	From    time.Time
	To      time.Time
	Filters Filters
	Limit   int
	// Cursor is nil for the first page.
	Cursor *Cursor
}

// AllocationRow is one fact row as the API returns it.
//
// Deliberately NOT domain.ContainerAllocation. That type is the engine's output and carries
// PodID, an internal surrogate key that means nothing to a client and would couple the API to
// our schema. This is a response shape, chosen for a consumer.
type AllocationRow struct {
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`

	Namespace    string `json:"namespace"`
	PodName      string `json:"pod_name"`
	Container    string `json:"container_name"`
	WorkloadKind string `json:"workload_kind,omitempty"`
	WorkloadName string `json:"workload_name,omitempty"`
	Node         string `json:"node_name,omitempty"`
	Team         string `json:"team,omitempty"`
	CostCentre   string `json:"cost_centre,omitempty"`
	Environment  string `json:"environment,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
	CapacityType string `json:"capacity_type,omitempty"`
	Zone         string `json:"zone,omitempty"`
	QoSClass     string `json:"qos_class,omitempty"`

	CPUMillicoresRequested int64 `json:"cpu_millicores_requested"`
	CPUMillicoresUsed      int64 `json:"cpu_millicores_used"`
	CPUMillicoresBillable  int64 `json:"cpu_millicores_billable"`
	MemoryBytesRequested   int64 `json:"memory_bytes_requested"`
	MemoryBytesUsed        int64 `json:"memory_bytes_used"`
	MemoryBytesBillable    int64 `json:"memory_bytes_billable"`

	CPUCost    decimal.Decimal `json:"cpu_cost"`
	MemoryCost decimal.Decimal `json:"memory_cost"`
	TotalCost  decimal.Decimal `json:"total_cost"`

	// RateSource is surfaced so a caller can tell a real price from a fallback guess. See
	// migration 000002.
	RateSource string `json:"rate_source,omitempty"`
}

// AllocationsPage is one page of results plus the cursor for the next.
type AllocationsPage struct {
	Rows []AllocationRow
	// NextCursor is empty when there are no more rows.
	NextCursor string
}

// maxPageSize caps a page. A caller asking for a million rows gets 500, because the response is
// buffered in memory before it is written (see writeJSON) and an unbounded page is a way to make
// the service OOM from a single request.
const maxPageSize = 500

// defaultPageSize is used when the caller does not say.
const defaultPageSize = 100

// Allocations returns one page of raw fact rows, newest first.
func (r *ReportRepository) Allocations(ctx context.Context, p AllocationsParams) (AllocationsPage, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}

	args := []any{p.From, p.To}
	where := []string{"window_start >= $1", "window_start < $2"}

	for _, f := range p.Filters.values() {
		col, known := filterColumns[f[0]]
		if !known {
			return AllocationsPage{}, fmt.Errorf("unknown filter %q", f[0])
		}
		args = append(args, f[1])
		where = append(where, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if p.Filters.EstimatedOnly {
		where = append(where, "rate_source = 'fallback'")
	}

	if p.Cursor != nil {
		// THE KEYSET PREDICATE, and the reason this is fast.
		//
		// Postgres compares ROW VALUES lexicographically, so this single expression means
		// "strictly before this position in (window_start, pod_id, container_name) order" --
		// which is exactly the primary key's order, so the planner serves it as an index range
		// scan rather than a filter over rows it had to read first.
		//
		// Writing it out as nested ORs -- (w < $x) OR (w = $x AND p < $y) OR ... -- is
		// equivalent, far easier to get subtly wrong, and the planner does not always recognise
		// it as an index range.
		//
		// The comparison is STRICT (<), not <=, or the cursor row itself would be returned again
		// as the first row of every subsequent page.
		args = append(args, p.Cursor.WindowStart, p.Cursor.PodID, p.Cursor.ContainerName)
		n := len(args)
		where = append(where, fmt.Sprintf(
			"(window_start, pod_id, container_name) < ($%d, $%d, $%d)", n-2, n-1, n))
	}

	// LIMIT is limit+1: one extra row is fetched purely to discover whether a next page exists.
	//
	// The alternative is a separate COUNT(*) over the same predicate, which doubles the work and
	// is racy anyway -- the count can change between the two queries. Fetching one spare row
	// answers the only question a client actually has ("is there more?") for the cost of one row.
	args = append(args, limit+1)

	query := fmt.Sprintf(`
		SELECT window_start, window_end,
		       namespace_name, pod_name, container_name,
		       workload_kind, workload_name, node_name,
		       team, cost_centre, environment,
		       instance_type, capacity_type, zone, qos_class,
		       cpu_millicores_requested, cpu_millicores_used, cpu_millicores_billable,
		       memory_bytes_requested, memory_bytes_used, memory_bytes_billable,
		       cpu_cost, memory_cost, total_cost, rate_source,
		       pod_id
		FROM container_allocations
		WHERE %s
		-- The ORDER BY must match the cursor's key order EXACTLY, including direction, or the
		-- keyset predicate excludes the wrong rows and pages silently skip data.
		ORDER BY window_start DESC, pod_id DESC, container_name DESC
		LIMIT $%d`,
		strings.Join(where, " AND "), len(args))

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return AllocationsPage{}, fmt.Errorf("querying allocations: %w", err)
	}
	defer rows.Close()

	page := AllocationsPage{Rows: []AllocationRow{}}

	// pod ids are scanned but never returned. They are an internal surrogate key with no meaning
	// to a client, and they are needed only to build the next cursor -- so they are kept in a
	// PARALLEL SLICE rather than on the response row.
	//
	// Parallel rather than a single tracked "last" variable, because the spare row fetched below
	// is discarded: a lone variable would end up holding the DISCARDED row's id, and the cursor
	// would skip one row on every page. Indexing keeps that impossible.
	podIDs := []int64{}

	for rows.Next() {
		var row AllocationRow
		var podID int64
		if err := rows.Scan(
			&row.WindowStart, &row.WindowEnd,
			&row.Namespace, &row.PodName, &row.Container,
			&row.WorkloadKind, &row.WorkloadName, &row.Node,
			&row.Team, &row.CostCentre, &row.Environment,
			&row.InstanceType, &row.CapacityType, &row.Zone, &row.QoSClass,
			&row.CPUMillicoresRequested, &row.CPUMillicoresUsed, &row.CPUMillicoresBillable,
			&row.MemoryBytesRequested, &row.MemoryBytesUsed, &row.MemoryBytesBillable,
			&row.CPUCost, &row.MemoryCost, &row.TotalCost, &row.RateSource,
			&podID,
		); err != nil {
			return AllocationsPage{}, fmt.Errorf("scanning allocation row: %w", err)
		}
		page.Rows = append(page.Rows, row)
		podIDs = append(podIDs, podID)
	}
	if err := rows.Err(); err != nil {
		return AllocationsPage{}, fmt.Errorf("iterating allocations: %w", err)
	}

	// The spare row came back, so there IS a next page. Discard it, and issue a cursor pointing
	// at the last row actually being returned -- NOT the discarded one, or the next page would
	// skip a row.
	if len(page.Rows) > limit {
		page.Rows = page.Rows[:limit]
		podIDs = podIDs[:limit]

		last := page.Rows[limit-1]
		encoded, err := Cursor{
			WindowStart:   last.WindowStart,
			PodID:         podIDs[limit-1],
			ContainerName: last.Container,
		}.Encode()
		if err != nil {
			return AllocationsPage{}, err
		}
		page.NextCursor = encoded
	}

	return page, nil
}
