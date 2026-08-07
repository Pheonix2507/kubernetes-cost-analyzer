import { describe, expect, it } from "vitest";
import { absMoney, asMoney, formatMoney, formatQuantity, formatRatio, isNegative, toPlotValue } from "./money";

/**
 * Money is a branded string, and these tests are about the two things that go wrong with money in a
 * browser: losing precision, and rendering a value that is technically correct and reads as wrong.
 */

describe("formatMoney", () => {
  it("renders a dash rather than 0 for absent values", () => {
    // THE DISTINCTION THAT MATTERS. A missing figure and a zero figure mean completely different
    // things: "we have no data for this namespace" versus "this namespace costs nothing". Rendering
    // both as 0.00 tells an operator a workload is free when the truth is that nobody measured it.
    expect(formatMoney(undefined)).toBe("—");
    expect(formatMoney("" as ReturnType<typeof asMoney>)).toBe("—");
    expect(formatMoney(asMoney("not-a-number"))).toBe("—");
  });

  it("scales fraction digits to the magnitude", () => {
    // A cost engine emitting five-minute windows produces genuinely tiny numbers. Formatting
    // 0.000123 with two decimals gives "0.00", so a real cost renders as free -- and every
    // per-window figure on the dashboard is in that range.
    expect(formatMoney(asMoney("0.000123"))).toContain("0.000123");
    // Four significant decimals are KEPT for a sub-1 value, where two would round it to 0.54.
    // Note that maximumFractionDigits permits digits, it does not pad them: 0.5 stays "0.50"
    // rather than becoming "0.5000", because minimumFractionDigits is 2.
    expect(formatMoney(asMoney("0.5432"))).toBe("0.5432");
    expect(formatMoney(asMoney("0.5"))).toBe("0.50");
    expect(formatMoney(asMoney("1234.5678"))).toBe("1,234.57");
  });

  it("keeps a realistic sub-cent figure from rounding away to zero", () => {
    // The regression this guards. One small container over a five-minute window costs a fraction of
    // a cent -- an m5.large is about $0.096/hour, so the whole node is $0.008 for the window and a
    // container using a few percent of it lands near 1e-4. A fixed two-decimal format renders every
    // one of those as free, i.e. the entire dashboard reads as costing nothing.
    for (const tiny of ["0.0001", "0.00001", "0.000001"]) {
      expect(formatMoney(asMoney(tiny))).not.toBe("0.00");
    }
  });

  it("floors at six decimal places, which is documented rather than fixed", () => {
    // Below 5e-7 a figure does collapse to 0.00. That is a deliberate limit, not an oversight: six
    // decimals is a millionth of a currency unit, three orders of magnitude below any realistic
    // per-container window cost. Asserting it here means the boundary is a decision on record, so
    // that if per-second billing ever pushes real values under it, this test fails and says why.
    expect(formatMoney(asMoney("0.0000004"))).toBe("0.00");
  });

  it("gives exact zero two digits rather than six", () => {
    // Zero is not "very small". Rendering it as 0.000000 implies a precision that is not being
    // claimed and makes a column of real figures harder to scan.
    expect(formatMoney(asMoney("0"))).toBe("0.00");
  });

  it("formats as currency when a code is supplied", () => {
    const out = formatMoney(asMoney("12.34"), { currency: "USD" });
    expect(out).toMatch(/12\.34/);
    expect(out).not.toBe("12.34");
  });
});

describe("toPlotValue", () => {
  it("is the ONLY place a Money becomes a number", () => {
    // Charts need numbers; money is carried as a string precisely because parsing it loses
    // precision beyond 2^53. Funnelling that conversion through one named function means the lossy
    // step is greppable, rather than a Number() sprinkled through twenty components.
    expect(toPlotValue(asMoney("1.5"))).toBe(1.5);
  });

  it("treats absent and unparseable values as 0 so a chart still renders", () => {
    // A chart is a different context from a table. NaN in a plot breaks the whole series -- and
    // therefore every other series sharing the axis -- whereas a table can honestly print a dash.
    expect(toPlotValue(undefined)).toBe(0);
    expect(toPlotValue(asMoney(""))).toBe(0);
    expect(toPlotValue(asMoney("nonsense"))).toBe(0);
  });
});

describe("isNegative", () => {
  it("reads the SIGN CHARACTER, not the parsed number", () => {
    expect(isNegative(asMoney("-0.5"))).toBe(true);
    expect(isNegative(asMoney("0.5"))).toBe(false);
    expect(isNegative(undefined)).toBe(false);
  });

  it("treats a negative zero as negative, which is the whole reason it inspects the string", () => {
    // THE BUG THIS ENCODES. `Number("-0") < 0` is FALSE, so a numeric sign test classifies "-0.00"
    // as positive. The recommendation engine legitimately emits negative savings -- correct advice
    // sometimes costs money -- and a saving of exactly -0.00 was rendering with a minus sign in one
    // place and without in another. Reading the character makes the three states (positive,
    // negative, negative-zero) distinguishable.
    expect(isNegative(asMoney("-0"))).toBe(true);
    expect(isNegative(asMoney("-0.00"))).toBe(true);
    expect(Number("-0") < 0).toBe(false); // the trap, asserted so nobody "simplifies" the above
  });

  it("tolerates leading whitespace, since these strings come off the wire", () => {
    expect(isNegative(asMoney(" -1"))).toBe(true);
  });
});

describe("absMoney", () => {
  it("strips the sign WITHOUT going through a number", () => {
    // 12345678901234567890.12 exceeds 2^53. Math.abs(Number(x)) would silently round it; removing
    // the character cannot.
    expect(absMoney(asMoney("-12345678901234567890.12"))).toBe("12345678901234567890.12");
    expect(absMoney(asMoney("-0.5"))).toBe("0.5");
    expect(absMoney(asMoney("0.5"))).toBe("0.5");
  });
});

describe("formatQuantity", () => {
  it("appends the unit and scales digits to magnitude", () => {
    expect(formatQuantity(asMoney("0.5"), "core-h")).toBe("0.5 core-h");
    expect(formatQuantity(asMoney("12.345"), "GiB-h")).toBe("12.35 GiB-h");
    expect(formatQuantity(asMoney("1234.5"), "core-h")).toBe("1,235 core-h");
  });

  it("dashes an absent quantity rather than printing a bare unit", () => {
    // "— " with a stray unit reads as a value of zero somethings. The dash has to stand alone.
    expect(formatQuantity(undefined, "core-h")).toBe("—");
  });
});

describe("formatRatio", () => {
  it("renders a fraction as a percentage to one decimal", () => {
    expect(formatRatio("0.8523")).toBe("85.2%");
    expect(formatRatio("1")).toBe("100.0%");
  });

  it("distinguishes an absent ratio from zero", () => {
    // Coverage is the case: a month with no data has no coverage ratio, which is not the same as
    // 0% coverage, and a monthly statement that reports 0.0% where it means "unknown" is a
    // statement nobody can act on.
    expect(formatRatio(null)).toBe("—");
    expect(formatRatio(undefined)).toBe("—");
    expect(formatRatio("")).toBe("—");
    expect(formatRatio("0")).toBe("0.0%");
  });
});
