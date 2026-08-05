-- Record the PEAK usage within each window, alongside the average.
--
-- WHY THE AVERAGE IS NOT ENOUGH, AND WHY THIS IS A CORRECTNESS FIX
-- ---------------------------------------------------------------
-- The existing cpu_millicores_used and memory_bytes_used are AVERAGES over the window, which is
-- exactly right for cost: cost is an integral over time, so a container at one full core for one
-- second and idle for 299 costs the same as one steady at ~3 millicores.
--
-- It is exactly WRONG for right-sizing. A container averaging 50m that peaks at 400m will be
-- CPU-throttled the moment its request is set to 50m, and a memory recommendation derived from an
-- average gets the container OOMKilled. Recommending from an average is how a cost tool causes an
-- incident, and it is the single most common way these tools lose the trust of the engineers they
-- are meant to help.
--
-- WHY A PERCENTILE OVER WINDOWS IS STILL NOT ENOUGH ON ITS OWN
-- Taking p95 across many windows helps, but each of those windows is itself an average -- so the
-- peaks have already been smoothed away before the percentile ever sees them. p95 of five-minute
-- averages systematically UNDERSTATES the true p95 of instantaneous usage, by more the burstier the
-- workload. The fix is to capture the peak WITHIN the window, at collection time, while the
-- resolution still exists.
--
-- Additive and nullable-with-default, so this takes a brief lock and returns even on a large table.
-- Existing rows get 0, which the recommendation engine treats as "no peak data" rather than as a
-- genuine zero -- see the coverage check in the stats query.

ALTER TABLE container_allocations
    ADD COLUMN cpu_millicores_max bigint NOT NULL DEFAULT 0,
    ADD COLUMN memory_bytes_max   bigint NOT NULL DEFAULT 0;

COMMENT ON COLUMN container_allocations.cpu_millicores_max IS
    'Peak CPU within the window. Right-sizing uses this; cost uses the average. 0 means the row predates this column.';
COMMENT ON COLUMN container_allocations.memory_bytes_max IS
    'Peak memory within the window. Right-sizing uses this; cost uses the average.';

-- The peak can never be below the average it summarises. Enforced rather than trusted, because a
-- max below the mean means a collector bug and any recommendation derived from it would be
-- confidently wrong in the dangerous direction.
--
-- NOT VALID skips the check for EXISTING rows, which all have max = 0 and would violate it. New
-- rows are still checked. That is the standard way to add a constraint to a populated table
-- without a full table scan under lock.
ALTER TABLE container_allocations
    ADD CONSTRAINT container_allocations_max_at_least_avg CHECK (
        (cpu_millicores_max = 0 OR cpu_millicores_max >= cpu_millicores_used)
        AND (memory_bytes_max = 0 OR memory_bytes_max >= memory_bytes_used)
    ) NOT VALID;
