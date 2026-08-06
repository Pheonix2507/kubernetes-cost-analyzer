import type { ReactNode } from "react";
import { SEVERITY, type Severity } from "@/lib/viz/palette";
import { formatRatio } from "@/lib/money";

/**
 * Figures: the forms that are NOT charts.
 *
 * WHY THIS FILE EXISTS
 * ====================
 * The most common dashboard mistake is drawing a chart where a number would do. A single value gets a
 * one-bar bar chart; a ratio against a limit gets a two-slice pie; four headline figures get a grouped
 * bar chart nobody can read. Each of those is more ink, more code and less information than the figure
 * it replaced.
 *
 * These are the deliberate non-chart forms:
 *
 *   StatTile   a single current value, optionally with a delta      (not a one-bar chart)
 *   Meter      one ratio against a limit                            (not a pie of two slices)
 *   MagnitudeBar  comparison inside a table row                     (sequential, one hue)
 *   SeverityBadge status, with the icon it must never travel without
 *
 * All are Server Components -- no "use client", no hooks, no handlers. They render to HTML once and
 * ship zero JavaScript, which is the whole point of putting them in the server tree: a KPI row is
 * static, and hydrating it would cost bundle for no interactivity.
 */

// =============================================================================
// StatTile
// =============================================================================

/**
 * StatTile is a label, a value, and optionally a delta.
 *
 * WHY `hint` RATHER THAN A TOOLTIP. A tooltip needs JavaScript and hides the thing that explains the
 * number behind an interaction nobody performs. A cost figure that needs a caveat -- "this month is
 * 16% collected" -- must carry it in the open, because the caveat is what stops somebody quoting the
 * number.
 */
export function StatTile({
  label,
  value,
  unit,
  delta,
  hint,
  emphasis = false,
}: {
  label: string;
  value: string;
  unit?: string;
  /**
   * delta.direction says which way the number moved; delta.goodWhen says which way is GOOD.
   *
   * The two are separate because for cost they DISAGREE: a rise in spend is bad, so `direction: "up"`
   * with `goodWhen: "down"` renders in the warning colour rather than the success colour. A single
   * "isPositive" boolean conflates the arithmetic sign with the judgement, and gets cost dashboards
   * exactly backwards -- green for a bill going up.
   */
  delta?: { text: string; direction: "up" | "down" | "flat"; goodWhen: "up" | "down" };
  hint?: string;
  /** emphasis promotes the tile to the view's hero figure. Exactly one per view. */
  emphasis?: boolean;
}) {
  const deltaColour = (() => {
    if (!delta || delta.direction === "flat") return "var(--text-secondary)";
    return delta.direction === delta.goodWhen
      ? "var(--delta-good)"
      : "var(--status-serious)";
  })();

  const arrow = delta?.direction === "up" ? "↑" : delta?.direction === "down" ? "↓" : "→";

  return (
    <div
      className="rounded-lg border p-4"
      style={{ background: "var(--surface-1)", borderColor: "var(--border)" }}
    >
      {/* Sentence case, no trailing colon. Muted, because the label is the least interesting part of
          a tile and competing with the value for attention makes both harder to read. */}
      <div className="text-xs" style={{ color: "var(--text-muted)" }}>
        {label}
      </div>

      <div className="mt-1 flex items-baseline gap-1.5">
        {/*
         * The hero figure is >= 48px and stays in the SAME system sans as everything else. A display or
         * serif face here reads as decoration bolted onto a tool.
         *
         * Proportional figures, NOT tabular: at 48px, tabular-nums gives every digit the width of a
         * zero and a value like 121 looks visibly loose. Tabular is for columns that must align.
         */}
        <span
          className={emphasis ? "text-5xl font-semibold" : "text-2xl font-semibold"}
          style={{ color: "var(--text-primary)" }}
        >
          {value}
        </span>
        {unit && (
          <span className="text-sm" style={{ color: "var(--text-secondary)" }}>
            {unit}
          </span>
        )}
      </div>

      {delta && (
        <div className="mt-1.5 text-xs" style={{ color: deltaColour }}>
          {/* The arrow is the secondary channel. A delta that were colour-only would be unreadable to
              a reader who cannot separate the success green from the serious orange. */}
          <span aria-hidden="true">{arrow} </span>
          {delta.text}
        </div>
      )}

      {hint && (
        <div className="mt-2 text-xs leading-snug" style={{ color: "var(--text-muted)" }}>
          {hint}
        </div>
      )}
    </div>
  );
}

// =============================================================================
// Meter
// =============================================================================

/**
 * Meter shows one ratio against its limit.
 *
 * A two-slice pie is the usual mistake here, and it is worse in every way: an angle is harder to judge
 * than a length, it needs a legend, and it cannot be read at a glance in a table row.
 *
 * THE TRACK IS A LIGHTER STEP OF THE FILL'S OWN RAMP -- blue-on-blue -- rather than a neutral grey.
 * Same-ramp means the state reads across the whole bar: the eye compares fill to track as one object
 * instead of reading a coloured bar sitting on unrelated furniture.
 *
 * Coverage is the reason this component exists. A monthly statement with 0.0968 coverage carries a
 * total that is confidently too low, and nothing about the total reveals it -- so the meter goes beside
 * the figure, not in a detail panel.
 */
export function Meter({
  ratio,
  label,
  warnBelow = 0.9,
}: {
  /** An exact decimal string, as the API sends it. */
  ratio: string | null | undefined;
  label?: string;
  /**
   * warnBelow is where the fill switches to a status colour.
   *
   * 0.9 rather than something lower, because coverage is DAY-level: a day the collector ran for one
   * hour counts as a full day, so the figure already flatters itself. A threshold that tolerated 70%
   * would be tolerating an unknown amount worse than 70%.
   */
  warnBelow?: number;
}) {
  const n = Number(ratio ?? 0);
  const pct = Number.isFinite(n) ? Math.max(0, Math.min(1, n)) : 0;

  // Severity by threshold, and the fill carries it. Below a third is not "a bit incomplete" -- it is a
  // number that should not be quoted, so it gets the critical step rather than the warning one.
  const fill =
    pct < warnBelow / 3
      ? "var(--status-critical)"
      : pct < warnBelow
        ? "var(--status-warning)"
        : "var(--seq-400)";

  return (
    <div className="flex items-center gap-2">
      <div
        className="relative h-1.5 w-full overflow-hidden rounded-full"
        style={{ background: "var(--seq-100)" }}
        role="meter"
        aria-valuenow={Math.round(pct * 100)}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label={label ?? "coverage"}
      >
        <div
          className="absolute inset-y-0 left-0 rounded-full"
          style={{ width: `${pct * 100}%`, background: fill }}
        />
      </div>
      {/* The number is always shown beside the bar. A meter whose value can only be estimated from a
          length is a meter that gets estimated wrongly, and this one gates whether a total is quotable. */}
      <span className="tabular shrink-0 text-xs" style={{ color: "var(--text-secondary)" }}>
        {formatRatio(ratio)}
      </span>
    </div>
  );
}

// =============================================================================
// MagnitudeBar
// =============================================================================

/**
 * MagnitudeBar is the inline comparison cue in a table row.
 *
 * SEQUENTIAL, ONE HUE. The job is magnitude -- low to high -- and sequential is the safe default for
 * that: it stays legible, it is consistent, and it is hard to misread. Categorical colour here would be
 * actively wrong, because it would imply the rows are different KINDS of thing when they differ only in
 * size.
 *
 * The bar is 6px and sits under the value rather than replacing it. It is a reading aid for the shape
 * of a column, not the data: the number is the data, and it is always present.
 */
export function MagnitudeBar({ fraction }: { fraction: number }) {
  const pct = Math.max(0, Math.min(1, Number.isFinite(fraction) ? fraction : 0));
  return (
    // NO TRACK, and a screenshot is why.
    //
    // The first version drew the fill over a `--seq-100` track. In dark mode that step is #0d366b -- a
    // solid dark blue -- and for local-path-storage, whose bar is 0.16% wide, the only thing visible
    // was the track. It read as a FULL bar for the cheapest namespace in the table: the exact opposite
    // of what the cue exists to say. I misread my own screenshot the same way before measuring the
    // rendered widths.
    //
    // A track belongs on a METER, where there is a limit and the unfilled remainder means something
    // ("70% of the month collected"). A magnitude bar compares rows and has no limit, so the remainder
    // means nothing and drawing it is data-weight ink that is not data. Removing it also makes a
    // near-zero row look near-zero, which is the whole point.
    <div className="mt-1 h-1.5 w-full" aria-hidden="true">
      <div
        className="h-full"
        // 4px rounded data-end, square at the baseline, so the bar reads as growing from the left edge
        // rather than floating in the row.
        style={{
          width: `${pct * 100}%`,
          background: "var(--seq-400)",
          borderRadius: "0 4px 4px 0",
        }}
      />
    </div>
  );
}

// =============================================================================
// SeverityBadge
// =============================================================================

/**
 * SeverityBadge renders a status with the icon and label it must never travel without.
 *
 * THE ICON IS NOT OPTIONAL, and this component exists to make that structural rather than a convention.
 * On the light surface, warning measures 1.79:1 against the chart surface and serious 2.57:1 -- both
 * deliberately below 3:1 -- so hue cannot carry the meaning alone. A caller cannot render a bare
 * coloured dot here even by accident, because there is no prop for it.
 *
 * Note the TEXT is not in the status colour. The glyph beside it carries the identity; colouring the
 * word "Warning" in #fab219 would put 1.79:1 text on the surface, which is unreadable for exactly the
 * readers the icon was added for.
 */
export function SeverityBadge({ severity }: { severity: Severity }) {
  const s = SEVERITY[severity];
  return (
    <span className="inline-flex items-center gap-1.5 text-xs" title={s.meaning}>
      <span aria-hidden="true" style={{ color: s.colour }}>
        {s.icon}
      </span>
      <span style={{ color: "var(--text-secondary)" }}>{s.label}</span>
    </span>
  );
}

// =============================================================================
// Shared layout
// =============================================================================

/** Card is the standard panel: hairline ring, chart surface, no shadow. */
export function Card({
  title,
  subtitle,
  actions,
  children,
}: {
  title?: string;
  subtitle?: string;
  actions?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section
      className="rounded-lg border"
      style={{ background: "var(--surface-1)", borderColor: "var(--border)" }}
    >
      {(title || actions) && (
        <header
          className="flex flex-wrap items-baseline justify-between gap-2 border-b px-4 py-3"
          style={{ borderColor: "var(--border)" }}
        >
          <div>
            {title && (
              <h2 className="text-sm font-semibold" style={{ color: "var(--text-primary)" }}>
                {title}
              </h2>
            )}
            {/* The subtitle names what is plotted, which is why a single-series chart needs no legend:
                a one-swatch legend box restates the title and costs space. */}
            {subtitle && (
              <p className="mt-0.5 text-xs" style={{ color: "var(--text-muted)" }}>
                {subtitle}
              </p>
            )}
          </div>
          {actions}
        </header>
      )}
      <div className="p-4">{children}</div>
    </section>
  );
}

/** EmptyState is what a panel shows instead of an empty chart. */
export function EmptyState({ message, hint }: { message: string; hint?: string }) {
  return (
    <div className="py-8 text-center">
      <p className="text-sm" style={{ color: "var(--text-secondary)" }}>
        {message}
      </p>
      {hint && (
        <p className="mx-auto mt-1 max-w-md text-xs" style={{ color: "var(--text-muted)" }}>
          {hint}
        </p>
      )}
    </div>
  );
}
