import Link from "next/link";
import { api, serverBaseURL, type Recommendation } from "@/lib/api/client";
import { absMoney, asMoney, formatMoney, formatQuantity, isNegative, toPlotValue, type Money } from "@/lib/money";
import { severityOf } from "@/lib/viz/palette";
import { Card, EmptyState, SeverityBadge, StatTile } from "@/components/figures";
import { TrendChart } from "@/components/TrendChart";
import { resolveRange } from "@/lib/range";

/**
 * The overview.
 *
 * A SERVER COMPONENT, AND THAT IS THE POINT
 * =========================================
 * No "use client" anywhere in this file. Everything here runs on the server, which buys three things:
 *
 *   1. THE API KEY STAYS SERVER-SIDE. This page fetches through the proxy, which attaches the
 *      credential. The browser receives rendered HTML and never sees a token.
 *   2. NO LOADING SPINNER ON FIRST PAINT. The data is already in the markup, so there is no
 *      fetch-then-render round trip and no layout shift when numbers arrive.
 *   3. ALMOST NO JAVASCRIPT. The only client component on this page is TrendChart, because Recharts
 *      needs the DOM for its crosshair. The stat tiles and the recommendation list ship as HTML.
 *
 * WHY NO TanStack Query HERE
 * Nothing on this page changes without a navigation: there are no filters and no polling. Query's value
 * is caching across interactions, and an interaction-free page has none to cache. Adding it would make
 * the page a Client Component and ship a library to render static numbers -- which is the mistake this
 * split exists to avoid. The /costs page is where Query earns its place.
 *
 * `force-dynamic` because every fetch is per-request and time-dependent. Without it, Next.js would try
 * to statically prerender this at build time -- when there is no API to reach -- and a cost dashboard
 * baked at build time would show the same numbers forever.
 */
export const dynamic = "force-dynamic";

export default async function OverviewPage() {
  const base = serverBaseURL();
  const day = resolveRange("24h");
  const month = resolveRange("30d");

  /**
   * THREE REQUESTS IN PARALLEL, not sequentially awaited.
   *
   * The naive version costs the sum of the latencies:
   *
   *     const summary = await api.costSummary(...);   // 40ms
   *     const trend   = await api.trend(...);         // 60ms   -> 140ms total
   *     const recs    = await api.recommendations(...);// 40ms
   *
   * They are independent, so Promise.all makes it the MAX instead -- 60ms. On a server-rendered page
   * this is the whole time-to-first-byte, and sequential awaits are the most common reason an RSC page
   * feels slower than the client-rendered version it replaced.
   *
   * allSettled rather than all, deliberately. `all` rejects on the first failure, so a broken
   * recommendations query would blank the entire page including the cost figures that loaded fine. This
   * is the same reasoning internal/costing applies to a failing namespace: partial coverage that reports
   * its gaps beats a total blank.
   */
  const [summaryResult, trendResult, recsResult] = await Promise.allSettled([
    api.costSummary({ ...day, group_by: "namespace", limit: 100 }, base),
    api.trend({ ...month, interval: "day", group_by: "namespace", limit: 1 }, base),
    api.recommendations({ ...resolveRange("7d"), limit: 200 }, base),
  ]);

  const summary = summaryResult.status === "fulfilled" ? summaryResult.value : null;
  const trend = trendResult.status === "fulfilled" ? trendResult.value : null;
  const recs = recsResult.status === "fulfilled" ? recsResult.value : null;

  const totals = summary?.totals;
  const recTotals = recs?.totals;

  /**
   * The top findings, by SEVERITY then saving.
   *
   * The API already returns them in that order -- severity first, then saving within a severity -- so
   * this slices rather than re-sorts. Re-sorting by saving alone would put a $9 right-sizing above a
   * critical OOM risk, which is precisely the ordering the Go side went out of its way to avoid.
   */
  const topFindings: Recommendation[] = (recs?.items ?? []).slice(0, 5);

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-lg font-semibold" style={{ color: "var(--text-primary)" }}>
          Overview
        </h1>
        <p className="mt-1 text-sm" style={{ color: "var(--text-secondary)" }}>
          Spend over the last 24 hours, and what to change about it.
        </p>
      </div>

      {summaryResult.status === "rejected" && (
        <Card>
          <EmptyState
            message="Cost data is unavailable."
            hint={
              summaryResult.reason instanceof Error
                ? summaryResult.reason.message
                : "The cost API did not respond."
            }
          />
        </Card>
      )}

      {/*
       * A KPI ROW, not a grouped bar chart.
       *
       * Four headline numbers of different kinds -- money, a quantity, money again, a count -- have no
       * common scale, so a bar chart of them would compare things that cannot be compared and force a
       * dual axis to try. Tiles are the correct form for "a handful of headline numbers".
       */}
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <StatTile
          label="Cost, last 24 hours"
          value={formatMoney(asMoney(totals?.total_cost ?? "0"))}
          // The hero figure: exactly one per view, and it is the number this dashboard exists to
          // report.
          emphasis
          hint={
            totals?.estimated_rates
              ? "Includes at least one node priced from a fallback rate, so this is an estimate."
              : undefined
          }
        />

        <StatTile
          label="Wasted CPU"
          value={formatQuantity(asMoney(totals?.wasted_cpu_core_hours ?? "0"), "core-h")}
          hint="Reserved but not used. Floored per container, so an under-requested workload never credits against real waste."
        />

        <StatTile
          label="Available to save"
          value={formatMoney(asMoney(recTotals?.potential_monthly_saving ?? "0"))}
          unit="/month"
          hint="From safe findings only. Reliability fixes that cost money are counted separately."
        />

        {/*
         * The required increase is its OWN tile rather than netted against the saving.
         *
         * Netting them would let a large right-sizing win cancel a memory increase somebody MUST make,
         * and the tile would read "saving $30" with the reliability fix invisible inside it. The API
         * returns two separate totals for exactly this reason; the UI must not undo that by adding them.
         */}
        <StatTile
          label="Needed for reliability"
          value={formatMoney(asMoney(recTotals?.required_monthly_increase ?? "0"))}
          unit="/month"
          delta={
            recTotals?.critical
              ? {
                  text: `${recTotals.critical} critical finding${recTotals.critical === 1 ? "" : "s"}`,
                  direction: "up",
                  // Up is BAD for a bill, so this renders in the serious colour rather than the success
                  // green. See the note on StatTile's delta prop.
                  goodWhen: "down",
                }
              : undefined
          }
          hint="Under-requested memory risks an OOM kill. Acting on these correctly increases spend."
        />
      </div>

      {/*
       * ONE series, so NO LEGEND -- the card title and subtitle say what is plotted, and a one-swatch
       * legend box would restate the title and cost a row.
       *
       * `limit: 1` asks the API for the single largest namespace rather than for a total, which is an
       * honest limitation worth naming in the subtitle rather than mislabelling the line as "total".
       */}
      <Card
        title="Daily cost, largest namespace"
        subtitle={
          trend
            ? `${trend.series?.[0]?.group ? Object.values(trend.series[0].group).join(" · ") : "no data"} — from the ${trend.source === "daily_rollup" ? "daily rollup" : "raw samples"}`
            : "unavailable"
        }
        actions={
          <Link href="/trends" className="text-xs hover:underline" style={{ color: "var(--text-secondary)" }}>
            All series →
          </Link>
        }
      >
        {trend?.series?.length ? (
          <TrendChart series={trend.series} interval="day" />
        ) : (
          <EmptyState
            message="No daily data yet."
            hint="The rollup runs on complete days. Run `make rollup-backfill` to build it from the samples already collected."
          />
        )}
      </Card>

      <Card
        title="What to change"
        subtitle="Most urgent first — severity, then saving"
        actions={
          <Link
            href="/recommendations"
            className="text-xs hover:underline"
            style={{ color: "var(--text-secondary)" }}
          >
            All {recs?.count ?? 0} →
          </Link>
        }
      >
        {topFindings.length === 0 ? (
          <EmptyState
            message="No findings."
            hint="Either the estate is well sized, or there is not yet enough history — the engine requires a minimum observation span before it will advise anything."
          />
        ) : (
          <ul className="divide-y" style={{ borderColor: "var(--border)" }}>
            {topFindings.map((r, i) => {
              const saving = asMoney(r.estimated_monthly_saving ?? "0");
              return (
                <li key={`${r.workload_name}-${r.kind}-${i}`} className="flex flex-wrap gap-x-4 gap-y-1 py-2.5">
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-x-2">
                      {/* Status colour never travels alone: SeverityBadge pairs it with a glyph and a
                          word, because warning measures 1.79:1 on the light surface. */}
                      <SeverityBadge severity={severityOf(r.severity)} />
                      <span className="text-sm" style={{ color: "var(--text-primary)" }}>
                        {r.workload_name}
                      </span>
                      <span className="text-xs" style={{ color: "var(--text-muted)" }}>
                        {r.namespace}
                      </span>
                    </div>
                    <p className="mt-0.5 text-xs" style={{ color: "var(--text-secondary)" }}>
                      {r.summary}
                    </p>
                  </div>
                  <SavingFigure amount={saving} />
                </li>
              );
            })}
          </ul>
        )}
      </Card>

      {/*
       * The provenance footer.
       *
       * Not decoration. `analysed_containers` is the denominator for every finding count above: three
       * recommendations across four containers and three across four thousand are completely different
       * situations, and the second looks like good news without this line.
       */}
      {recs && (
        <p className="text-xs" style={{ color: "var(--text-muted)" }}>
          {recs.count} finding{recs.count === 1 ? "" : "s"} across {recs.analysed_containers} analysed
          container{recs.analysed_containers === 1 ? "" : "s"}
          {recTotals?.estimated_rates && " · some savings rest on fallback rates and are estimates"}
          {summary?.totals?.truncated && " · the cost summary was truncated"}
          {" · "}
          {formatQuantity(asMoney(totals?.cpu_billable_core_hours ?? "0"), "core-h")} billed
          {" · "}
          {toPlotValue(asMoney(totals?.total_cost ?? "0")) === 0 &&
            "zero cost usually means the collector has not run — try `make run-collector`"}
        </p>
      )}
    </div>
  );
}

/**
 * SavingFigure renders a monthly saving, a required increase, or neither.
 *
 * WHY THIS IS A COMPONENT AND NOT AN INLINE TERNARY
 * ================================================
 * A screenshot of the overview showed `−0.00 per month` against two warning findings. Those are
 * `set_requests` recommendations, whose saving is exactly ZERO: declaring a request on a BestEffort
 * container does not reduce the bill, it makes the cost attributable to whoever caused it. So the
 * figure is genuinely nothing, and the inline `costsMoney ? "+" : "−"` rendered it as a minus sign on
 * a rounded zero -- which reads as a tiny saving rather than as no saving at all.
 *
 * THREE STATES, not two, and that is the whole point:
 *
 *   positive   a saving        shown in the success ink, prefixed −  (the bill goes DOWN)
 *   negative   a cost          shown in the serious ink, prefixed +  (the bill goes UP)
 *   zero       neither         shown as text, because a signed zero is a lie about direction
 *
 * The two-state version was found by looking at a screenshot rather than by any check, because
 * "−0.00" is a perfectly valid string that no type or test was asserting about.
 */
function SavingFigure({ amount }: { amount: Money }) {
  const n = Number(amount);
  const isZero = !Number.isFinite(n) || n === 0;
  const costs = isNegative(amount);

  if (isZero) {
    return (
      <div className="text-right">
        <div className="text-sm" style={{ color: "var(--text-secondary)" }}>
          no direct saving
        </div>
        <div className="text-xs" style={{ color: "var(--text-muted)" }}>
          makes cost attributable
        </div>
      </div>
    );
  }

  return (
    <div className="text-right">
      <div
        className="tabular text-sm font-medium"
        style={{ color: costs ? "var(--status-serious)" : "var(--delta-good)" }}
      >
        {/* A negative saving reads as a COST with a plus sign, not as a smaller saving. They are
            opposite actions and must not look alike. */}
        {costs ? "+" : "−"}
        {formatMoney(absMoney(amount))}
      </div>
      <div className="text-xs" style={{ color: "var(--text-muted)" }}>
        per month
      </div>
    </div>
  );
}
