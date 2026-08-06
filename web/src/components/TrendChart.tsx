"use client";

import { useMemo, useState } from "react";
import type { TrendSeries } from "@/lib/api/client";
import { MAX_SERIES } from "@/lib/viz/palette";
import { asMoney, formatMoney } from "@/lib/money";
import { LineChartSVG, buildSeries } from "./LineChart";

/**
 * The trend chart: an SVG plot plus the two things that make it readable.
 *
 * WHY THIS IS STILL A CLIENT COMPONENT WHEN THE PLOT IS NOT
 * ========================================================
 * LineChartSVG renders the whole plot -- lines, markers, labels, axes -- as plain SVG, and needs no
 * JavaScript at all.
 *
 * This wrapper is a Client Component for exactly one reason: the table TOGGLE is `useState`, and state
 * needs interactivity.
 *
 * The split is deliberate. If the drawing lived in here it would still be inside a client boundary, but
 * the important property is that it does not DEPEND on hydration to produce marks -- which is precisely
 * what made Recharts unusable: its lines only became visible after JavaScript ran. Here the SVG is in
 * the server-rendered HTML, so the data is visible in print, in a screenshot, and before hydration.
 * Only the toggle is inert for a moment, and a late toggle is fine in a way that late DATA is not.
 *
 * WHAT THIS COMPONENT IS OBLIGED TO SHIP, AND WHY
 * The palette validator returns a contrast WARNING for three light-mode slots -- aqua 2.74:1, yellow
 * 2.11:1, magenta 2.62:1 against the light surface. That is not dismissable: it obligates relief,
 * meaning visible direct labels or a table view. So:
 *
 *   - a legend, always, for two or more series, because identity must never be colour alone
 *   - direct end-labels on every line, in LineChartSVG
 *   - a table view, here
 *
 * All unconditional rather than mode-dependent. A component whose accessibility switched on a media
 * query would be one nobody could reason about.
 */
export function TrendChart({
  series: raw,
  interval,
  emptyMessage = "No cost recorded in this range.",
}: {
  series: TrendSeries[];
  interval: string;
  emptyMessage?: string;
}) {
  const [showTable, setShowTable] = useState(false);

  const { series, buckets, hidden } = useMemo(() => buildSeries(raw), [raw]);

  if (buckets.length === 0) {
    return (
      <p className="py-8 text-center text-sm" style={{ color: "var(--text-secondary)" }}>
        {emptyMessage}
      </p>
    );
  }

  const fmt = (iso: string) => formatBucket(iso, interval);
  const singleSeries = series.length === 1;

  return (
    <div>
      <LineChartSVG
        series={series}
        buckets={buckets}
        formatBucket={fmt}
        ariaLabel={`Cost per ${interval} for ${series.map((s) => s.label).join(", ")}`}
      />

      {/*
       * The legend: present for two or more series, ABSENT for one.
       *
       * A one-swatch legend restates the card title and costs a row of space. With several series it is
       * the dependable identity channel -- a reader should never have to match a colour by eye against
       * a line whose value they are also trying to read.
       */}
      {!singleSeries && (
        <ul className="mt-3 flex flex-wrap gap-x-4 gap-y-1">
          {series.map((s) => (
            <li key={s.label} className="flex items-center gap-1.5 text-xs">
              {/* A short line-key rather than a filled square: it matches the mark the reader is
                  hunting for on the plot. */}
              <span
                aria-hidden="true"
                className="inline-block h-0.5 w-4 rounded-full"
                style={{ background: s.colour }}
              />
              <span style={{ color: "var(--text-secondary)" }}>{s.label}</span>
            </li>
          ))}
        </ul>
      )}

      <div className="mt-3 flex flex-wrap items-center justify-between gap-2">
        {hidden > 0 ? (
          <p className="text-xs" style={{ color: "var(--text-muted)" }}>
            Showing the {MAX_SERIES} largest of {raw.length}. {hidden} more are folded out of this chart
            — narrow the filters or read the table.
          </p>
        ) : (
          <span />
        )}

        <button
          type="button"
          onClick={() => setShowTable((v) => !v)}
          className="rounded border px-2 py-1 text-xs"
          style={{ borderColor: "var(--border)", color: "var(--text-secondary)" }}
          aria-expanded={showTable}
        >
          {showTable ? "Hide table" : "Show as table"}
        </button>
      </div>

      {showTable && (
        <div className="mt-3 overflow-x-auto">
          <table className="w-full text-xs">
            <caption className="sr-only">Cost per {interval} for each series</caption>
            <thead>
              <tr style={{ color: "var(--text-muted)" }}>
                <th scope="col" className="py-1 pr-3 text-left font-medium">
                  {interval}
                </th>
                {series.map((s) => (
                  <th key={s.label} scope="col" className="py-1 pl-3 text-right font-medium">
                    {s.label}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {buckets.map((b, i) => (
                <tr key={b} className="border-t" style={{ borderColor: "var(--border)" }}>
                  <th
                    scope="row"
                    className="py-1 pr-3 text-left font-normal"
                    style={{ color: "var(--text-secondary)" }}
                  >
                    {fmt(b)}
                  </th>
                  {series.map((s) => {
                    const p = s.points[i];
                    return (
                      // tabular-nums HERE, because this is a column of numbers that must align
                      // vertically -- the opposite decision from a stat tile's hero figure, where
                      // tabular figures make a value look loose.
                      <td key={s.label} className="tabular py-1 pl-3 text-right">
                        {/* An em dash for a missing bucket, never a zero. A gap in collection and a day
                            that genuinely cost nothing are different facts. */}
                        {p === null || p === undefined ? "—" : formatMoney(asMoney(String(p.value)))}
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

/**
 * formatBucket renders a bucket start at the resolution its interval implies.
 *
 * An hourly bucket labelled "6 Aug" is ambiguous across 24 points; a monthly bucket labelled
 * "1 Aug 00:00" implies a precision it does not have -- it is a whole month's cost, not midnight's. A
 * label should say exactly as much as the bucket means.
 */
function formatBucket(iso: string, interval: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  switch (interval) {
    case "hour":
      return d.toLocaleString(undefined, { day: "numeric", month: "short", hour: "2-digit" });
    case "month":
      return d.toLocaleString(undefined, { month: "short", year: "numeric" });
    case "week":
      return `w/c ${d.toLocaleString(undefined, { day: "numeric", month: "short" })}`;
    default:
      return d.toLocaleString(undefined, { day: "numeric", month: "short" });
  }
}
