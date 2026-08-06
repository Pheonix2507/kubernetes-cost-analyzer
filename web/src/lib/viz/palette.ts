/**
 * Palette assignment.
 *
 * WHY THIS FILE EXISTS -- ONE RULE THAT IS EASY TO STATE AND EASY TO BREAK
 * ======================================================================
 * **Colour follows the entity, never its rank.**
 *
 * The natural Recharts code breaks it on the first line:
 *
 *     series.map((s, i) => <Line stroke={PALETTE[i]} />)   // WRONG
 *
 * That assigns by ARRAY POSITION. The API returns series ordered by cost, so the moment a filter
 * changes which groups are present -- or one team's spend overtakes another's -- every surviving
 * series is repainted. A reader who has learned "the orange line is team-search" now finds orange
 * means team-platform, and they will not notice: the chart still looks correct.
 *
 * It is the visual equivalent of a non-deterministic query. The fix is to derive the slot from the
 * group's NAME, so a given namespace has one colour for the life of the dashboard regardless of what
 * else is on screen.
 */

/**
 * SERIES_SLOTS are the categorical slots, IN FIXED ORDER.
 *
 * CSS variables rather than hex, so light and dark swap underneath without this module knowing which
 * mode is active. Recharts writes these straight into `stroke`, and `stroke="var(--series-1)"` is
 * valid SVG.
 *
 * THE ORDER IS THE COLOURBLIND-SAFETY MECHANISM, not an aesthetic sequence. Candidate orderings were
 * enumerated and only those clearing every adjacent-pair gate in both modes were kept; this is one of
 * them. Reordering to "look nicer" would silently break a validated property -- notably, moving
 * yellow next to orange fails the floors outright.
 */
export const SERIES_SLOTS = [
  "var(--series-1)", // blue
  "var(--series-2)", // orange
  "var(--series-3)", // aqua
  "var(--series-4)", // yellow
  "var(--series-5)", // magenta
  "var(--series-6)", // green
] as const;

/**
 * MAX_SERIES caps how many lines a chart will draw.
 *
 * SIX, where the API's own default limit is twenty. That difference is deliberate and is a UI decision
 * rather than an API one: twenty categorical lines is not a chart anybody can read, and no palette
 * fixes it -- a generated ninth hue is indistinguishable from an existing one under CVD and breaks
 * every check. Past the cap the honest moves are to fold the tail into "Other", facet into small
 * multiples, or show the table.
 *
 * This app asks the API for six and says so on screen, rather than requesting twenty and quietly
 * drawing part of the answer.
 */
export const MAX_SERIES = 6;

/**
 * assignSlots maps group names to colour slots, stably.
 *
 * Assignment is by SORTED NAME, not by the order the API returned. Sorting makes it a pure function
 * of the SET of names present: the same set always yields the same mapping, whatever order it arrived
 * in and however it was sorted for display.
 *
 * THE HONEST LIMITATION, stated rather than hidden: the mapping depends on the set, so adding a
 * seventh namespace can shift the slot of an existing one. A truly stable scheme would hash the name
 * to a slot -- and hashing collides, so two namespaces on screen together could land the same colour,
 * which is worse than a shift. Between "colours may shift when the cluster's namespace list changes"
 * and "two series may be indistinguishable", the first is the better failure: it is rare, visible, and
 * does not make the chart lie.
 *
 * What this DOES guarantee is the case that actually bites: re-sorting, filtering by date, or one
 * series overtaking another never repaints anything.
 */
export function assignSlots(names: readonly string[]): Map<string, string> {
  const sorted = [...new Set(names)].sort((a, b) => a.localeCompare(b));
  const out = new Map<string, string>();
  sorted.forEach((name, i) => {
    // Modulo is a deliberate last resort and should be unreachable: callers cap at MAX_SERIES before
    // they get here. Cycling would put two series in the same colour, so if this ever wraps the bug is
    // upstream -- the cap was not applied.
    out.set(name, SERIES_SLOTS[i % SERIES_SLOTS.length]!);
  });
  return out;
}

/**
 * seriesLabel renders a group object as a single readable string.
 *
 * The API returns `group` as a map of dimension column to value, because a series is IDENTIFIED by its
 * group rather than described by it -- see the note on TrendSeries in the Go repository. A workload
 * grouping carries three columns (namespace, kind, name) precisely so two namespaces' identically
 * named Deployments are not merged, and a legend needs one string.
 *
 * Joined with a middle dot rather than a slash: a Kubernetes name may contain neither, but a slash
 * reads as a path and invites the guess that this is a URL.
 */
export function seriesLabel(group: Record<string, string> | undefined): string {
  if (!group) return "unknown";
  const values = Object.values(group).filter((v) => v !== "");
  return values.length > 0 ? values.join(" · ") : "unknown";
}

/**
 * SEVERITY carries the reserved status colours, with the icon each must travel with.
 *
 * STATUS COLOURS ARE NEVER USED AS SERIES COLOURS, and these four steps are deliberately distinct from
 * the categorical slots so a status cannot impersonate a series.
 *
 * The icon is not decoration and is not optional. On the light surface, warning measures 1.79:1 and
 * serious 2.57:1 against the chart surface -- both under 3:1 by design -- so hue alone cannot carry
 * the meaning. Pairing every status with a glyph AND a text label is the mitigation, which is why the
 * icon lives in this table rather than being left to each caller to remember.
 *
 * `info` maps to the neutral secondary ink rather than to a status colour, because it is the absence
 * of urgency rather than a state. Painting it green would read as "good", which an informational
 * right-sizing finding is not -- it is money being wasted, just not dangerously.
 */
export const SEVERITY = {
  critical: {
    colour: "var(--status-critical)",
    icon: "▲",
    label: "Critical",
    /** Reliability risk. Acting on it usually INCREASES cost, which is the correct trade. */
    meaning: "reliability risk — act on this even though it costs more",
  },
  warning: {
    colour: "var(--status-warning)",
    icon: "◆",
    label: "Warning",
    meaning: "needs a human decision before acting",
  },
  info: {
    colour: "var(--text-secondary)",
    icon: "●",
    label: "Info",
    meaning: "safe to apply — a straightforward saving",
  },
} as const;

export type Severity = keyof typeof SEVERITY;

/** severityOf narrows an API string to a known severity, defaulting to info. */
export function severityOf(raw: string | undefined): Severity {
  return raw === "critical" || raw === "warning" ? raw : "info";
}
