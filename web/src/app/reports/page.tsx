import { api, serverBaseURL } from "@/lib/api/client";
import { asMoney, formatMoney, formatQuantity } from "@/lib/money";
import { Card, EmptyState, Meter } from "@/components/figures";

/**
 * The monthly statements page.
 *
 * NOT "what is true now" -- what we SAID. Every other page here is a view of current data; a statement is
 * stored, because it must not change after it is issued. `finalised_at` is the line, and the UI has to
 * make that visible rather than presenting a frozen figure and a provisional one identically.
 *
 * COVERAGE IS THE POINT OF THIS PAGE'S LAYOUT
 * ==========================================
 * A month containing a collector outage produces a total that is confidently too low, and nothing about
 * the total itself reveals it. So coverage gets a METER beside every figure -- not a footnote, not a
 * detail panel. A ratio against a limit is exactly what a meter is for, and a two-slice pie would be
 * harder to read and need a legend.
 *
 * The meter also encodes that coverage is DAY-level: a day the collector ran for one hour counts as full,
 * so the warning threshold is high (0.9) rather than forgiving. The figure already flatters itself.
 */
export const dynamic = "force-dynamic";

export default async function ReportsPage() {
  const data = await api.monthlyReports({ limit: 200 }, serverBaseURL());
  const items = data.items ?? [];

  // Grouped by month so each statement period reads as one block, newest first. The API already orders
  // newest-first then largest-cost, so this preserves that order rather than re-sorting.
  const byMonth = new Map<string, typeof items>();
  for (const r of items) {
    const key = r.month ?? "unknown";
    byMonth.set(key, [...(byMonth.get(key) ?? []), r]);
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-lg font-semibold" style={{ color: "var(--text-primary)" }}>
          Monthly statements
        </h1>
        <p className="mt-1 text-sm" style={{ color: "var(--text-secondary)" }}>
          Frozen once signed off. A provisional statement is regenerated after every rollup; a finalised
          one is immutable, enforced by a database trigger rather than by whichever code last wrote it.
        </p>
      </div>

      {data.provisional_count > 0 && (
        <p className="text-xs" style={{ color: "var(--text-muted)" }}>
          {data.provisional_count} of {data.count} statements are still provisional and can change.
          Lowest coverage on this page: {data.lowest_coverage} — read that before quoting any total.
        </p>
      )}

      {items.length === 0 ? (
        <Card>
          <EmptyState
            message="No statements yet."
            hint="Statements are generated from the daily rollup. Run `make rollup-month MONTH=2026-07`, then `make rollup-close MONTH=2026-07` to freeze it."
          />
        </Card>
      ) : (
        [...byMonth.entries()].map(([month, rows]) => (
          <Card key={month} title={month} subtitle={`${rows.length} statements`}>
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="text-xs" style={{ color: "var(--text-muted)" }}>
                    <th scope="col" className="py-2 pr-4 text-left font-medium">Scope</th>
                    <th scope="col" className="py-2 pl-4 text-right font-medium">Cost</th>
                    <th scope="col" className="py-2 pl-4 text-right font-medium">Wasted CPU</th>
                    <th scope="col" className="w-40 py-2 pl-4 text-left font-medium">Coverage</th>
                    <th scope="col" className="py-2 pl-4 text-right font-medium">State</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((r, i) => (
                    <tr key={`${r.scope_kind}-${r.scope_value}-${i}`} className="border-t" style={{ borderColor: "var(--border)" }}>
                      <th scope="row" className="py-2 pr-4 text-left font-normal">
                        <span style={{ color: "var(--text-primary)" }}>{r.scope_value}</span>
                        <span className="ml-2 text-xs" style={{ color: "var(--text-muted)" }}>
                          {r.scope_kind}
                        </span>
                      </th>
                      <td className="tabular py-2 pl-4 text-right" style={{ color: "var(--text-primary)" }}>
                        {formatMoney(asMoney(r.total_cost ?? "0"))}
                      </td>
                      <td className="tabular py-2 pl-4 text-right" style={{ color: "var(--text-secondary)" }}>
                        {formatQuantity(asMoney(r.wasted_cpu_core_hours ?? "0"), "core-h")}
                      </td>
                      <td className="py-2 pl-4">
                        {/* Beside the figure, always. This is what stops somebody quoting a total from a
                            month the collector was down for. */}
                        <Meter ratio={r.coverage} label={`coverage for ${r.scope_value}`} />
                        <span className="text-xs" style={{ color: "var(--text-muted)" }}>
                          {r.days_with_data}/{r.days_in_month} days · {r.window_count} windows
                        </span>
                      </td>
                      <td className="py-2 pl-4 text-right text-xs">
                        {r.finalised_at ? (
                          <span style={{ color: "var(--text-secondary)" }}>
                            {/* "Frozen" rather than a green tick: a frozen statement is not "good", it is
                                unchangeable, and those are different claims. */}
                            frozen
                          </span>
                        ) : (
                          <span style={{ color: "var(--status-warning)" }}>provisional</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            {/*
             * The unattributed-spend note. The gap between a cluster statement and the sum of its team
             * statements IS cost whose containers carry no team label -- on this cluster, about half the
             * bill, because kube-system and monitoring are unlabelled. Worth naming, because a reader
             * comparing the two numbers will otherwise assume one of them is wrong.
             */}
            {rows.some((r) => r.scope_kind === "cluster") && rows.some((r) => r.scope_kind === "team") && (
              <p className="mt-3 text-xs" style={{ color: "var(--text-muted)" }}>
                The cluster total exceeds the sum of the team statements by the amount of unattributed
                spend — cost whose containers carry no team label. Those get no team statement rather than
                one belonging to nobody.
              </p>
            )}
          </Card>
        ))
      )}
    </div>
  );
}
