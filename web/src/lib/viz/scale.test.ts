import { describe, expect, it } from "vitest";
import { niceTicks } from "./scale";

/**
 * The y-axis scale, which is load-bearing rather than decorative: LineChartSVG divides by the LAST
 * tick to place every point, so a top tick below the data maximum plots those points outside the
 * frame and they are clipped away.
 */

describe("niceTicks", () => {
  it("always reaches or exceeds the maximum", () => {
    // THE INVARIANT. Stated as a property over a wide range rather than as examples, because the
    // failure only appears for values more than half a step above a round number -- exactly the
    // band a handful of hand-picked cases walks straight past.
    const values = [
      0.0001, 0.0007, 0.001, 0.0155, 0.05, 0.0999, 0.1, 0.1155, 0.15, 0.9, 1, 1.01, 2.5, 7, 9.9,
      10, 42, 99.9, 100, 12345, 0.3882, 12.51, 59.38,
    ];
    for (const max of values) {
      const ticks = niceTicks(max);
      const top = ticks[ticks.length - 1]!;
      expect(top, `top tick ${top} must cover max ${max}, or points above it get clipped`)
        .toBeGreaterThanOrEqual(max);
    }
  });

  it("covers 0.1155, the value that exposed the bug", () => {
    // A real figure from the demo cluster: kube-system cost 0.1155 a day for five consecutive days.
    // The old implementation returned [0, 0.05, 0.1] and those five days disappeared from the chart.
    const ticks = niceTicks(0.1155);
    expect(ticks[ticks.length - 1]).toBeGreaterThanOrEqual(0.1155);
    expect(ticks).toContain(0);
  });

  it("starts at zero, because a truncated axis exaggerates change", () => {
    // A cost chart whose axis starts at 0.09 turns a 2% rise into a cliff. Money charts start at
    // zero or they mislead.
    for (const max of [0.5, 5, 500]) {
      expect(niceTicks(max)[0]).toBe(0);
    }
  });

  it("uses only 1/2/5 multiples, which are the ones humans read without arithmetic", () => {
    for (const max of [0.1155, 3, 7, 42, 880]) {
      const ticks = niceTicks(max);
      const step = ticks[1]! - ticks[0]!;
      // Normalise the step to its leading digit and check it is 1, 2 or 5.
      const mag = 10 ** Math.floor(Math.log10(step));
      const lead = Math.round(step / mag);
      expect([1, 2, 5, 10], `step ${step} for max ${max}`).toContain(lead);
    }
  });

  it("keeps the tick count small enough to read", () => {
    // Ceiling the top tick adds at most one, so the axis cannot grow unbounded.
    for (const max of [0.1155, 1, 9.9, 12345]) {
      expect(niceTicks(max).length).toBeLessThanOrEqual(7);
    }
  });

  it("degrades to a single tick for an empty or zero series rather than dividing by zero", () => {
    // An all-zero series is legitimate: a namespace that existed and cost nothing. Returning [0]
    // means LineChartSVG falls back to a scale of 1 instead of producing NaN coordinates, which
    // would blank the whole chart.
    expect(niceTicks(0)).toEqual([0]);
    expect(niceTicks(-1)).toEqual([0]);
  });

  it("does not emit floating point noise in the labels", () => {
    // 0.1 + 0.05 is 0.15000000000000002. Unrounded, that reaches the axis as a label.
    for (const tick of niceTicks(0.1155)) {
      expect(String(tick).length).toBeLessThan(8);
    }
  });
});
