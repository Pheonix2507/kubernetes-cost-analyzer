"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api, type CostSummaryResponse, type GroupBy } from "@/lib/api/client";
import { asMoney, formatMoney, formatQuantity, toPlotValue } from "@/lib/money";
import { keys } from "@/lib/query/keys";
import { Card, EmptyState, MagnitudeBar } from "@/components/figures";
import { GROUP_BY_OPTIONS, RANGES, resolveRange, type RangeKey } from "@/lib/range";

/**
 * The interactive cost table.
 *
 * WHY THIS IS THE PAGE THAT EARNS TanStack Query
 * =============================================
 * The overview is a Server Component with no Query at all, because nothing on it changes without a
 * navigation — caching across interactions is worthless when there are no interactions.
 *
 * This page has two: a grouping selector and a range selector. Both re-query, and a reader flips between
 * them constantly — namespace, then team, then back to namespace. Without a cache that third selection
 * is a full round trip for data the browser fetched ten seconds ago. With one it is instant, and Query's
 * `staleTime: 60_000` matches the API's own `Cache-Control: max-age=60` so the two layers agree about
 * what "fresh" means.
 *
 * THE HYDRATION CONTRACT
 * `initialData` is the Server Component's fetch, handed down as a prop. It is what makes this render
 * with rows already present rather than with a spinner: Query treats it as a cache entry that is already
 * fresh, so the first paint has data and no request is made.
 *
 * Without it the sequence would be render-empty → fetch → re-render, which is the classic double-fetch
 * that makes React Server Components look pointless: the server does the work, then the client throws it
 * away and does it again.
 */
export function CostsTable({
  initialData,
  initialGroupBy,
  initialRange,
}: {
  initialData: CostSummaryResponse;
  initialGroupBy: GroupBy;
  initialRange: RangeKey;
}) {
  const [groupBy, setGroupBy] = useState<GroupBy>(initialGroupBy);
  const [rangeKey, setRangeKey] = useState<RangeKey>(initialRange);
  const [sort, setSort] = useState<"total_cost" | "wasted_cpu_core_hours">("total_cost");

  const range = resolveRange(rangeKey);
  const params = { ...range, group_by: groupBy, sort, order: "desc" as const, limit: 100 };

  const { data, isFetching, isError, error } = useQuery({
    queryKey: keys.costs.summary({
      from: range.from,
      to: range.to,
      groupBy,
      sort,
      order: "desc",
      limit: 100,
      filters: {},
    }),
    queryFn: () => api.costSummary(params),
    /**
     * The server's fetch, used only while the selectors still match what the server was asked for.
     *
     * The conditional matters. Without it, `initialData` would be returned for EVERY key — so switching
     * the grouping to "team" would show the namespace rows as if they were teams, briefly and
     * convincingly, before the real data arrived. A wrong label on a real number is worse than a spinner.
     */
    initialData:
      groupBy === initialGroupBy && rangeKey === initialRange ? initialData : undefined,
    /**
     * placeholderData keeps the PREVIOUS result on screen while a new one loads.
     *
     * The alternative is the table emptying on every selector change, so the page height collapses and
     * the content below jumps. Keeping the stale rows visible and dimming them communicates "this is
     * being replaced" without moving anything — and the `isFetching` flag below is what makes the
     * staleness honest rather than hidden.
     */
    placeholderData: (previous) => previous,
  });

  const rows = data?.items ?? [];
  const totals = data?.totals;

  /**
   * The largest value in the sorted column, for the inline magnitude bars.
   *
   * Scaled to the largest ROW rather than to the total, because a bar is a comparison between rows.
   * Against the total, the top row of a six-row table would fill a third of its bar and every other row
   * would be a sliver — technically proportional, and useless for the comparison the reader is making.
   *
   * toPlotValue is the sanctioned float conversion: this is a pixel width, not a figure. The number
   * beside the bar is always the exact string.
   */
  const maxValue = Math.max(
    ...rows.map((r) =>
      toPlotValue(asMoney(sort === "total_cost" ? (r.total_cost ?? "0") : (r.wasted_cpu_core_hours ?? "0"))),
    ),
    0,
  );

  return (
    <div className="space-y-4">
      {/*
       * FILTERS IN ONE ROW ABOVE THE DATA. Not in a sidebar, not in a modal: the reader changes a
       * selector and looks immediately at the number below it, so the two belong adjacent.
       */}
      <div className="flex flex-wrap items-end gap-3">
        <label className="text-xs">
          <span className="block pb-1" style={{ color: "var(--text-muted)" }}>
            Group by
          </span>
          <select
            value={groupBy}
            onChange={(e) => setGroupBy(e.target.value as GroupBy)}
            className="rounded border px-2 py-1 text-sm"
            style={{
              borderColor: "var(--border)",
              background: "var(--surface-1)",
              color: "var(--text-primary)",
            }}
          >
            {GROUP_BY_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        </label>

        <label className="text-xs">
          <span className="block pb-1" style={{ color: "var(--text-muted)" }}>
            Range
          </span>
          <select
            value={rangeKey}
            onChange={(e) => setRangeKey(e.target.value as RangeKey)}
            className="rounded border px-2 py-1 text-sm"
            style={{
              borderColor: "var(--border)",
              background: "var(--surface-1)",
              color: "var(--text-primary)",
            }}
          >
            {RANGES.map((r) => (
              <option key={r.key} value={r.key}>
                {r.label}
              </option>
            ))}
          </select>
        </label>

        <label className="text-xs">
          <span className="block pb-1" style={{ color: "var(--text-muted)" }}>
            Sort by
          </span>
          <select
            value={sort}
            onChange={(e) => setSort(e.target.value as typeof sort)}
            className="rounded border px-2 py-1 text-sm"
            style={{
              borderColor: "var(--border)",
              background: "var(--surface-1)",
              color: "var(--text-primary)",
            }}
          >
            <option value="total_cost">Cost</option>
            <option value="wasted_cpu_core_hours">Wasted CPU</option>
          </select>
        </label>

        {/* A quiet, non-moving staleness cue. A spinner that replaced the table would discard data the
            reader is still using. */}
        {isFetching && (
          <span className="pb-1 text-xs" style={{ color: "var(--text-muted)" }}>
            updating…
          </span>
        )}
      </div>

      {isError && (
        <Card>
          <EmptyState
            message="Could not load costs."
            // The API's own message, which names the offending parameter for a 400. Replacing it with
            // "something went wrong" would discard the only part the reader can act on.
            hint={error instanceof Error ? error.message : undefined}
          />
        </Card>
      )}

      <Card
        title={`Cost by ${GROUP_BY_OPTIONS.find((o) => o.value === groupBy)?.label.toLowerCase()}`}
        subtitle={
          totals
            ? `${formatMoney(asMoney(totals.total_cost))} total · ${formatQuantity(asMoney(totals.wasted_cpu_core_hours), "core-h")} wasted${totals.truncated ? " · TRUNCATED" : ""}`
            : undefined
        }
      >
        {rows.length === 0 ? (
          <EmptyState
            message="No cost recorded in this range."
            hint="If the collector has never run, the fact table is empty — try `make run-collector`."
          />
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr style={{ color: "var(--text-muted)" }} className="text-xs">
                  <th scope="col" className="py-2 pr-4 text-left font-medium">
                    {GROUP_BY_OPTIONS.find((o) => o.value === groupBy)?.label}
                  </th>
                  <th scope="col" className="py-2 pl-4 text-right font-medium">
                    Cost
                  </th>
                  <th scope="col" className="py-2 pl-4 text-right font-medium">
                    Billed CPU
                  </th>
                  <th scope="col" className="py-2 pl-4 text-right font-medium">
                    Wasted CPU
                  </th>
                  <th scope="col" className="py-2 pl-4 text-right font-medium">
                    Wasted memory
                  </th>
                  <th scope="col" className="py-2 pl-4 text-right font-medium">
                    Windows
                  </th>
                </tr>
              </thead>
              <tbody>
                {rows.map((row, i) => {
                  const label = groupLabel(row);
                  const sortedValue = toPlotValue(
                    asMoney(sort === "total_cost" ? (row.total_cost ?? "0") : (row.wasted_cpu_core_hours ?? "0")),
                  );
                  return (
                    <tr
                      key={`${label}-${i}`}
                      className="border-t"
                      style={{ borderColor: "var(--border)", opacity: isFetching ? 0.6 : 1 }}
                    >
                      <th
                        scope="row"
                        className="max-w-xs py-2 pr-4 text-left font-normal"
                        style={{ color: "var(--text-primary)" }}
                      >
                        <span className="block truncate">{label}</span>
                        {/* The magnitude bar sits UNDER the row label rather than replacing a number.
                            It is a reading aid for the shape of the column; every value is still text. */}
                        <MagnitudeBar fraction={maxValue > 0 ? sortedValue / maxValue : 0} />
                        {row.estimated_rates && (
                          <span className="text-xs" style={{ color: "var(--text-muted)" }}>
                            estimated rate
                          </span>
                        )}
                      </th>
                      {/* tabular-nums on every numeric column: these must align vertically, which is
                          the opposite decision from the stat tiles' hero figures. */}
                      <td className="tabular py-2 pl-4 text-right" style={{ color: "var(--text-primary)" }}>
                        {formatMoney(asMoney(row.total_cost ?? "0"))}
                      </td>
                      <td className="tabular py-2 pl-4 text-right" style={{ color: "var(--text-secondary)" }}>
                        {formatQuantity(asMoney(row.cpu_billable_core_hours ?? "0"), "core-h")}
                      </td>
                      <td className="tabular py-2 pl-4 text-right" style={{ color: "var(--text-secondary)" }}>
                        {formatQuantity(asMoney(row.wasted_cpu_core_hours ?? "0"), "core-h")}
                      </td>
                      <td className="tabular py-2 pl-4 text-right" style={{ color: "var(--text-secondary)" }}>
                        {formatQuantity(asMoney(row.wasted_memory_gib_hours ?? "0"), "GiB-h")}
                      </td>
                      <td className="tabular py-2 pl-4 text-right" style={{ color: "var(--text-muted)" }}>
                        {row.container_windows ?? 0}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
              {totals && (
                <tfoot>
                  <tr className="border-t-2" style={{ borderColor: "var(--axis)" }}>
                    <th scope="row" className="py-2 pr-4 text-left text-xs font-medium" style={{ color: "var(--text-muted)" }}>
                      {/* THE TOTAL COMES FROM THE API, not from summing the column above.
                          Two reasons: these are 26-digit decimals that parseFloat would truncate, and
                          when `truncated` is true the rows are not the whole answer, so no client-side
                          sum could be right. */}
                      Total {totals.truncated && "(truncated — showing the first 100)"}
                    </th>
                    <td className="tabular py-2 pl-4 text-right font-semibold" style={{ color: "var(--text-primary)" }}>
                      {formatMoney(asMoney(totals.total_cost))}
                    </td>
                    <td className="tabular py-2 pl-4 text-right" style={{ color: "var(--text-secondary)" }}>
                      {formatQuantity(asMoney(totals.cpu_billable_core_hours), "core-h")}
                    </td>
                    <td className="tabular py-2 pl-4 text-right" style={{ color: "var(--text-secondary)" }}>
                      {formatQuantity(asMoney(totals.wasted_cpu_core_hours), "core-h")}
                    </td>
                    <td className="tabular py-2 pl-4 text-right" style={{ color: "var(--text-secondary)" }}>
                      {formatQuantity(asMoney(totals.wasted_memory_gib_hours), "GiB-h")}
                    </td>
                    <td />
                  </tr>
                </tfoot>
              )}
            </table>
          </div>
        )}
      </Card>
    </div>
  );
}

/**
 * groupLabel picks the dimension values relevant to the current grouping.
 *
 * The API populates only the fields the grouping selected and leaves the rest empty -- a row grouped by
 * team carries no namespace, deliberately, because a team spans namespaces and naming one would be
 * arbitrary. So this collects whatever is non-empty rather than reading a fixed field.
 */
function groupLabel(row: Record<string, unknown>): string {
  const dimensions = [
    "namespace",
    "team",
    "environment",
    "cost_centre",
    "workload_kind",
    "workload_name",
    "node",
    "instance_type",
    "capacity_type",
    "pod_name",
    "container_name",
  ];
  const parts = dimensions
    .map((d) => row[d])
    .filter((v): v is string => typeof v === "string" && v !== "");
  return parts.length > 0 ? parts.join(" · ") : "(unattributed)";
}
