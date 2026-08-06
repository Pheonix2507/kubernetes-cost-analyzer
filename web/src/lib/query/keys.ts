import type { Filters, GroupBy, Interval } from "@/lib/api/client";

/**
 * Query keys.
 *
 * WHY THEY LIVE IN ONE FILE
 * =========================
 * A TanStack Query key IS the cache identity: two calls with the same key share a cache entry, and
 * two with different keys do not. Scattering key construction across components produces both failure
 * modes at once -- one component writing `["trend", interval]` while another writes
 * `["trend", interval, groupBy]` means they either collide and show each other's data, or they miss
 * and refetch identical data.
 *
 * Centralising also makes invalidation possible: `invalidateQueries({ queryKey: keys.costs.all })`
 * clears every cost query in one call, which is only expressible if every cost key starts with the
 * same prefix by construction rather than by convention.
 *
 * THE HIERARCHY IS DELIBERATE. Keys are arrays and TanStack matches by PREFIX, so
 * ["costs"] matches ["costs", "summary", {...}]. Ordering from general to specific is what makes
 * partial invalidation work.
 *
 * KEYS MUST BE SERIALISABLE AND ORDER-STABLE. Objects are hashed by their contents, not their key
 * order, so { a: 1, b: 2 } and { b: 2, a: 1 } are the same key -- which is why passing the params
 * object directly is safe and does not need sorting.
 */
export const keys = {
  costs: {
    all: ["costs"] as const,
    summary: (params: {
      from: string;
      to: string;
      groupBy: GroupBy;
      sort?: string;
      order?: string;
      limit?: number;
      filters: Filters;
    }) => ["costs", "summary", params] as const,
    trend: (params: {
      from: string;
      to: string;
      interval: Interval;
      groupBy: GroupBy;
      compare: boolean;
      filters: Filters;
    }) => ["costs", "trend", params] as const,
  },
  recommendations: {
    all: ["recommendations"] as const,
    list: (params: { from: string; to: string; filters: Filters }) =>
      ["recommendations", "list", params] as const,
  },
  reports: {
    all: ["reports"] as const,
    monthly: (params: { from?: string; to?: string; scopeKind?: string }) =>
      ["reports", "monthly", params] as const,
  },
} as const;
