-- Reverse of 000001_baseline.up.sql.
--
-- WHY WRITE A DOWN MIGRATION AT ALL
-- ---------------------------------
-- You will almost never run this against production. Rolling a schema BACKWARDS while
-- data exists usually means destroying data, which is why mature teams roll FORWARD
-- instead: a mistake in migration 12 is fixed by migration 13, not by reversing 12.
--
-- It is worth writing anyway, for two reasons:
--
--  1. LOCAL ITERATION. `make migrate-down && make migrate-up` rebuilds the schema from
--     scratch while you are still designing it, which is exactly Phase 2's situation.
--  2. IT PROVES THE UP MIGRATION IS COMPLETE. Writing the reverse forces you to
--     enumerate everything the forward migration created. Objects that are easy to
--     forget -- a function, a DEFAULT partition -- get noticed here rather than
--     surviving as orphans in every environment.
--
-- DROP ORDER IS THE REVERSE OF CREATE ORDER, because foreign keys point backwards.
-- Dropping clusters first would fail on nodes' reference to it.

-- Dropping the parent removes every partition with it, including the default and any
-- created later by ensure_allocation_partition. Explicitly dropping partitions first
-- would leave the list to drift as new months are added.
DROP TABLE IF EXISTS container_allocations;

DROP FUNCTION IF EXISTS ensure_allocation_partition (date);

-- Dimensions, most-dependent first. CASCADE is deliberately NOT used: if something
-- outside this migration has come to depend on these tables, the drop should fail loudly
-- rather than quietly taking that dependency with it.
DROP TABLE IF EXISTS pods;
DROP TABLE IF EXISTS workloads;
DROP TABLE IF EXISTS namespaces;
DROP TABLE IF EXISTS nodes;
DROP TABLE IF EXISTS clusters;
