-- Reverse of 000002.
--
-- Dropping the column takes the index with it, so the index is not dropped separately -- doing
-- so would be redundant and would drift if another index were added later.
ALTER TABLE container_allocations DROP COLUMN IF EXISTS rate_source;
