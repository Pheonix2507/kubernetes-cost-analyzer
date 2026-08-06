import { api, serverBaseURL } from "@/lib/api/client";
import { absMoney, asMoney, formatMoney, isNegative, type Money } from "@/lib/money";
import { SEVERITY, severityOf } from "@/lib/viz/palette";
import { Card, EmptyState, SeverityBadge, StatTile } from "@/components/figures";
import { defaultRangeFor, resolveRange } from "@/lib/range";

/**
 * The recommendations page.
 *
 * A pure Server Component: no filters, no polling, so nothing to cache and no reason to ship JavaScript.
 * The whole page is HTML.
 *
 * THE ONE DESIGN RULE THAT MATTERS HERE
 * =====================================
 * Savings and required increases are NEVER netted, and never rendered in a way that invites a reader to
 * add them. The API returns two separate totals for exactly this reason: a single net figure would let a
 * large right-sizing win cancel a memory increase somebody MUST make, and the page would read "saving
 * $30" with the reliability fix invisible inside it.
 *
 * So they are two tiles, in different colours, with the increase labelled as something you SPEND.
 */
export const dynamic = "force-dynamic";

export default async function RecommendationsPage() {
  const range = resolveRange(defaultRangeFor("recommendations"));
  const data = await api.recommendations({ ...range, limit: 200 }, serverBaseURL());

  const totals = data.totals;
  const items = data.items ?? [];

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-lg font-semibold" style={{ color: "var(--text-primary)" }}>
          Recommendations
        </h1>
        <p className="mt-1 text-sm" style={{ color: "var(--text-secondary)" }}>
          Right-sizing uses the 95th percentile of per-window PEAKS, not the average — sizing a request on
          the average guarantees throttling, because an average sits below half the observations by
          definition.
        </p>
      </div>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <StatTile
          label="Available to save"
          value={formatMoney(asMoney(totals.potential_monthly_saving ?? "0"))}
          unit="/month"
          emphasis
          hint="Positive savings only. Never netted against the figure beside it."
        />
        <StatTile
          label="Needed for reliability"
          value={formatMoney(asMoney(totals.required_monthly_increase ?? "0"))}
          unit="/month"
          hint="Under-requested memory risks an OOM kill. Memory is incompressible — the kernel kills rather than throttles."
        />
        <StatTile
          label="Findings"
          value={String(data.count)}
          hint={`across ${data.analysed_containers} analysed containers — the denominator that makes this number mean anything`}
        />
        <StatTile
          label="Critical"
          value={String(totals.critical)}
          hint="Reliability risk. Acting on these correctly increases spend."
        />
      </div>

      <Card title="Findings" subtitle="Most urgent first — severity, then saving. Not sorted by money.">
        {items.length === 0 ? (
          <EmptyState
            message="No findings."
            hint="The engine requires a minimum window count, a minimum observation SPAN and peak coverage before it will advise anything. A hundred windows over one hour still only describes that hour."
          />
        ) : (
          <ul className="divide-y" style={{ borderColor: "var(--border)" }}>
            {items.map((r, i) => {
              const saving = asMoney(r.estimated_monthly_saving ?? "0");
              const sev = severityOf(r.severity);
              return (
                <li key={`${r.namespace}-${r.workload_name}-${r.kind}-${i}`} className="py-3">
                  <div className="flex flex-wrap items-start gap-x-4 gap-y-2">
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                        <SeverityBadge severity={sev} />
                        {/* The kind, as a plain word rather than a coloured chip. Reserving colour for
                            severity means a reader never has to work out which coloured thing is the
                            urgent one. */}
                        <code className="text-xs" style={{ color: "var(--text-muted)" }}>
                          {r.kind}
                        </code>
                        <span className="text-sm font-medium" style={{ color: "var(--text-primary)" }}>
                          {r.workload_name}
                        </span>
                        {r.container_name && (
                          <span className="text-xs" style={{ color: "var(--text-muted)" }}>
                            / {r.container_name}
                          </span>
                        )}
                        <span className="text-xs" style={{ color: "var(--text-muted)" }}>
                          in {r.namespace}
                        </span>
                      </div>

                      <p className="mt-1 text-sm" style={{ color: "var(--text-secondary)" }}>
                        {r.summary}
                      </p>

                      {/* The rationale, WITH the numbers. A recommendation nobody understands is one
                          nobody applies, and one that cannot be argued with cannot be trusted either. */}
                      <p className="mt-1 text-xs leading-relaxed" style={{ color: "var(--text-muted)" }}>
                        {r.rationale}
                      </p>

                      {(r.current || r.proposed) && (
                        <p className="mt-1.5 text-xs">
                          <span style={{ color: "var(--text-muted)" }}>{r.current}</span>
                          <span aria-hidden="true" style={{ color: "var(--text-muted)" }}> → </span>
                          {/* Formatted in Kubernetes units so it can be pasted into a manifest
                              unchanged. */}
                          <code style={{ color: "var(--text-primary)" }}>{r.proposed}</code>
                        </p>
                      )}
                    </div>

                    <div className="text-right">
                      <SavingFigure amount={saving} />
                      {r.estimated_rates && (
                        <div className="mt-1 text-xs" style={{ color: "var(--text-muted)" }}>
                          estimated rate
                        </div>
                      )}
                      {r.confidence && (
                        <div className="mt-1 text-xs" style={{ color: "var(--text-muted)" }}>
                          {r.confidence} confidence
                        </div>
                      )}
                    </div>
                  </div>
                </li>
              );
            })}
          </ul>
        )}
      </Card>

      {/* The legend for the status colours, because a colour whose meaning is only in a tooltip is a
          colour with no meaning. */}
      <div className="flex flex-wrap gap-x-6 gap-y-1 text-xs" style={{ color: "var(--text-muted)" }}>
        {(["critical", "warning", "info"] as const).map((k) => (
          <span key={k} className="flex items-center gap-1.5">
            <span aria-hidden="true" style={{ color: SEVERITY[k].colour }}>{SEVERITY[k].icon}</span>
            <span>{SEVERITY[k].label} — {SEVERITY[k].meaning}</span>
          </span>
        ))}
      </div>
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
