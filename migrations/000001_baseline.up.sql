-- Baseline schema for the Kubernetes Cost Analyzer.
--
--   make migrate-up
--
-- WHY MIGRATIONS AND NOT AN ORM'S AUTO-MIGRATE
-- --------------------------------------------
-- Auto-migrate derives DDL by diffing structs against the live database. It cannot
-- express the things that matter here: it will not write a partitioned table, it cannot
-- know whether to add a column with a default or backfill it, and it has no concept of
-- doing either without locking a large table. Worst of all, what runs against production
-- is computed at deploy time rather than reviewed in a pull request.
--
-- Numbered SQL files are boring and auditable. Every environment applies the same bytes
-- in the same order, and the diff is the review.
--
-- THE ONE RULE: never edit a migration that has been applied anywhere. golang-migrate
-- records the version it has reached; changing an applied file makes the database and
-- the repository silently disagree. Always add a new migration.

-- ============================================================================
-- DIMENSIONS: the things cost is attributed TO.
-- ============================================================================
-- These are NORMALISED, because they are mutable and low-volume. A node's labels can be
-- corrected, a namespace's owner reassigned, and we want one row to update rather than
-- hunting duplicated strings. This is the opposite of the choice made for the fact table
-- below, and the difference is the whole lesson: normalise what changes, denormalise what
-- must never change.

-- clusters exists in Phase 2 despite being single-cluster today.
--
-- WHY NOW: it is the one dimension that is impossible to retrofit cheaply. Adding
-- cluster_id later means backfilling every dimension row AND every fact row, and until
-- the backfill completes every query is ambiguous. As a column present from the start it
-- costs 8 bytes and one join; added in Phase 11 it is a migration nobody wants to run.
CREATE TABLE clusters (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       text        NOT NULL,
    provider   text        NOT NULL DEFAULT 'unknown',
    region     text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- A NATURAL KEY constraint alongside the surrogate primary key. Both earn their
    -- place: the surrogate gives narrow, stable foreign keys, and this unique constraint
    -- is what makes INSERT ... ON CONFLICT (name) work, which is how the collector stays
    -- idempotent without a read-then-write race.
    CONSTRAINT clusters_name_key UNIQUE (name),
    CONSTRAINT clusters_name_not_blank CHECK (name <> '')
);

COMMENT ON TABLE clusters IS 'Clusters we collect from. Single-row today; the column exists so Phase 11 needs no backfill.';

-- nodes: where cost originates. A pod costs money because it occupies part of a node,
-- and the node has a price.
CREATE TABLE nodes (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    cluster_id bigint NOT NULL REFERENCES clusters (id) ON DELETE CASCADE,
    name       text   NOT NULL,

    -- The pricing key. Nullable-as-empty rather than NULL: '' means "we saw the node and
    -- it had no instance-type label" (bare metal), which is different information from
    -- NULL meaning "unknown". Using '' consistently also keeps GROUP BY from splitting
    -- into a NULL bucket that surprises everyone reading a report.
    instance_type text NOT NULL DEFAULT '',
    region        text NOT NULL DEFAULT '',
    zone          text NOT NULL DEFAULT '',
    capacity_type text NOT NULL DEFAULT '',

    -- CAPACITY vs ALLOCATABLE, both stored, because they answer different questions:
    -- you are BILLED for capacity (you rented the whole instance) but pods are SCHEDULED
    -- against allocatable. The gap is the kubelet's reserve, and it is itself waste worth
    -- reporting. Storing only one makes the other permanently unrecoverable.
    capacity_cpu_millicores    bigint NOT NULL,
    capacity_memory_bytes      bigint NOT NULL,
    allocatable_cpu_millicores bigint NOT NULL,
    allocatable_memory_bytes   bigint NOT NULL,

    -- first_seen/last_seen rather than a hard delete.
    --
    -- A node removed by the autoscaler must not vanish from the database: last month's
    -- costs reference it, and a report that cannot name the node it billed for is
    -- unauditable. last_seen_at is what tells us a node is gone.
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT nodes_cluster_name_key UNIQUE (cluster_id, name),
    -- Negative capacity is impossible and would silently produce negative cost.
    CONSTRAINT nodes_capacity_non_negative CHECK (
        capacity_cpu_millicores >= 0 AND capacity_memory_bytes >= 0
        AND allocatable_cpu_millicores >= 0 AND allocatable_memory_bytes >= 0
    )
);

COMMENT ON COLUMN nodes.capacity_cpu_millicores IS 'What the instance has. BILL THIS.';
COMMENT ON COLUMN nodes.allocatable_cpu_millicores IS 'What the scheduler may hand out. Compute UTILISATION against this.';

-- namespaces: the primary cost-allocation dimension.
CREATE TABLE namespaces (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    cluster_id bigint NOT NULL REFERENCES clusters (id) ON DELETE CASCADE,
    name       text   NOT NULL,

    team        text NOT NULL DEFAULT '',
    cost_centre text NOT NULL DEFAULT '',
    environment text NOT NULL DEFAULT '',

    -- The full label set as jsonb, so a dimension we did not anticipate can still be
    -- grouped by without a schema change.
    --
    -- jsonb NOT json: jsonb is parsed and stored in a binary form, so it can be indexed
    -- with GIN and queried with containment operators. json keeps the original text
    -- (including whitespace and duplicate keys) and must be re-parsed on every access.
    -- There is essentially no reason to choose json for data you intend to query.
    labels jsonb NOT NULL DEFAULT '{}'::jsonb,

    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT namespaces_cluster_name_key UNIQUE (cluster_id, name),
    -- Guard against a labels value that is a scalar or array rather than an object; the
    -- application would then read garbage from ->> lookups.
    CONSTRAINT namespaces_labels_is_object CHECK (jsonb_typeof(labels) = 'object')
);

-- workloads: the controller that owns pods, and the level cost is REPORTED at.
--
-- Pods are cattle. A Deployment rollout replaces every pod UID, so per-pod history
-- fragments on every deploy. The Deployment is what persists and what a team calls
-- "their service".
CREATE TABLE workloads (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    cluster_id   bigint NOT NULL REFERENCES clusters (id) ON DELETE CASCADE,
    namespace_id bigint NOT NULL REFERENCES namespaces (id) ON DELETE CASCADE,

    -- '' for a bare pod with no controller. Legitimate but unusual, and worth being able
    -- to report on: nothing will recreate it, so it is often a forgotten debugging
    -- leftover that has been billing for months.
    kind text NOT NULL DEFAULT '',
    name text NOT NULL DEFAULT '',

    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),

    -- namespace_id is in the key as well as cluster_id: two namespaces may each have a
    -- Deployment called "api", and they are different workloads.
    CONSTRAINT workloads_identity_key UNIQUE (cluster_id, namespace_id, kind, name)
);

-- pods: the scheduling unit. Kept as a dimension so a fact row can point at one narrow
-- id instead of repeating the pod's UID, name and node on every sample.
CREATE TABLE pods (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    cluster_id bigint NOT NULL REFERENCES clusters (id) ON DELETE CASCADE,

    -- The Kubernetes UID is the real identity, NOT the name.
    --
    -- Names are reused: delete a StatefulSet pod and its replacement has the identical
    -- name but is a different pod with a different lifetime. Keying on name would merge
    -- two pods' cost into one series and hide a crashlooping replica entirely.
    uid text NOT NULL,

    namespace_id bigint NOT NULL REFERENCES namespaces (id) ON DELETE CASCADE,

    -- NULLABLE, deliberately, and the two NULLs mean different things:
    --   workload_id NULL -> a bare pod with no controller
    --   node_id     NULL -> Pending, not yet scheduled; reserves nothing, costs nothing
    -- A NOT NULL constraint here would force the collector to invent a placeholder row,
    -- and every query would then have to know the placeholder's name.
    workload_id bigint REFERENCES workloads (id) ON DELETE SET NULL,
    node_id     bigint REFERENCES nodes (id) ON DELETE SET NULL,

    name      text NOT NULL,
    qos_class text NOT NULL DEFAULT '',

    started_at    timestamptz,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT pods_cluster_uid_key UNIQUE (cluster_id, uid),
    CONSTRAINT pods_qos_valid CHECK (qos_class IN ('', 'Guaranteed', 'Burstable', 'BestEffort'))
);

-- Reporting access paths on the dimensions. Each is here because a specific query needs
-- it, not for completeness: every index is write amplification and storage, so an index
-- without a query is pure cost.
CREATE INDEX nodes_cluster_instance_type_idx ON nodes (cluster_id, instance_type);
CREATE INDEX namespaces_team_idx ON namespaces (cluster_id, team) WHERE team <> '';
CREATE INDEX pods_namespace_idx ON pods (namespace_id);
CREATE INDEX pods_workload_idx ON pods (workload_id) WHERE workload_id IS NOT NULL;
CREATE INDEX pods_node_idx ON pods (node_id) WHERE node_id IS NOT NULL;
-- A GIN index over jsonb supports "which namespaces carry label X?" without a table
-- scan. jsonb_path_ops is smaller and faster than the default for containment queries,
-- at the cost of not supporting key-existence-only lookups.
CREATE INDEX namespaces_labels_idx ON namespaces USING gin (labels jsonb_path_ops);

-- ============================================================================
-- THE FACT TABLE
-- ============================================================================
-- One row per CONTAINER per time window. Everything the product reports is an
-- aggregation over this table.
--
-- WHY CONTAINER GRAIN AND NOT POD GRAIN
-- A pod with a bloated sidecar and a correctly sized application container looks
-- perfectly fine at pod grain -- the totals average out. That is the most common real
-- waste pattern on any cluster running a service mesh, and it is invisible unless the
-- grain is per container. Pod and workload figures are SUMs over this table; the reverse
-- is not recoverable.
--
-- WHY THE ATTRIBUTION COLUMNS ARE DENORMALISED HERE
-- This looks like a normalisation failure and is the opposite. A fact row is an
-- IMMUTABLE HISTORICAL STATEMENT: "between 09:00 and 09:05, this container, owned by the
-- payments team, reserved 500 millicores at this rate."
--
-- If team came from a join to namespaces, then relabelling team-search to
-- team-discovery would silently rewrite every historical report -- last month's
-- finalised, reconciled figure would change after the fact. Denormalising the attribution
-- makes reports reproducible forever, and it removes four joins from every query the
-- dashboard runs.
--
-- The rule: normalise mutable current state, denormalise immutable history.
CREATE TABLE container_allocations (
    -- The window this sample covers. Half-open [window_start, window_end).
    --
    -- Half-open intervals are not a detail: with closed intervals, consecutive windows
    -- share an endpoint and any range query double-counts the boundary. Every
    -- time-bucketing bug in reporting traces back to this.
    window_start timestamptz NOT NULL,
    window_end   timestamptz NOT NULL,

    pod_id         bigint NOT NULL REFERENCES pods (id) ON DELETE CASCADE,
    container_name text   NOT NULL,

    -- Denormalised attribution: an immutable snapshot of ownership at collection time.
    cluster_name   text NOT NULL,
    namespace_name text NOT NULL,
    pod_name       text NOT NULL,
    workload_kind  text NOT NULL DEFAULT '',
    workload_name  text NOT NULL DEFAULT '',
    node_name      text NOT NULL DEFAULT '',
    team           text NOT NULL DEFAULT '',
    cost_centre    text NOT NULL DEFAULT '',
    environment    text NOT NULL DEFAULT '',
    instance_type  text NOT NULL DEFAULT '',
    capacity_type  text NOT NULL DEFAULT '',
    zone           text NOT NULL DEFAULT '',
    qos_class      text NOT NULL DEFAULT '',

    -- MEASUREMENTS, in the same integer units internal/kube uses. Integers, never floats:
    -- these are summed across millions of rows and reconciled against an invoice, and
    -- accumulated binary floating point error in a number finance checks is a
    -- credibility problem rather than a rounding curiosity.
    cpu_millicores_requested bigint NOT NULL DEFAULT 0,
    memory_bytes_requested   bigint NOT NULL DEFAULT 0,
    cpu_millicores_used      bigint NOT NULL DEFAULT 0,
    memory_bytes_used        bigint NOT NULL DEFAULT 0,

    -- BILLABLE quantities: max(requested, used).
    --
    -- Stored rather than computed at query time because it is the definition of the
    -- product and must not drift between the three places that would otherwise
    -- reimplement it. It is also why the no-requests-at-all fixture exists: billing on
    -- request alone reports that pod as free and smears its real cost across every other
    -- team.
    cpu_millicores_billable bigint NOT NULL DEFAULT 0,
    memory_bytes_billable   bigint NOT NULL DEFAULT 0,

    -- RATES USED AT THIS MOMENT, denormalised for the same reason as attribution: spot
    -- prices move hourly, and a fact row that joined to a live price table would produce
    -- a different answer every time an old report was re-run.
    --
    -- numeric, not double precision. numeric is exact decimal arithmetic; float8 cannot
    -- represent 0.1 and its errors accumulate under SUM. For money this is not
    -- negotiable. The cost is slower arithmetic, which is irrelevant next to the I/O of
    -- reading these rows.
    cpu_cost_per_core_hour   numeric(20, 10) NOT NULL DEFAULT 0,
    memory_cost_per_gib_hour numeric(20, 10) NOT NULL DEFAULT 0,

    cpu_cost    numeric(20, 10) NOT NULL DEFAULT 0,
    memory_cost numeric(20, 10) NOT NULL DEFAULT 0,

    -- A GENERATED column, so the total can never disagree with its parts. Computing it
    -- in application code means every writer must remember; computing it at query time
    -- means every reader must. STORED rather than VIRTUAL because Postgres only supports
    -- STORED, and because it can then be indexed.
    total_cost numeric(20, 10) GENERATED ALWAYS AS (cpu_cost + memory_cost) STORED,

    -- When the collector wrote this. Distinct from window_start: a backfill run today
    -- writes rows for last week, and telling the two apart is what makes a late-arriving
    -- correction explicable rather than mysterious.
    collected_at timestamptz NOT NULL DEFAULT now(),

    -- THE PRIMARY KEY MUST INCLUDE THE PARTITION KEY.
    --
    -- Postgres cannot enforce uniqueness across partitions without it: each partition has
    -- its own index, and a global unique constraint would require checking every one.
    -- So window_start is part of the key whether or not we wanted it there. This single
    -- constraint shapes the entire design of a partitioned table, and it is the first
    -- thing people hit when they try to partition an existing table.
    --
    -- It also gives us IDEMPOTENCY: the collector can re-run a window after a crash and
    -- ON CONFLICT updates the row instead of double-counting. A bare INSERT here would
    -- silently inflate the bill on every retry.
    CONSTRAINT container_allocations_pkey PRIMARY KEY (window_start, pod_id, container_name),

    -- A zero-length or reversed window would make every rate calculation divide by zero
    -- or go negative.
    CONSTRAINT container_allocations_window_ordered CHECK (window_end > window_start),
    CONSTRAINT container_allocations_amounts_non_negative CHECK (
        cpu_millicores_requested >= 0 AND memory_bytes_requested >= 0
        AND cpu_millicores_used >= 0 AND memory_bytes_used >= 0
        AND cpu_millicores_billable >= 0 AND memory_bytes_billable >= 0
        AND cpu_cost >= 0 AND memory_cost >= 0
    ),
    -- Billable must be at least the larger of requested and used, or the max(request,
    -- usage) rule has been broken by whoever wrote the row.
    CONSTRAINT container_allocations_billable_is_max CHECK (
        cpu_millicores_billable >= GREATEST(cpu_millicores_requested, cpu_millicores_used)
        AND memory_bytes_billable >= GREATEST(memory_bytes_requested, memory_bytes_used)
    )
) PARTITION BY RANGE (window_start);

COMMENT ON TABLE container_allocations IS
    'Immutable per-container cost samples. Attribution and rates are denormalised so historical reports are reproducible.';

-- ============================================================================
-- PARTITIONS
-- ============================================================================
-- WHY PARTITION AT ALL
-- At 5,000 pods with two containers each and a 5-minute window, this table takes
-- ~86 million rows a month. Two consequences:
--
--   RETENTION. Deleting a month with DELETE rewrites the table's visibility map, leaves
--   the space occupied until VACUUM, and takes hours. DROP TABLE on a partition is
--   instant and returns the disk immediately.
--
--   QUERY PRUNING. A report for August reads only the August partition. The planner
--   excludes the others before touching them, so scan cost stays flat as history grows
--   rather than degrading every month.

-- A function rather than fifteen hand-written CREATE TABLE statements.
--
-- The collector cannot write a row whose window falls outside every partition -- the
-- insert fails outright -- so partitions must exist BEFORE the data does. Phase 7's
-- scheduler will call this ahead of time; for now the migration seeds a fixed range.
CREATE OR REPLACE FUNCTION ensure_allocation_partition(target_month date)
RETURNS text
LANGUAGE plpgsql
AS $$
DECLARE
    start_at   date := date_trunc('month', target_month)::date;
    end_at     date := (date_trunc('month', target_month) + interval '1 month')::date;
    part_name  text := format('container_allocations_%s', to_char(start_at, 'YYYY_MM'));
BEGIN
    -- format() with %I and %L quotes identifiers and literals correctly. Building DDL by
    -- concatenating strings is how SQL injection reaches a maintenance job.
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF container_allocations FOR VALUES FROM (%L) TO (%L)',
        part_name, start_at, end_at
    );
    RETURN part_name;
END;
$$;

COMMENT ON FUNCTION ensure_allocation_partition IS
    'Idempotently create the monthly partition covering target_month. Must run BEFORE data for that month arrives.';

-- Seed an explicit, DETERMINISTIC range rather than one derived from now().
--
-- A migration whose result depends on when it runs is not reproducible: replaying it on a
-- fresh database next year would produce different partitions from production's, and the
-- schema would no longer be a function of the migration history.
DO $$
DECLARE
    m date;
BEGIN
    FOR m IN
        SELECT generate_series('2026-01-01'::date, '2027-12-01'::date, '1 month'::interval)::date
    LOOP
        PERFORM ensure_allocation_partition(m);
    END LOOP;
END;
$$;

-- A DEFAULT partition catches anything outside the seeded range.
--
-- THE TRADE-OFF, STATED PLAINLY: without it, a row for an unseeded month fails to insert
-- and the collector loses data. With it, the row lands here silently -- and a partition
-- for that month can then no longer be created without first moving those rows out,
-- because Postgres must prove no existing default row belongs in the new partition.
--
-- Losing data is worse than needing maintenance, so the default partition stays. It
-- should be monitored: rows here mean partition creation has fallen behind.
CREATE TABLE container_allocations_default PARTITION OF container_allocations DEFAULT;

-- ============================================================================
-- FACT TABLE INDEXES
-- ============================================================================
-- Declared on the PARENT, so Postgres creates and maintains a matching index on every
-- partition, including ones added later. Indexing partitions individually is a
-- maintenance burden that will be forgotten exactly once.
--
-- Every index below leads with its GROUPING column and ends with window_start, because
-- every report filters a time range and groups by a dimension. An index on
-- (window_start, team) could not serve "team X over a range" nearly as well: the
-- leading column must be the equality predicate.

-- "what does this namespace cost over time" -- the dashboard's primary query
CREATE INDEX container_allocations_namespace_window_idx
    ON container_allocations (namespace_name, window_start DESC);

-- "what does this team cost" -- the finance question. Partial, because rows with no team
-- label cannot be grouped by team and would only bloat the index.
CREATE INDEX container_allocations_team_window_idx
    ON container_allocations (team, window_start DESC) WHERE team <> '';

-- "what does this workload cost" -- the engineer's question
CREATE INDEX container_allocations_workload_window_idx
    ON container_allocations (workload_kind, workload_name, window_start DESC)
    WHERE workload_name <> '';

-- "what is this node earning us" -- node utilisation and bin-packing waste
CREATE INDEX container_allocations_node_window_idx
    ON container_allocations (node_name, window_start DESC) WHERE node_name <> '';

-- Supports the per-pod lookup that does not lead with window_start, which the primary
-- key cannot serve.
CREATE INDEX container_allocations_pod_idx ON container_allocations (pod_id, window_start DESC);
