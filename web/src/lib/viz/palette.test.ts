import { describe, expect, it } from "vitest";
import { MAX_SERIES, SERIES_SLOTS, assignSlots, seriesLabel, severityOf } from "./palette";

describe("assignSlots", () => {
  it("is deterministic for a given set, regardless of input order", () => {
    // The property that makes a chart re-render without flickering. The API returns rows sorted by
    // cost, so the order this receives changes whenever the numbers move; if colour depended on
    // arrival order, every refresh would repaint the whole chart.
    const a = assignSlots(["team-search", "team-payments", "team-platform"]);
    const b = assignSlots(["team-platform", "team-search", "team-payments"]);
    expect([...a.entries()].sort()).toEqual([...b.entries()].sort());
  });

  it("gives every series a distinct colour up to MAX_SERIES", () => {
    // Two series sharing a colour is worse than any other failure here, because the chart is then
    // actively misleading rather than merely ugly: a legend says two things and the plot shows one.
    const names = Array.from({ length: MAX_SERIES }, (_, i) => `series-${i}`);
    const slots = new Set(assignSlots(names).values());
    expect(slots.size).toBe(MAX_SERIES);
  });

  it("deduplicates repeated names", () => {
    const slots = assignSlots(["a", "a", "b"]);
    expect(slots.size).toBe(2);
  });

  it("wraps rather than crashing beyond the slot count", () => {
    // Callers are supposed to fold the tail into "Other" before reaching this, but a function that
    // throws on its eighth argument is a landmine. Wrapping is a visible, survivable degradation.
    const names = Array.from({ length: SERIES_SLOTS.length + 2 }, (_, i) => `s${i}`);
    expect(() => assignSlots(names)).not.toThrow();
    expect(assignSlots(names).size).toBe(names.length);
  });

  it("REPAINTS survivors when the set changes, which is a known limitation", () => {
    // Asserted so the trade-off is on record rather than discovered by a reader.
    //
    // Slots are handed out by position in the SORTED set, so removing a series that sorts early
    // shifts every later one down a slot. The dataviz rule this bends is "colour follows the entity,
    // never its rank -- a filter that changes the series count must not repaint the survivors".
    //
    // WHY IT IS STILL THE RIGHT CHOICE HERE. The alternative is hashing the name to a slot, which
    // IS stable under filtering and buys that stability with collisions: with six slots, two of any
    // three series collide often enough to matter, and two series sharing a colour is a worse
    // failure than repainting. Positional assignment is collision-free by construction.
    //
    // The proper fix is neither: assign from the FULL set of known entities rather than the filtered
    // one, so the mapping is stable and collision-free. That needs the caller to know the full set,
    // which the current component tree does not. Recorded as the reason this test exists.
    const before = assignSlots(["alpha", "beta", "gamma"]);
    const after = assignSlots(["beta", "gamma"]);
    expect(after.get("beta")).not.toBe(before.get("beta"));
  });
});

describe("seriesLabel", () => {
  it("joins a multi-column grouping into one label", () => {
    // Grouping by workload needs three columns to be unambiguous, because two namespaces can each
    // have a Deployment called "api". The label has to carry all of them or the legend merges
    // unrelated services into one entry.
    expect(seriesLabel({ namespace: "team-search", kind: "Deployment", name: "api" }))
      .toBe("team-search · Deployment · api");
  });

  it("drops empty components rather than rendering separators around nothing", () => {
    // Denormalised dimension columns are empty strings, not nulls, when a pod has no owner. Without
    // this filter the legend reads "team-search ·  · " which looks like a rendering fault.
    expect(seriesLabel({ namespace: "team-search", kind: "", name: "" })).toBe("team-search");
  });

  it("says unknown rather than producing an empty legend entry", () => {
    expect(seriesLabel(undefined)).toBe("unknown");
    expect(seriesLabel({})).toBe("unknown");
    expect(seriesLabel({ a: "", b: "" })).toBe("unknown");
  });
});

describe("severityOf", () => {
  it("maps an unrecognised severity to a defined value rather than undefined", () => {
    // A severity arriving from the API that this palette does not know must still render. Returning
    // undefined would put `undefined` into a className and the row would lose its styling entirely,
    // which is how a critical finding ends up looking like a note.
    expect(severityOf("not-a-severity")).toBeDefined();
    expect(severityOf(undefined)).toBeDefined();
  });
});
