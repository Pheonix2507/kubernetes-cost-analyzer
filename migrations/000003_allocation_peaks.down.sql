-- Reverse of 000003. Dropping the columns removes the constraint with them.
ALTER TABLE container_allocations
    DROP CONSTRAINT IF EXISTS container_allocations_max_at_least_avg;
ALTER TABLE container_allocations
    DROP COLUMN IF EXISTS cpu_millicores_max,
    DROP COLUMN IF EXISTS memory_bytes_max;
