/**
 * Time ranges.
 *
 * WHY A SHARED MODULE RATHER THAN `new Date()` IN EACH PAGE
 * ========================================================
 * Every page needs "the last N days" as two RFC3339 strings, and computing that inline in five places
 * produces five slightly different answers. Worse, it produces a NEW answer on every render: a page that
 * calls `new Date()` during render generates a different `from` each time, so a TanStack Query key built
 * from it changes constantly and the cache never hits. The query refetches on every re-render and the
 * cache appears broken.
 *
 * Ranges are therefore snapped -- see `snapToMinute`.
 *
 * WHY RFC3339 AND WHY UTC
 * The API accepts RFC3339 only, and requires a timezone. That was a deliberate Go-side decision:
 * "01/02/2026" is January 2nd or February 1st depending on the reader, and guessing shifts a whole cost
 * report by a month. `toISOString()` always produces UTC with a Z suffix, so it satisfies both
 * requirements without the caller thinking about it.
 */

/** RANGES are the presets a reader picks from. */
export const RANGES = [
  { key: "24h", label: "Last 24 hours", hours: 24 },
  { key: "7d", label: "Last 7 days", hours: 24 * 7 },
  { key: "30d", label: "Last 30 days", hours: 24 * 30 },
  { key: "90d", label: "Last 90 days", hours: 24 * 90 },
] as const;

export type RangeKey = (typeof RANGES)[number]["key"];

/**
 * snapToMinute rounds an instant down to the minute.
 *
 * THE REASON IS CACHE STABILITY, not tidiness. An unsnapped `to` changes every millisecond, so:
 *
 *   - the TanStack Query key changes on every render, and nothing is ever a cache hit
 *   - two components asking for "the last 7 days" in the same paint get different keys and fetch twice
 *   - the API's own `Cache-Control: private, max-age=60` can never be used, because the URL is new
 *
 * A minute is well inside the collector's five-minute write interval, so snapping costs no freshness
 * whatsoever -- the data cannot have changed within the window being discarded.
 */
function snapToMinute(d: Date): Date {
  const out = new Date(d);
  out.setSeconds(0, 0);
  return out;
}

/** resolveRange turns a preset into the two timestamps the API wants. */
export function resolveRange(key: RangeKey, now: Date = new Date()): { from: string; to: string } {
  const range = RANGES.find((r) => r.key === key) ?? RANGES[1];
  const to = snapToMinute(now);
  const from = new Date(to.getTime() - range.hours * 3_600_000);
  return { from: from.toISOString(), to: to.toISOString() };
}

/**
 * defaultRangeFor picks a sensible preset per page.
 *
 * DIFFERENT PER PAGE, mirroring the API's own asymmetric defaults, because each page asks a different
 * question:
 *
 *   overview / costs    24h  -- a cost figure is about now
 *   recommendations      7d  -- the API defaults here too: a recommendation from one day of data cannot
 *                              see a weekly pattern, so a Sunday batch job looks abandoned on a Tuesday
 *   trends              30d  -- a trend with two points is not a trend
 *
 * Hard-coding 7 days everywhere would be one line simpler and would make the trends page useless and
 * the recommendations page misleading.
 */
export function defaultRangeFor(page: "overview" | "costs" | "trends" | "recommendations"): RangeKey {
  switch (page) {
    case "trends":
      return "30d";
    case "recommendations":
      return "7d";
    default:
      return "24h";
  }
}

/** GROUP_BY_OPTIONS are the dimensions a reader can group by, with readable labels. */
export const GROUP_BY_OPTIONS = [
  { value: "namespace", label: "Namespace" },
  { value: "team", label: "Team" },
  { value: "environment", label: "Environment" },
  { value: "cost_centre", label: "Cost centre" },
  { value: "workload", label: "Workload" },
  { value: "node", label: "Node" },
  { value: "instance_type", label: "Instance type" },
  { value: "capacity_type", label: "Spot vs on-demand" },
  { value: "pod", label: "Pod" },
  { value: "container", label: "Container" },
] as const;

/**
 * INTERVAL_OPTIONS are the trend bucket widths, annotated with which table serves each.
 *
 * The `source` note is shown in the UI rather than kept as a comment, because the two sources do not
 * answer identically -- the rollup has no pod grain and only covers days the rollup job has processed,
 * so a rollup-sourced series can legitimately stop short of today. A reader comparing this against the
 * costs page needs to know which they got.
 */
export const INTERVAL_OPTIONS = [
  { value: "hour", label: "Hourly", note: "reads raw samples; limited to 14 days" },
  { value: "day", label: "Daily", note: "reads the daily rollup" },
  { value: "week", label: "Weekly", note: "reads the daily rollup" },
  { value: "month", label: "Monthly", note: "reads the daily rollup" },
] as const;
