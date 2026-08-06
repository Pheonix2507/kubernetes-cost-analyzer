"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api, type Interval, type GroupBy, type TrendResponse } from "@/lib/api/client";
import { asMoney, formatMoney, formatRatio, isNegative } from "@/lib/money";
import { keys } from "@/lib/query/keys";
import { Card, EmptyState, StatTile } from "@/components/figures";
import { TrendChart } from "@/components/TrendChart";
import { MAX_SERIES } from "@/lib/viz/palette";
import { GROUP_BY_OPTIONS, INTERVAL_OPTIONS, RANGES, resolveRange, type RangeKey } from "@/lib/range";

/** The interactive trends view. Interval and grouping both re-query, so Query's cache earns its place. */
export function TrendsView({
  initialData,
  initialRange,
}: {
  initialData: TrendResponse;
  initialRange: RangeKey;
}) {
  const [interval, setInterval] = useState<Interval>("day");
  const [groupBy, setGroupBy] = useState<GroupBy>("namespace");
  const [rangeKey, setRangeKey] = useState<RangeKey>(initialRange);

  const range = resolveRange(rangeKey);

  const { data, isFetching, isError, error } = useQuery({
    queryKey: keys.costs.trend({
      from: range.from,
      to: range.to,
      interval,
      groupBy,
      compare: true,
      filters: {},
    }),
    queryFn: () =>
      api.trend({
        ...range,
        interval,
        group_by: groupBy,
        compare: "previous_period",
        // SIX, not the API's default of twenty. Twenty categorical lines is not a chart anyone can read
        // and no palette fixes it. The chart says so on screen when it has folded series out.
        limit: MAX_SERIES,
      }),
    initialData:
      interval === "day" && groupBy === "namespace" && rangeKey === initialRange
        ? initialData
        : undefined,
    placeholderData: (previous) => previous,
  });

  const cmp = data?.comparison;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end gap-3">
        <label className="text-xs">
          <span className="block pb-1" style={{ color: "var(--text-muted)" }}>Interval</span>
          <select
            value={interval}
            onChange={(e) => setInterval(e.target.value as Interval)}
            className="rounded border px-2 py-1 text-sm"
            style={{ borderColor: "var(--border)", background: "var(--surface-1)", color: "var(--text-primary)" }}
          >
            {INTERVAL_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>{o.label}</option>
            ))}
          </select>
        </label>

        <label className="text-xs">
          <span className="block pb-1" style={{ color: "var(--text-muted)" }}>Group by</span>
          <select
            value={groupBy}
            onChange={(e) => setGroupBy(e.target.value as GroupBy)}
            className="rounded border px-2 py-1 text-sm"
            style={{ borderColor: "var(--border)", background: "var(--surface-1)", color: "var(--text-primary)" }}
          >
            {GROUP_BY_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>{o.label}</option>
            ))}
          </select>
        </label>

        <label className="text-xs">
          <span className="block pb-1" style={{ color: "var(--text-muted)" }}>Range</span>
          <select
            value={rangeKey}
            onChange={(e) => setRangeKey(e.target.value as RangeKey)}
            className="rounded border px-2 py-1 text-sm"
            style={{ borderColor: "var(--border)", background: "var(--surface-1)", color: "var(--text-primary)" }}
          >
            {RANGES.map((r) => <option key={r.key} value={r.key}>{r.label}</option>)}
          </select>
        </label>

        {isFetching && <span className="pb-1 text-xs" style={{ color: "var(--text-muted)" }}>updating…</span>}
      </div>

      {isError && (
        <Card>
          <EmptyState
            message="Could not load the trend."
            hint={error instanceof Error ? error.message : undefined}
          />
        </Card>
      )}

      {/*
       * The period-over-period comparison, and the honesty flag beside it.
       *
       * `comparable: false` is the single most important field on this page. A period during which
       * collection started shows a large apparent increase that is entirely an artefact of when the
       * collector was deployed -- it fired on the real cluster the first time this ran, with cost "up
       * 68%" between two five-day windows where the earlier one had three days of data.
       *
       * So when it is false the change is shown with the caveat ATTACHED, not in a tooltip.
       */}
      {cmp && (
        <div className="grid gap-4 sm:grid-cols-3">
          <StatTile label="This period" value={formatMoney(asMoney(cmp.current_total_cost))} />
          <StatTile label="Previous period" value={formatMoney(asMoney(cmp.previous_total_cost))} />
          <StatTile
            label="Change"
            value={formatMoney(asMoney(cmp.change))}
            delta={{
              // change_ratio is NULL when the previous period cost nothing -- "new workload" has no
              // percentage increase, and rendering "+Inf%" would state something the data does not
              // support.
              text: cmp.change_ratio ? formatRatio(cmp.change_ratio) : "no baseline to compare against",
              direction: isNegative(asMoney(cmp.change)) ? "down" : "up",
              // Down is GOOD for a bill. A single "isPositive" flag would paint a rising bill green.
              goodWhen: "down",
            }}
            hint={cmp.comparable ? undefined : cmp.note}
          />
        </div>
      )}

      <Card
        title="Cost over time"
        subtitle={
          data
            ? `${INTERVAL_OPTIONS.find((o) => o.value === interval)?.note} · source: ${data.source === "daily_rollup" ? "daily rollup" : "raw samples"}`
            : undefined
        }
      >
        {data?.series?.length ? (
          <TrendChart series={data.series} interval={interval} />
        ) : (
          <EmptyState
            message="No data in this range."
            hint={
              interval === "day" || interval === "week" || interval === "month"
                ? "Daily and coarser buckets read the rollup, which only covers complete days that have been rolled up. Try `make rollup-backfill`, or switch to hourly to read the raw samples."
                : undefined
            }
          />
        )}
      </Card>
    </div>
  );
}
