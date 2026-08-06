package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

// InventoryRepository persists the dimension tables: clusters, nodes, namespaces,
// workloads and pods.
//
// WHY EVERY METHOD IS AN UPSERT AND NEVER A PLAIN INSERT
// -----------------------------------------------------
// The collector sees the SAME objects on every cycle. A node that existed five minutes ago
// still exists now. So the operation is not "insert this node", it is "make sure this node
// is recorded and current".
//
// The tempting shape is SELECT-then-INSERT-or-UPDATE. It is wrong, and wrong in a way that
// only shows up under concurrency: between the SELECT and the INSERT another writer can
// create the same row, and the INSERT fails on the unique constraint. With two collector
// replicas, or a retry overlapping its predecessor, that is not rare. Worse, a
// SELECT-then-INSERT needs a transaction and a lock to be safe, and then the locks
// serialise the whole collection cycle.
//
// INSERT ... ON CONFLICT DO UPDATE pushes the race into Postgres, which resolves it
// atomically against the unique index. One statement, one round trip, no lock held by us,
// correct under any amount of concurrency.
type InventoryRepository struct {
	db Querier
}

// NewInventoryRepository returns a repository bound to db, which may be a pool or a
// transaction -- see the Querier doc for why that matters.
func NewInventoryRepository(db Querier) *InventoryRepository {
	return &InventoryRepository{db: db}
}

// UpsertCluster records a cluster and returns its id.
//
// The RETURNING clause is what makes this ONE round trip instead of two. Without it we
// would need a follow-up SELECT to learn the id, doubling the latency of every upsert --
// and on the ON CONFLICT path there is no other way to discover the existing row's id.
func (r *InventoryRepository) UpsertCluster(ctx context.Context, name, provider, region string) (int64, error) {
	// A named constant for the SQL rather than an inline string, so it can be read as SQL
	// and pasted into psql or EXPLAIN unchanged.
	const q = `
		INSERT INTO clusters (name, provider, region)
		VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE
		SET provider   = EXCLUDED.provider,
		    region     = EXCLUDED.region,
		    updated_at = now()
		RETURNING id`

	var id int64
	// $1, $2, $3 are PLACEHOLDERS, not string interpolation. pgx sends the SQL and the
	// arguments as separate protocol messages, so the values are never parsed as SQL and
	// injection is structurally impossible -- not merely escaped. This is why the input
	// validation in the HTTP layer is defence in depth rather than the primary control.
	if err := r.db.QueryRow(ctx, q, name, provider, region).Scan(&id); err != nil {
		return 0, fmt.Errorf("upsert cluster %q: %w", name, err)
	}
	return id, nil
}

// UpsertNode records a node and returns its id.
func (r *InventoryRepository) UpsertNode(ctx context.Context, clusterID int64, n domain.Node) (int64, error) {
	const q = `
		INSERT INTO nodes (
			cluster_id, name, instance_type, region, zone, capacity_type,
			capacity_cpu_millicores, capacity_memory_bytes,
			allocatable_cpu_millicores, allocatable_memory_bytes,
			first_seen_at, last_seen_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now(), now())
		ON CONFLICT (cluster_id, name) DO UPDATE
		SET instance_type              = EXCLUDED.instance_type,
		    region                     = EXCLUDED.region,
		    zone                       = EXCLUDED.zone,
		    capacity_type              = EXCLUDED.capacity_type,
		    capacity_cpu_millicores    = EXCLUDED.capacity_cpu_millicores,
		    capacity_memory_bytes      = EXCLUDED.capacity_memory_bytes,
		    allocatable_cpu_millicores = EXCLUDED.allocatable_cpu_millicores,
		    allocatable_memory_bytes   = EXCLUDED.allocatable_memory_bytes,
		    -- last_seen_at advances; first_seen_at is DELIBERATELY NOT in this list.
		    -- Overwriting it would erase when we first observed the node, and node
		    -- lifetime is what a "how long did we pay for this instance" query needs.
		    last_seen_at               = now()
		RETURNING id`

	var id int64
	err := r.db.QueryRow(ctx, q,
		clusterID, n.Name, n.InstanceType, n.Region, n.Zone, n.CapacityType,
		n.CapacityCPUMillicores, n.CapacityMemoryBytes,
		n.AllocatableCPUMillicores, n.AllocatableMemoryBytes,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert node %q: %w", n.Name, err)
	}
	return id, nil
}

// UpsertNamespace records a namespace and returns its id.
func (r *InventoryRepository) UpsertNamespace(ctx context.Context, clusterID int64, ns domain.Namespace) (int64, error) {
	const q = `
		INSERT INTO namespaces (
			cluster_id, name, team, cost_centre, environment, labels,
			first_seen_at, last_seen_at
		) VALUES ($1, $2, $3, $4, $5, $6, now(), now())
		ON CONFLICT (cluster_id, name) DO UPDATE
		SET team         = EXCLUDED.team,
		    cost_centre  = EXCLUDED.cost_centre,
		    environment  = EXCLUDED.environment,
		    labels       = EXCLUDED.labels,
		    last_seen_at = now()
		RETURNING id`

	// The labels map is marshalled here rather than handed to pgx directly.
	//
	// pgx CAN encode a map[string]string to jsonb, but doing it explicitly means a nil map
	// becomes the literal `{}` rather than SQL NULL -- and the column has a
	// jsonb_typeof(labels) = 'object' CHECK that NULL would violate. Being explicit at the
	// boundary beats relying on a library's nil handling matching a database constraint.
	labels := ns.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		// Unreachable for map[string]string, but returned rather than ignored: a silent
		// fallback here would write empty labels and quietly break cost attribution.
		return 0, fmt.Errorf("marshal labels for namespace %q: %w", ns.Name, err)
	}

	var id int64
	err = r.db.QueryRow(ctx, q,
		clusterID, ns.Name, ns.Team, ns.CostCentre, ns.Environment, labelsJSON,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert namespace %q: %w", ns.Name, err)
	}
	return id, nil
}

// UpsertWorkload records a workload and returns its id.
//
// A zero-value domain.Workload (a bare pod with no controller) is stored as kind=” and
// name=”, which the unique constraint treats as one legitimate row per namespace. That is
// deliberate: bare pods are worth being able to report on, since nothing will recreate
// them and they are often forgotten debugging leftovers.
func (r *InventoryRepository) UpsertWorkload(ctx context.Context, clusterID, namespaceID int64, w domain.Workload) (int64, error) {
	const q = `
		INSERT INTO workloads (cluster_id, namespace_id, kind, name, first_seen_at, last_seen_at)
		VALUES ($1, $2, $3, $4, now(), now())
		ON CONFLICT (cluster_id, namespace_id, kind, name) DO UPDATE
		SET last_seen_at = now()
		RETURNING id`

	var id int64
	if err := r.db.QueryRow(ctx, q, clusterID, namespaceID, w.Kind, w.Name).Scan(&id); err != nil {
		return 0, fmt.Errorf("upsert workload %s/%s: %w", w.Kind, w.Name, err)
	}
	return id, nil
}

// UpsertPodParams carries the foreign keys the caller has already resolved.
//
// A params struct rather than seven positional arguments. Two reasons: at the call site
// `UpsertPodParams{NodeID: nil, WorkloadID: &wid}` says what it means, whereas
// `UpsertPod(ctx, 1, 2, nil, &wid, ...)` does not. And adding a field later is a
// compile-safe change, whereas inserting a parameter silently reorders every existing
// call that happens to have compatible types.
type UpsertPodParams struct {
	ClusterID int64
	Pod       domain.Pod

	NamespaceID int64

	// POINTERS, so that "no workload" and "no node" can be expressed as SQL NULL.
	//
	// A plain int64 zero would be indistinguishable from a real id of 0, and worse, it
	// would violate the foreign key because no row has id 0. The two NULLs mean different
	// things and both are legitimate:
	//   WorkloadID nil -> a bare pod with no controller
	//   NodeID     nil -> Pending, unscheduled; reserves nothing, costs nothing
	WorkloadID *int64
	NodeID     *int64
}

// UpsertPod records a pod and returns its id.
func (r *InventoryRepository) UpsertPod(ctx context.Context, p UpsertPodParams) (int64, error) {
	const q = `
		INSERT INTO pods (
			cluster_id, uid, namespace_id, workload_id, node_id,
			name, qos_class, started_at, first_seen_at, last_seen_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now(), now())
		ON CONFLICT (cluster_id, uid) DO UPDATE
		SET namespace_id = EXCLUDED.namespace_id,
		    -- workload_id and node_id CAN legitimately change after creation: a Pending
		    -- pod acquires a node when it is scheduled. So they are updated.
		    workload_id  = EXCLUDED.workload_id,
		    node_id      = EXCLUDED.node_id,
		    name         = EXCLUDED.name,
		    qos_class    = EXCLUDED.qos_class,
		    started_at   = EXCLUDED.started_at,
		    last_seen_at = now()
		RETURNING id`

	var id int64
	err := r.db.QueryRow(ctx, q,
		p.ClusterID, p.Pod.UID, p.NamespaceID, p.WorkloadID, p.NodeID,
		p.Pod.Name, p.Pod.QoSClass, p.Pod.StartedAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert pod %s/%s: %w", p.Pod.Namespace, p.Pod.Name, err)
	}
	return id, nil
}

// No CountRows here, and an audit removed one -- worth recording, because deleting it removed a
// security test and that needs justifying rather than glossing.
//
// CountRows counted every row in an allow-listed table. It was honestly documented as test-only and
// kept on the reasoning that the allow-list pattern is the answer to a real problem: table and column
// names cannot be parameterised in SQL, so an allow-list is the only safe way to make an identifier
// dynamic. Its test asserted an unknown table name was refused.
//
// That test proved nothing about production, because production never called the method. Meanwhile the
// pattern now has four instances on paths that DO run, each with its own rejection test:
// group_by and sort in CostSummary, the interval unit in Trend, the filter columns in ContainerStats,
// and scope_kind in MonthlyReports. The lesson is carried by code that executes.
//
// A security test on a code path production never takes is not defence in depth; it is a green tick
// with nothing behind it. The rule it demonstrated survives in the four places it matters -- NEVER
// format a caller-supplied identifier into SQL without an allow-list.
