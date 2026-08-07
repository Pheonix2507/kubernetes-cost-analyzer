import type { TrendSeries } from "@/lib/api/client";
import { MAX_SERIES, assignSlots, seriesLabel } from "@/lib/viz/palette";
import { asMoney, formatMoney, toPlotValue } from "@/lib/money";
import { niceTicks } from "@/lib/viz/scale";

/**
 * A hand-written SVG line chart.
 *
 * WHY THIS REPLACED RECHARTS -- AND A CORRECTION TO WHY
 * ======================================================
 * Recharts was the first thing tried, it appeared to render nothing, and I concluded it was broken
 * under React 19. THAT CONCLUSION WAS WRONG, and the mistake is worth recording because it wasted more
 * time than the rewrite did.
 *
 * A stale `next start` process was holding port 3100. Every subsequent `pnpm start` failed to bind with
 * EADDRINUSE -- into a log nobody was reading -- so every verification curl and screenshot was being
 * served by a BUILD FROM BEFORE THE CHANGES. Recharts was probably fine. The lesson is the one this
 * project keeps relearning: a test that passes for a reason you have not established is not evidence,
 * and that applies to manual verification exactly as much as to unit tests.
 *
 * SO WHY IS THIS STILL HERE, rather than reverted? Because two of the observations were real, made
 * before the stale-server confusion, and they hold up:
 *
 *   BUNDLE. Removing Recharts took the /trends route's first load from 222 kB to 117 kB. Measured
 *   twice, on builds that were definitely served. 105 kB for a line chart is a lot.
 *
 *   IT NEEDED HYDRATION TO DRAW ANYTHING, and it animated the line into existence -- stroke-dasharray
 *   from "0px <length>" to "<length> 0px". That was observed in a real DOM dump. A chart whose data
 *   only becomes visible once an animation COMPLETES is invisible in print, in PDF export, in a
 *   screenshot for a report, and potentially to a reader with prefers-reduced-motion. This component is
 *   plain SVG in the server-rendered HTML: it renders with JavaScript disabled entirely.
 *
 *   And the mark specs the design system asks for -- 2px strokes, 8px markers with a 2px surface ring,
 *   selective direct labels, hairline solid gridlines -- are attributes here rather than props to
 *   discover.
 *
 * WHAT IS GENUINELY LOST: no brush, no zoom, no animated transitions between datasets, and any hover
 * layer must be written. For a dashboard that reads values off a line and offers a table view, that is
 * a good trade. For an exploratory tool with linked brushing it would not be.
 *
 * RESPONSIVE WITHOUT MEASURING
 * A viewBox plus `width: 100%` scales the whole drawing to its container with no ResizeObserver, no
 * hydration and no layout thrash. The cost is that text scales with the chart -- 11 units at a 1.2x
 * scale renders as ~13px -- which is acceptable within the narrow range a dashboard column spans, and
 * is why the coordinate space below is close to a real pixel size rather than something abstract.
 */

/** The drawing's coordinate space. Close to real pixels so scaled text stays near its nominal size. */
const VIEW_W = 1000;
const VIEW_H = 300;
/** Padding for the axes. Left is widest because y-tick labels are money. */
const PAD = { top: 12, right: 96, bottom: 28, left: 68 };

const PLOT_W = VIEW_W - PAD.left - PAD.right;
const PLOT_H = VIEW_H - PAD.top - PAD.bottom;

export interface Point {
  bucket: string;
  value: number;
}
export interface Series {
  label: string;
  colour: string;
  points: (Point | null)[];
}

/**
 * buildSeries turns the API's response into plot-ready series.
 *
 * Exported and pure, so the pivot is testable without a DOM. Recharts' equivalent logic lived inside a
 * useMemo in a client component and could only be exercised by rendering.
 */
export function buildSeries(raw: TrendSeries[]): { series: Series[]; buckets: string[]; hidden: number } {
  const shown = raw.slice(0, MAX_SERIES);
  const labels = shown.map((s) => seriesLabel(s.group));
  const slots = assignSlots(labels);

  // Every bucket any series mentions, sorted. A series missing one leaves a null at that index rather
  // than shifting its later points left -- a gap is a collection outage and must look like one.
  const buckets = [
    ...new Set(shown.flatMap((s) => (s.points ?? []).map((p) => p.bucket_start ?? ""))),
  ]
    .filter(Boolean)
    .sort();

  const series: Series[] = shown.map((s, i) => {
    const label = labels[i]!;
    const byBucket = new Map(
      (s.points ?? []).map((p) => [
        p.bucket_start ?? "",
        toPlotValue(asMoney(p.total_cost ?? "0")),
      ]),
    );
    return {
      label,
      colour: slots.get(label) ?? "var(--series-1)",
      points: buckets.map((b) =>
        byBucket.has(b) ? { bucket: b, value: byBucket.get(b)! } : null,
      ),
    };
  });

  return { series, buckets, hidden: raw.length - shown.length };
}


export function LineChartSVG({
  series,
  buckets,
  formatBucket,
  ariaLabel,
}: {
  series: Series[];
  buckets: string[];
  formatBucket: (iso: string) => string;
  ariaLabel: string;
}) {
  const values = series.flatMap((s) => s.points.filter((p): p is Point => p !== null).map((p) => p.value));
  const rawMax = values.length > 0 ? Math.max(...values) : 0;
  const ticks = niceTicks(rawMax);
  // The scale tops out at the last TICK, not at the data's max, so the highest point sits on a
  // gridline rather than at the very edge of the plot where it is hard to read against the frame.
  const yMax = ticks[ticks.length - 1] || 1;

  const x = (i: number) =>
    PAD.left + (buckets.length <= 1 ? PLOT_W / 2 : (i / (buckets.length - 1)) * PLOT_W);
  const y = (v: number) => PAD.top + PLOT_H - (v / yMax) * PLOT_H;

  // Ticks are thinned rather than allowed to collide. A collided axis reads as a rendering bug.
  const maxXLabels = 8;
  const xStep = Math.max(1, Math.ceil(buckets.length / maxXLabels));

  /**
   * DIRECT END-LABELS ARE DROPPED WHOLESALE WHEN THEY WOULD COLLIDE.
   *
   * A screenshot showed six series converging at the right edge, and two labels overprinted into
   * "team-platfolatform" -- worse than no label, because it is unreadable AND looks like a rendering
   * fault.
   *
   * The tempting fix is to nudge them apart vertically. That is wrong: a label moved away from its line
   * is no longer attached to anything, so it stops identifying the series it names and reads as noise.
   * The alternatives are leader lines, small multiples, or falling back to the legend -- and past about
   * four converging series the fallback is the honest answer.
   *
   * ALL-OR-NOTHING rather than per-label, because labelling three of six would leave a reader wondering
   * what distinguishes the labelled ones. And dropping them costs no accessibility here: the legend is
   * always present and the table view is one click away, which is the relief the palette's contrast
   * warning actually requires.
   */
  const endYs = series
    .map((s) => {
      const lastIdx = s.points.reduce((acc, p, i) => (p !== null ? i : acc), -1);
      const last = lastIdx >= 0 ? s.points[lastIdx] : null;
      return last ? y(last.value) : null;
    })
    .filter((v): v is number => v !== null)
    .sort((a, b) => a - b);
  // 13 units at font-size 11 is roughly one line-height in this coordinate space.
  const minLabelGap = 13;
  const labelsWouldCollide = endYs.some(
    (v, i) => i > 0 && v - endYs[i - 1]! < minLabelGap,
  );

  return (
    <svg
      viewBox={`0 0 ${VIEW_W} ${VIEW_H}`}
      // width 100% + height auto: the drawing scales to its column with no measurement, no
      // ResizeObserver and no hydration.
      style={{ width: "100%", height: "auto", display: "block" }}
      role="img"
      aria-label={ariaLabel}
    >
      {/* Horizontal gridlines only: hairline, SOLID, one step off the surface. Vertical lines would add
          ink without information, because the x-ticks already mark the buckets. Dashed lines compete
          with the data for the eye at exactly the density where the chart is busiest. */}
      {ticks.map((t) => (
        <g key={t}>
          <line
            x1={PAD.left}
            x2={PAD.left + PLOT_W}
            y1={y(t)}
            y2={y(t)}
            stroke="var(--grid)"
            strokeWidth={1}
          />
          {/* Axis labels wear a MUTED INK token, never a series colour. */}
          <text
            x={PAD.left - 8}
            y={y(t)}
            dy={4}
            textAnchor="end"
            fontSize={11}
            fill="var(--text-muted)"
            style={{ fontVariantNumeric: "tabular-nums" }}
          >
            {formatMoney(asMoney(String(t)), { maximumFractionDigits: t < 1 ? 3 : 2 })}
          </text>
        </g>
      ))}

      {/* The baseline. One step darker than the gridlines so the zero line reads as the frame rather
          than as another gridline. */}
      <line
        x1={PAD.left}
        x2={PAD.left + PLOT_W}
        y1={y(0)}
        y2={y(0)}
        stroke="var(--axis)"
        strokeWidth={1}
      />

      {buckets.map((b, i) =>
        i % xStep === 0 ? (
          <text
            key={b}
            x={x(i)}
            y={VIEW_H - 8}
            textAnchor="middle"
            fontSize={11}
            fill="var(--text-muted)"
          >
            {formatBucket(b)}
          </text>
        ) : null,
      )}

      {series.map((s) => {
        /*
         * Each unbroken run of points becomes its own polyline, so a null leaves a REAL GAP.
         *
         * The alternative -- one polyline skipping nulls -- would draw a straight segment across the
         * missing bucket, which is a chart asserting a value nobody measured. A gap in the line is the
         * honest rendering of a gap in the data.
         */
        const runs: Point[][] = [];
        let run: Point[] = [];
        s.points.forEach((p) => {
          if (p === null) {
            if (run.length > 0) runs.push(run);
            run = [];
          } else {
            run.push(p);
          }
        });
        if (run.length > 0) runs.push(run);

        const lastIndex = s.points.reduce((acc, p, i) => (p !== null ? i : acc), -1);
        const last = lastIndex >= 0 ? s.points[lastIndex] : null;

        return (
          <g key={s.label}>
            {runs.map((r, ri) => {
              const idxOf = (p: Point) => s.points.findIndex((q) => q !== null && q.bucket === p.bucket);
              const d = r.map((p) => `${x(idxOf(p))},${y(p.value)}`).join(" ");
              return r.length === 1 ? (
                // A single isolated point cannot be a line, so it is drawn as a dot. Otherwise a
                // one-point series would be invisible -- which is exactly the case a brand-new
                // workload produces on its first day.
                <circle
                  key={ri}
                  cx={x(idxOf(r[0]!))}
                  cy={y(r[0]!.value)}
                  r={4}
                  fill={s.colour}
                  stroke="var(--surface-1)"
                  strokeWidth={2}
                />
              ) : (
                // 2px stroke, round join and cap. No fill.
                <polyline
                  key={ri}
                  points={d}
                  fill="none"
                  stroke={s.colour}
                  strokeWidth={2}
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
              );
            })}

            {last && (
              <>
                {/* The end marker: r=4 gives an 8px diameter, with a 2px SURFACE RING so it stays
                    legible where lines cross each other. The ring is surface-coloured rather than a
                    stroke around the mark -- a border would add data-weight ink that is not data. */}
                <circle
                  cx={x(lastIndex)}
                  cy={y(last.value)}
                  r={4}
                  fill={s.colour}
                  stroke="var(--surface-1)"
                  strokeWidth={2}
                />
                {/*
                 * The direct end-label: the relief the palette's contrast warning obligates.
                 *
                 * Three light-mode slots sit below 3:1 against the light surface (aqua 2.74, yellow
                 * 2.11, magenta 2.62), which requires visible labels or a table view. This provides
                 * the first; TrendChart provides the second. Neither is optional.
                 *
                 * SELECTIVE -- only the last point of each series. A value on every point is chaos and
                 * goes unread; direct labels work BECAUSE they are sparing.
                 *
                 * The text is an INK token, never the series colour: yellow at 2.11:1 is illegible as
                 * text. The coloured line arriving at the label is what carries identity.
                 */}
                {!labelsWouldCollide && (
                  <text
                    x={x(lastIndex) + 10}
                    y={y(last.value)}
                    dy={4}
                    fontSize={11}
                    fill="var(--text-secondary)"
                  >
                    {s.label.length > 16 ? `${s.label.slice(0, 15)}…` : s.label}
                  </text>
                )}
              </>
            )}
          </g>
        );
      })}
    </svg>
  );
}
