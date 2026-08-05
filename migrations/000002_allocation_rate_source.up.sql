-- Persist the PROVENANCE of the rate used for each fact row.
--
-- WHY A NEW MIGRATION RATHER THAN EDITING 000001
-- ----------------------------------------------
-- 000001 has been applied, and golang-migrate compares only the version number. Editing an
-- applied file leaves the database and the repository silently disagreeing: this machine would
-- have the column, a colleague's would not, and nothing would report the difference.
--
-- The rule is roll FORWARD. A mistake in migration 1 is fixed by migration 2, never by
-- reversing 1. That is also why this is purely ADDITIVE -- a nullable-with-default column
-- requires no table rewrite in modern Postgres, so it takes a brief lock and returns, even on
-- a table with a hundred million rows.
--
-- WHY THE COLUMN IS NEEDED AT ALL
-- Phase 3 makes every rate carry how it was derived: an exact catalogue match, published
-- per-resource rates, or a fallback GUESS for an unrecognised instance type. Once a cost is a
-- number in a database those are indistinguishable, and they must not be trusted equally --
-- otherwise someone takes a figure derived from a guess to a finance meeting. Dropping the
-- provenance at the persistence boundary would waste the entire point of tracking it.

ALTER TABLE container_allocations
    ADD COLUMN rate_source text NOT NULL DEFAULT '';

COMMENT ON COLUMN container_allocations.rate_source IS
    'How the rate was derived: catalogue, explicit_rates, or fallback. A fallback figure is an estimate and must be reported as such.';

-- Partial index: only rows priced from a guess are interesting to find, and they should be the
-- minority. Indexing the whole column would mostly index the value 'catalogue', which no query
-- filters on.
--
-- This answers "how much of my reported bill is actually estimated?" without a full scan.
CREATE INDEX container_allocations_fallback_idx
    ON container_allocations (window_start DESC)
    WHERE rate_source = 'fallback';
