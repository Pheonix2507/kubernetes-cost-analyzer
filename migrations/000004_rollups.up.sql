-- Daily rollups and monthly reports: the tables that make history affordable to read.
--
-- WHY A ROLLUP EXISTS AT ALL
-- --------------------------
-- Measured on this database before writing this migration: 74,925 fact rows covering 8 days of a
-- 23-container cluster, and a "cost by namespace over a year" query read 3,372 buffers (~26 MB) to
-- produce SIX rows. That is not a badly written query. It is a query asking a question at 288x lower
-- resolution than the data is stored at.
--
-- Projected to a real cluster -- 5,000 containers at 288 five-minute windows a day:
--
--     1.44 M rows/day       43 M rows/month       525 M rows/year
--
-- A dashboard drawing a twelve-point annual trend would scan half a billion rows. The fix is not a
-- better index; no index turns 525 M rows into 12 without pre-aggregating them.
--
-- Measured compression at the exact grain below, using the real data rather than a convenient
-- simplification: 74,925 rows -> 256 rows, or 292.7x. That number is honest because the key keeps
-- EVERY dimension the summary endpoint can group by except pod.
--
-- THE RULE THAT DECIDES THIS ENTIRE DESIGN: NOT EVERY AGGREGATE ROLLS UP
-- ---------------------------------------------------------------------
--     sum        rolls up -- the sum of sums is the sum
--     max, min   roll up  -- the max of maxes is the max
--     count      rolls up -- it is a sum
--     avg        rolls up ONLY as sum / count. Averaging averages is wrong unless every count is
--                equal, and window counts are never equal once a container starts or stops mid-day.
--     percentile DOES NOT ROLL UP. p95 of daily p95s is not the p95 of the underlying data, and no
--                amount of care fixes that -- it needs a mergeable sketch (t-digest, HDR histogram).
--
-- Two consequences are designed in rather than discovered later:
--
--   1. This table stores CORE-HOURS, not millicores. A core-hour is a sum, so it re-aggregates
--      correctly at any grain. Storing avg_millicores would make a monthly average weight a
--      two-hour day identically to a twenty-four-hour one.
--
--   2. The recommendation engine KEEPS READING THE FACT TABLE, because its p95 cannot come from
--      here. That is an architectural boundary, not an omission -- and it is why raw-fact retention
--      must always exceed the recommendation window.

-- ============================================================================
-- container_allocations_daily
-- ============================================================================

-- WHY THIS IS NOT PARTITIONED, WHEN THE FACT TABLE IS
-- --------------------------------------------------
-- The fact table is partitioned so old raw data can be DROPped in O(1) instead of deleted row by
-- row. This table is the thing you KEEP when you drop those partitions -- that is its entire purpose.
-- Partitioning a table nobody intends to drop buys nothing and costs real complexity: every primary
-- key must carry the partition key, and every query that omits it scans every partition.
--
-- The volume supports that: 5,000 containers x 365 days is 1.8 M rows a year at this grain. Postgres
-- is comfortable into the tens of millions. Revisit at roughly 100 M rows -- five decades of a
-- 5,000-container cluster, or one year of a 250,000-container estate.
--
-- WHY cluster_name AND NOT cluster_id
-- The fact table denormalises cluster_name and has no cluster_id, so keying on the name keeps this
-- table a PURE PROJECTION of the fact table -- the rollup is one INSERT ... SELECT with no join.
-- It is also the right call on its own terms: migration 000001 draws the line at "normalise what
-- changes, denormalise what must never change", and a historical cost record must never change.
CREATE TABLE container_allocations_daily (
    -- A surrogate key rather than the twelve-column natural key, and that choice follows from the
    -- write strategy below: DELETE-then-INSERT needs no unique index on the dimensions, so paying
    -- for a twelve-column btree -- which would likely exceed the table itself -- would buy nothing.
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- THE DAY IS A DATE IN UTC, AND THAT IS A DECISION WITH TEETH.
    --
    -- date_trunc('day', ts) depends entirely on the session timezone, so the same fact rows bucket
    -- differently for different readers. Fixing it at UTC makes the rollup deterministic and makes
    -- re-running it reproduce byte-identical rows.
    --
    -- THE COST, STATED PLAINLY: once history is aggregated to UTC days, a report aligned to any
    -- other timezone can no longer be produced exactly from this table. An IST month boundary falls
    -- at 18:30 UTC the previous day, so an IST-aligned month is 30 UTC days plus two half-days that
    -- this table has already merged into their neighbours. That is unrecoverable here -- the fact
    -- table is the only place that can answer it, which is another reason retention matters.
    --
    -- The alternative is storing the rollup per reporting timezone, which multiplies the table by
    -- the number of timezones and makes "the" cost of a day ambiguous. One canonical grain, one
    -- documented limitation.
    day date NOT NULL,

    -- The dimensions, denormalised exactly as the fact table has them. Identical names on purpose:
    -- the rollup query is a straight projection, and a renamed column is a bug waiting for someone
    -- to write the obvious SELECT.
    cluster_name   text NOT NULL,
    namespace_name text NOT NULL,
    team           text NOT NULL DEFAULT '',
    cost_centre    text NOT NULL DEFAULT '',
    environment    text NOT NULL DEFAULT '',
    workload_kind  text NOT NULL DEFAULT '',
    workload_name  text NOT NULL DEFAULT '',
    node_name      text NOT NULL DEFAULT '',
    instance_type  text NOT NULL DEFAULT '',
    capacity_type  text NOT NULL DEFAULT '',
    container_name text NOT NULL,

    -- POD NAME IS DELIBERATELY ABSENT, and it is the only groupable dimension dropped.
    --
    -- Keeping it costs 1.6x of the compression (measured: 253x with pods, 293x without) which is not
    -- the real argument. The real argument is that a pod name is not a stable identity: a Deployment
    -- rollout replaces every pod, so a per-pod daily series fragments on every deploy and trends
    -- across it are meaningless. The workload is what persists and what a team calls "their service".
    --
    -- Per-pod detail is not lost, it lives where it belongs: /api/v1/allocations reads the fact
    -- table. A trend grouped by pod therefore routes to the fact table instead of here -- see
    -- rollup_repo.go, where that routing is explicit rather than a silent fallback.

    -- MEASURES.
    --
    -- All sums, all re-aggregatable. window_count is the denominator that lets any average be
    -- recomputed correctly at any grain, which is the whole reason an average is not stored.
    window_count     integer NOT NULL,
    -- observed_seconds is the actual measured wall-clock time, summed over windows. Not derivable
    -- from window_count x interval, because the interval is configurable and has changed before.
    -- This is what makes coverage checkable rather than assumed.
    observed_seconds bigint NOT NULL,

    cpu_requested_core_hours   numeric(30,10) NOT NULL,
    cpu_used_core_hours        numeric(30,10) NOT NULL,
    cpu_billable_core_hours    numeric(30,10) NOT NULL,
    -- Waste is summed from PER-ROW floored values, never computed as requested - used at this grain.
    -- Doing the subtraction here would resurrect exactly the bug fixed in Phase 6: an under-requested
    -- container issues a credit against real waste in the same group, and on real data that reported
    -- kube-system as having ZERO memory waste while it held 50 GiB-hours.
    wasted_cpu_core_hours      numeric(30,10) NOT NULL,

    memory_requested_gib_hours numeric(30,10) NOT NULL,
    memory_used_gib_hours      numeric(30,10) NOT NULL,
    memory_billable_gib_hours  numeric(30,10) NOT NULL,
    wasted_memory_gib_hours    numeric(30,10) NOT NULL,

    cpu_cost    numeric(20,10) NOT NULL,
    memory_cost numeric(20,10) NOT NULL,
    -- Generated, exactly as on the fact table, so the two can never disagree about what a total is.
    total_cost  numeric(20,10) GENERATED ALWAYS AS (cpu_cost + memory_cost) STORED,

    -- Peaks roll up (max of maxes is the max), so they are kept. p95 is NOT kept, because it cannot
    -- be rolled up -- see the header. A caller wanting a percentile must query the fact table.
    cpu_millicores_max bigint NOT NULL DEFAULT 0,
    memory_bytes_max   bigint NOT NULL DEFAULT 0,

    -- Provenance survives aggregation. bool_or, not a count: the question is whether ANY rate behind
    -- this figure was a fallback guess, and aggregating that away would waste the point of recording
    -- rate_source at all.
    estimated_rates boolean NOT NULL DEFAULT false,

    -- When this row was computed. Distinct from `day`, and useful precisely when they disagree: a
    -- rolled_up_at long after its day means the row came from a backfill rather than the nightly run.
    rolled_up_at timestamptz NOT NULL DEFAULT now(),

    -- Non-negative, for the same reason the fact table checks it: a negative core-hour is a bug in
    -- the rollup, and it would silently subtract from whatever it is summed into.
    CONSTRAINT container_allocations_daily_non_negative CHECK (
        window_count > 0
        AND observed_seconds >= 0
        AND cpu_requested_core_hours >= 0 AND cpu_used_core_hours >= 0
        AND cpu_billable_core_hours >= 0 AND wasted_cpu_core_hours >= 0
        AND memory_requested_gib_hours >= 0 AND memory_used_gib_hours >= 0
        AND memory_billable_gib_hours >= 0 AND wasted_memory_gib_hours >= 0
        AND cpu_cost >= 0 AND memory_cost >= 0
    ),
    -- Billable is max(requested, used) at every grain, so it can never be below either. Same
    -- invariant as the fact table, restated here because a rollup bug would otherwise be invisible.
    CONSTRAINT container_allocations_daily_billable_is_max CHECK (
        cpu_billable_core_hours >= cpu_requested_core_hours
        AND cpu_billable_core_hours >= cpu_used_core_hours
        AND memory_billable_gib_hours >= memory_requested_gib_hours
        AND memory_billable_gib_hours >= memory_used_gib_hours
    )
);

COMMENT ON TABLE container_allocations_daily IS
    'Daily rollup at container-within-workload grain. ~293x smaller than the fact table. Sums and maxima only: percentiles do not roll up.';
COMMENT ON COLUMN container_allocations_daily.day IS
    'UTC calendar day. Reports in other timezones cannot be produced exactly from this table -- use the fact table.';
COMMENT ON COLUMN container_allocations_daily.window_count IS
    'Windows aggregated. The denominator for recomputing any average at any grain -- which is why no average is stored.';
COMMENT ON COLUMN container_allocations_daily.rolled_up_at IS
    'When computed. Far from `day` means this row came from a backfill rather than the nightly run.';

-- INDEXES.
--
-- (day, <dimension>) rather than (<dimension>, day), which is the opposite of the fact table's
-- namespace_window index -- and the difference is which column the query constrains first. The fact
-- table is read as "this namespace, recently"; the rollup is read as "this date range, split by
-- dimension". A leading `day` serves the range scan and the GROUP BY reads dimension values off the
-- same index entries.
CREATE INDEX container_allocations_daily_day_idx        ON container_allocations_daily (day);
CREATE INDEX container_allocations_daily_namespace_idx  ON container_allocations_daily (day, namespace_name);
CREATE INDEX container_allocations_daily_team_idx       ON container_allocations_daily (day, team) WHERE team <> '';
CREATE INDEX container_allocations_daily_workload_idx   ON container_allocations_daily (day, namespace_name, workload_kind, workload_name);

-- The rollup writer deletes a whole (day, cluster) before reinserting it, so that predicate needs to
-- be an index lookup rather than a scan of every day ever rolled up.
CREATE INDEX container_allocations_daily_rewrite_idx    ON container_allocations_daily (day, cluster_name);

-- ============================================================================
-- monthly_reports
-- ============================================================================

-- WHY A TABLE, WHEN THIS IS DERIVABLE FROM THE DAILY ROLLUP
-- --------------------------------------------------------
-- Because a statement must not change after it is issued. A report computed on the fly is a VIEW of
-- current data: backfill a missing day, correct a price in the catalogue, or fix a rollup bug, and
-- last August's figure silently becomes a different number than the one somebody already reported.
--
-- Every other table here answers "what is true now". This one answers "what did we say in
-- September", which is a different question and needs its own storage. finalised_at is the line
-- between the two: NULL means provisional and regenerable, set means closed and immutable.
--
-- This is the same reasoning that makes the fact table immutable, applied one level up.
CREATE TABLE monthly_reports (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    cluster_name text NOT NULL,
    -- The first day of the month, in UTC. A date rather than (year, month) integers so range
    -- predicates and ordering are ordinary date operations rather than tuple comparisons.
    period_month date NOT NULL,

    -- SCOPE: what this statement is ABOUT.
    --
    -- Two columns rather than one table per scope. A statement for a cluster, a namespace and a team
    -- carry identical measures and identical coverage metadata, and only differ in what they name --
    -- so three tables would be three copies of the same schema, and any new scope a fourth.
    --
    -- scope_kind is constrained rather than free text, because an unconstrained kind eventually
    -- contains both 'team' and 'Team' and every query silently misses half its rows.
    scope_kind  text NOT NULL,
    scope_value text NOT NULL,

    cpu_cost    numeric(20,10) NOT NULL,
    memory_cost numeric(20,10) NOT NULL,
    total_cost  numeric(20,10) GENERATED ALWAYS AS (cpu_cost + memory_cost) STORED,

    cpu_billable_core_hours   numeric(30,10) NOT NULL,
    memory_billable_gib_hours numeric(30,10) NOT NULL,
    wasted_cpu_core_hours     numeric(30,10) NOT NULL,
    wasted_memory_gib_hours   numeric(30,10) NOT NULL,

    -- HONESTY METADATA. This is what separates a statement from a chart.
    --
    -- A month containing a collector outage produces a number that is confidently too low, and
    -- nothing about the number itself reveals that. A report that cannot say how complete it is
    -- should not be handed to anyone who makes decisions with it.
    days_with_data integer NOT NULL,
    days_in_month  integer NOT NULL,
    window_count   bigint  NOT NULL,
    -- Generated so it can never drift from its inputs.
    --
    -- KNOWN LIMITATION, stated rather than hidden: this is day-level coverage, so a day on which the
    -- collector ran for one hour counts as a full day. window_count is reported alongside precisely
    -- so a partial day is visible as a dip, and the daily trend endpoint is where you see it.
    -- Second-level coverage would need an expected-window count, which needs the collection interval
    -- of every historical day -- not something this schema records.
    coverage numeric(5,4) GENERATED ALWAYS AS (
        CASE WHEN days_in_month > 0
             THEN round(days_with_data::numeric / days_in_month, 4)
             ELSE 0 END
    ) STORED,
    estimated_rates boolean NOT NULL DEFAULT false,

    generated_at timestamptz NOT NULL DEFAULT now(),
    -- NULL means provisional: the month is still open, or is closed but not yet signed off, and
    -- regenerating is allowed. Once set, this row is frozen -- enforced by the trigger below rather
    -- than only by the code that happens to write it.
    finalised_at timestamptz,

    CONSTRAINT monthly_reports_scope_kind CHECK (scope_kind IN ('cluster', 'namespace', 'team')),
    -- A cluster-scoped report names the cluster, so scope_value is never empty for any kind.
    CONSTRAINT monthly_reports_scope_value_present CHECK (scope_value <> ''),
    -- The first of the month, enforced. Storing 2026-08-04 as a "month" would make two reports for
    -- August that neither the unique constraint nor any query would recognise as duplicates.
    CONSTRAINT monthly_reports_month_start CHECK (period_month = date_trunc('month', period_month)::date),
    CONSTRAINT monthly_reports_days CHECK (
        days_in_month BETWEEN 28 AND 31 AND days_with_data BETWEEN 0 AND days_in_month
    ),
    CONSTRAINT monthly_reports_non_negative CHECK (
        cpu_cost >= 0 AND memory_cost >= 0 AND window_count >= 0
        AND cpu_billable_core_hours >= 0 AND memory_billable_gib_hours >= 0
        AND wasted_cpu_core_hours >= 0 AND wasted_memory_gib_hours >= 0
    ),

    -- One statement per scope per month. This is the ON CONFLICT target for regenerating a
    -- provisional report.
    CONSTRAINT monthly_reports_unique UNIQUE (cluster_name, period_month, scope_kind, scope_value)
);

COMMENT ON TABLE monthly_reports IS
    'Immutable monthly statements. finalised_at NULL means provisional and regenerable; set means frozen.';
COMMENT ON COLUMN monthly_reports.coverage IS
    'days_with_data / days_in_month. DAY-level: a one-hour day counts as full. Read window_count alongside.';
COMMENT ON COLUMN monthly_reports.finalised_at IS
    'Once set, the row is frozen by trigger. This is what makes a statement quotable.';

CREATE INDEX monthly_reports_period_idx ON monthly_reports (period_month DESC, scope_kind);

-- IMMUTABILITY, ENFORCED IN THE DATABASE.
--
-- The writer already refuses to touch a finalised row: its upsert carries
-- `WHERE monthly_reports.finalised_at IS NULL`, so the normal path skips finalised rows gracefully
-- and reports how many it left alone.
--
-- This trigger is not that check repeated. It is the backstop for every OTHER code path -- a future
-- endpoint, a migration, an operator with psql and good intentions. "A finalised statement is never
-- rewritten" is a property of the DATA, and a property of the data belongs where the data is. An
-- invariant enforced only by the one function that currently writes the table is an invariant that
-- lasts exactly until the second writer appears.
--
-- Un-finalising is permitted (finalised_at set -> NULL is not blocked by this trigger's condition)
-- so a genuine mistake stays correctable in one deliberate, auditable statement.
CREATE OR REPLACE FUNCTION monthly_reports_reject_finalised_update() RETURNS trigger AS $$
BEGIN
    IF OLD.finalised_at IS NOT NULL AND NEW.finalised_at IS NOT NULL THEN
        RAISE EXCEPTION
            'monthly_reports: % % for % is finalised (at %) and cannot be modified; set finalised_at to NULL first if this is deliberate',
            OLD.scope_kind, OLD.scope_value, OLD.period_month, OLD.finalised_at;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER monthly_reports_immutable
    BEFORE UPDATE ON monthly_reports
    FOR EACH ROW EXECUTE FUNCTION monthly_reports_reject_finalised_update();

-- Deleting a finalised report is blocked for the same reason. A statement that can be deleted and
-- regenerated is not immutable, it is merely inconvenient to change.
CREATE OR REPLACE FUNCTION monthly_reports_reject_finalised_delete() RETURNS trigger AS $$
BEGIN
    IF OLD.finalised_at IS NOT NULL THEN
        RAISE EXCEPTION
            'monthly_reports: % % for % is finalised and cannot be deleted; set finalised_at to NULL first if this is deliberate',
            OLD.scope_kind, OLD.scope_value, OLD.period_month;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER monthly_reports_undeletable
    BEFORE DELETE ON monthly_reports
    FOR EACH ROW EXECUTE FUNCTION monthly_reports_reject_finalised_delete();
