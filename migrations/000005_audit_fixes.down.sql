-- Reverses 000005.
--
-- Recreates the two dropped indexes exactly as migration 000001 defined them, so a down-then-up cycle
-- leaves the schema byte-identical rather than approximately right.
CREATE INDEX IF NOT EXISTS namespaces_labels_idx ON namespaces USING gin (labels jsonb_path_ops);
CREATE INDEX IF NOT EXISTS namespaces_team_idx   ON namespaces (cluster_id, team) WHERE team <> '';

-- The constraint validation is deliberately NOT reversed.
--
-- There is no ALTER TABLE ... INVALIDATE CONSTRAINT, and inventing one by dropping and re-adding the
-- constraint NOT VALID would take an ACCESS EXCLUSIVE lock to achieve nothing -- a validated
-- constraint is strictly better than an unvalidated one in every respect, so "undoing" it would be
-- work in exchange for a worse schema.
--
-- Worth stating rather than leaving as an omission: a down migration that cannot fully reverse its up
-- migration should say which part and why, or the next person assumes it did.
COMMENT ON INDEX pods_namespace_idx IS NULL;
