-- Reverses 000004.
--
-- Triggers are dropped implicitly with their table, but the FUNCTIONS are not -- they are
-- schema-level objects and would survive as orphans, so a re-run of the up migration would hit
-- "function already exists". CREATE OR REPLACE tolerates that; dropping them explicitly means the
-- down migration actually leaves the schema as it found it, which is the only property that makes a
-- down migration worth having.
DROP TABLE IF EXISTS monthly_reports;
DROP FUNCTION IF EXISTS monthly_reports_reject_finalised_update();
DROP FUNCTION IF EXISTS monthly_reports_reject_finalised_delete();

-- Dropping the rollup destroys derived data only: every row is recomputable from the fact table by
-- `make rollup-backfill`. That is worth stating, because it is what makes this down migration safe
-- to run and the fact table's own down migration something you would never run.
DROP TABLE IF EXISTS container_allocations_daily;
