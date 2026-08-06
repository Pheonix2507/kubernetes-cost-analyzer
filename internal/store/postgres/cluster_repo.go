package postgres

import (
	"context"
	"fmt"
	"time"
)

// ClusterRepository serves the read side of the clusters dimension.
//
// SEPARATE FROM InventoryRepository.UpsertCluster FOR THE SAME REASON ReportRepository IS SEPARATE
// FROM AllocationRepository: that one writes, this one reads. The writer is called by the collector
// once per cycle inside a transaction with a dozen other upserts; this is called by the API to
// answer a request. They share a table and nothing else.
type ClusterRepository struct {
	db Querier
}

// NewClusterRepository returns a repository bound to db.
func NewClusterRepository(db Querier) *ClusterRepository {
	return &ClusterRepository{db: db}
}

// ClusterRow is one cluster and everything recorded about it.
type ClusterRow struct {
	Name     string
	Provider string
	Region   string
	Account  string
	Currency string
	// FirstSeen and LastSeen come from created_at and updated_at. LastSeen is the useful one
	// operationally: a cluster whose row has not been touched for hours has a collector that is
	// no longer reporting, and that is invisible from the cost figures alone -- they simply stop
	// growing, which looks identical to a cluster that got cheaper.
	FirstSeen time.Time
	LastSeen  time.Time
}

// ListClusters returns every known cluster, ordered by name.
//
// Ordered by name rather than by id or by cost, and deliberately: this list populates a selector in
// a UI, where a stable alphabetical order means the same cluster is in the same place every time.
// Ordering by cost would reshuffle the list as the numbers moved, which is precisely the wrong
// behaviour for something a human aims a mouse at.
//
// Unpaginated, because the row count is the number of clusters an organisation runs. If that ever
// reaches a thousand, pagination is the least of the problems.
func (r *ClusterRepository) ListClusters(ctx context.Context) ([]ClusterRow, error) {
	const q = `
		SELECT name, provider, region, account, currency, created_at, updated_at
		FROM clusters
		ORDER BY name`

	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("listing clusters: %w", err)
	}
	defer rows.Close()

	out := make([]ClusterRow, 0, 8)
	for rows.Next() {
		var c ClusterRow
		if err := rows.Scan(
			&c.Name, &c.Provider, &c.Region, &c.Account, &c.Currency, &c.FirstSeen, &c.LastSeen,
		); err != nil {
			return nil, fmt.Errorf("scanning cluster: %w", err)
		}
		out = append(out, c)
	}
	// rows.Err is checked because rows.Next returning false is ambiguous: it means either "no more
	// rows" or "the query failed part way through". Without this, a connection dropped mid-result
	// returns a SHORT LIST and no error, and the caller reports a fleet with clusters missing.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clusters: %w", err)
	}
	return out, nil
}
