package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

// AllocationRepository writes and reads the fact table.
type AllocationRepository struct {
	db Querier
}

// NewAllocationRepository returns a repository bound to db.
func NewAllocationRepository(db Querier) *AllocationRepository {
	return &AllocationRepository{db: db}
}

// insertAllocation is shared by the single and batch paths so the column list and the
// conflict behaviour can never diverge between them.
//
// ON CONFLICT DO UPDATE is what makes collection IDEMPOTENT. The collector runs on a
// timer; if it crashes after writing half a window and then retries, a bare INSERT would
// fail on the primary key -- or worse, if the key were incomplete, would insert duplicates
// and inflate the bill on every retry. Re-running a window must be safe and must converge
// on the same numbers.
//
// total_cost is absent from the column list because it is a GENERATED column: Postgres
// rejects any attempt to write it, which is precisely the guarantee we want.
const insertAllocation = `
	INSERT INTO container_allocations (
		window_start, window_end, pod_id, container_name,
		cluster_name, namespace_name, pod_name, workload_kind, workload_name,
		node_name, team, cost_centre, environment, instance_type, capacity_type,
		zone, qos_class, rate_source,
		cpu_millicores_requested, memory_bytes_requested,
		cpu_millicores_used, memory_bytes_used,
		cpu_millicores_max, memory_bytes_max,
		cpu_millicores_billable, memory_bytes_billable,
		cpu_cost_per_core_hour, memory_cost_per_gib_hour,
		cpu_cost, memory_cost, collected_at
	) VALUES (
		$1, $2, $3, $4,
		$5, $6, $7, $8, $9,
		$10, $11, $12, $13, $14, $15,
		$16, $17, $18,
		$19, $20,
		$21, $22,
		$23, $24,
		$25, $26,
		$27, $28,
		$29, $30, now()
	)
	ON CONFLICT (window_start, pod_id, container_name) DO UPDATE
	SET cluster_name             = EXCLUDED.cluster_name,
	    namespace_name           = EXCLUDED.namespace_name,
	    pod_name                 = EXCLUDED.pod_name,
	    workload_kind            = EXCLUDED.workload_kind,
	    workload_name            = EXCLUDED.workload_name,
	    node_name                = EXCLUDED.node_name,
	    team                     = EXCLUDED.team,
	    cost_centre              = EXCLUDED.cost_centre,
	    environment              = EXCLUDED.environment,
	    instance_type            = EXCLUDED.instance_type,
	    capacity_type            = EXCLUDED.capacity_type,
	    zone                     = EXCLUDED.zone,
	    qos_class                = EXCLUDED.qos_class,
	    rate_source              = EXCLUDED.rate_source,
	    window_end               = EXCLUDED.window_end,
	    cpu_millicores_requested = EXCLUDED.cpu_millicores_requested,
	    memory_bytes_requested   = EXCLUDED.memory_bytes_requested,
	    cpu_millicores_used      = EXCLUDED.cpu_millicores_used,
	    memory_bytes_used        = EXCLUDED.memory_bytes_used,
	    cpu_millicores_max       = EXCLUDED.cpu_millicores_max,
	    memory_bytes_max         = EXCLUDED.memory_bytes_max,
	    cpu_millicores_billable  = EXCLUDED.cpu_millicores_billable,
	    memory_bytes_billable    = EXCLUDED.memory_bytes_billable,
	    cpu_cost_per_core_hour   = EXCLUDED.cpu_cost_per_core_hour,
	    memory_cost_per_gib_hour = EXCLUDED.memory_cost_per_gib_hour,
	    cpu_cost                 = EXCLUDED.cpu_cost,
	    memory_cost              = EXCLUDED.memory_cost,
	    collected_at             = now()`

// allocationArgs flattens one allocation into the placeholder order above.
func allocationArgs(a domain.ContainerAllocation) []any {
	cpuBillable, memBillable := a.Billable()
	return []any{
		a.WindowStart, a.WindowEnd, a.PodID, a.ContainerName,
		a.ClusterName, a.NamespaceName, a.PodName, a.WorkloadKind, a.WorkloadName,
		a.NodeName, a.Team, a.CostCentre, a.Environment, a.InstanceType, a.CapacityType,
		a.Zone, a.QoSClass, a.RateSource,
		a.CPUMillicoresRequested, a.MemoryBytesRequested,
		a.CPUMillicoresUsed, a.MemoryBytesUsed,
		a.CPUMillicoresMax, a.MemoryBytesMax,
		cpuBillable, memBillable,
		a.CPUCostPerCoreHour, a.MemoryCostPerGiBHour,
		a.CPUCost, a.MemoryCost,
	}
}

// Insert writes a single allocation. Mostly for tests; the collector uses InsertBatch.
func (r *AllocationRepository) Insert(ctx context.Context, a domain.ContainerAllocation) error {
	if err := a.Validate(); err != nil {
		return fmt.Errorf("invalid allocation for pod %d container %q: %w", a.PodID, a.ContainerName, err)
	}
	if _, err := r.db.Exec(ctx, insertAllocation, allocationArgs(a)...); err != nil {
		return fmt.Errorf("insert allocation for pod %d container %q: %w", a.PodID, a.ContainerName, err)
	}
	return nil
}

// InsertBatch writes many allocations in a single network round trip.
//
// WHY BATCHING RATHER THAN A LOOP OVER Insert
// -------------------------------------------
// Each Exec is a full round trip. At 0.5ms of latency and 10,000 containers that is five
// seconds of pure waiting before Postgres does any work at all, every collection cycle.
// pgx.Batch pipelines the statements and reads the results back in order, so the latency
// is paid once.
//
// WHY NOT CopyFrom, WHICH IS FASTER STILL
// COPY cannot express ON CONFLICT, and idempotency is not negotiable here -- a retried
// window must converge rather than duplicate. The standard way to get both is COPY into a
// temporary table followed by INSERT ... SELECT ... ON CONFLICT, which is worth doing when
// profiling says this is the bottleneck. It is not yet, and the complexity would be
// unjustified.
//
// EVERY allocation is validated BEFORE anything is sent. One malformed row inside a batch
// aborts the enclosing transaction, taking every valid row with it, so failing early with a
// precise message beats discovering it halfway through.
func (r *AllocationRepository) InsertBatch(ctx context.Context, allocations []domain.ContainerAllocation) error {
	if len(allocations) == 0 {
		// Not an error. An empty cluster, or a window where every pod was Pending, is a
		// legitimate outcome and the caller should not have to special-case it.
		return nil
	}

	for i, a := range allocations {
		if err := a.Validate(); err != nil {
			return fmt.Errorf("allocation %d of %d (pod %d container %q) is invalid: %w",
				i, len(allocations), a.PodID, a.ContainerName, err)
		}
	}

	batch := &pgx.Batch{}
	for _, a := range allocations {
		batch.Queue(insertAllocation, allocationArgs(a)...)
	}

	results := r.db.SendBatch(ctx, batch)
	// Close MUST be called, and its error MUST be checked.
	//
	// pgx reads batch results lazily. Abandoning them without Close leaves unread
	// responses in the connection's buffer, and the connection is then returned to the
	// pool in a corrupt state -- the NEXT user of that connection reads our leftover
	// results and fails with a wildly misleading error. A defer alone is not enough
	// either, because a deferred Close's error would be discarded.
	var firstErr error
	for i := range allocations {
		if _, err := results.Exec(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("batch item %d (pod %d container %q): %w",
				i, allocations[i].PodID, allocations[i].ContainerName, err)
		}
	}
	if closeErr := results.Close(); closeErr != nil && firstErr == nil {
		firstErr = fmt.Errorf("close batch: %w", closeErr)
	}
	if firstErr != nil {
		return fmt.Errorf("insert %d allocations: %w", len(allocations), firstErr)
	}
	return nil
}
