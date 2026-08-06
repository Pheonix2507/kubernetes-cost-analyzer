/**
 * Money and quantity handling.
 *
 * WHY THIS FILE EXISTS
 * ====================
 * The API returns every monetary and quantity value as a JSON **string**, not a number. That was a
 * Phase 5 decision on the Go side and it is the frontend's job not to undo it.
 *
 * A real response from this API:
 *
 *     "cpu_requested_core_hours": "3.41666666666666666666666686"
 *
 * That is 26 significant digits, because it comes from a Postgres `numeric` via Go's
 * shopspring/decimal. `JSON.parse` would turn it into an IEEE 754 double, which holds about 15-17
 * significant digits:
 *
 *     parseFloat("3.41666666666666666666666686")  ->  3.4166666666666665
 *
 * The loss is small per row and it COMPOUNDS: sum a month of five-minute windows and the drift is a
 * real number of currency units, in a tool whose entire purpose is telling people what they spent.
 * And it is the worst kind of wrong, because it is plausible -- nobody looks at an invoice and thinks
 * "that is out by 2e-16".
 *
 * HOW COMPANIES USUALLY GET THIS WRONG
 * Three ways, in order of frequency. Storing money as a float in the database. Serialising a correct
 * decimal as a JSON number, which is precise on the wire and destroyed by the parser. And doing the
 * first two correctly, then writing `items.reduce((a, b) => a + parseFloat(b.total_cost), 0)` in a
 * chart component, which is this file's whole reason for existing.
 *
 * WHY A BRANDED TYPE RATHER THAN A CONVENTION
 * A comment saying "do not parseFloat money" is advice. A type that makes `parseFloat(m)` a compile
 * error is a guarantee. The brand costs one cast at the boundary and buys the rest of the codebase.
 */

/**
 * Money is an exact decimal as it arrived from the API.
 *
 * The intersection with `{ readonly __money: unique symbol }` is a **brand**: structurally it is
 * still a string, so `.length` and template literals work, but a plain `string` is not assignable to
 * `Money` and `Money` is not assignable to a `number` parameter. That asymmetry is the point --
 * `Number(m)` and `parseFloat(m)` stop being reachable by accident.
 *
 * `unique symbol` never exists at runtime. This is entirely a compile-time construct and the emitted
 * JavaScript is unchanged.
 */
export type Money = string & { readonly __money: unique symbol };

/**
 * asMoney marks a string from the API as exact.
 *
 * The ONE place a cast is allowed, so the unsafety is countable rather than scattered. Every value
 * passing through here came from a `numeric` column and is already a valid decimal string, so this
 * asserts rather than validates -- and it exists so that assertion happens at the boundary instead
 * of implicitly at every use.
 */
export function asMoney(raw: string): Money {
  return raw as Money;
}

/**
 * ZERO is the additive identity, for empty states.
 *
 * A constant rather than `asMoney("0")` inline, because "0" and "0.0000" and "" all render
 * differently and a component reaching for a fallback should not have to pick one.
 */
export const ZERO = asMoney("0");

/**
 * formatMoney renders an amount for a human.
 *
 * TRUNCATES FOR DISPLAY, WHICH IS SAFE AND DIFFERENT FROM TRUNCATING FOR ARITHMETIC. The exact value
 * is never mutated -- this produces a new string to put on a screen, and the caller still holds the
 * full-precision original. That distinction is the whole reason this is a formatter and not a parser.
 *
 * Intl.NumberFormat needs a number, so this is the one place a conversion happens, and it is
 * deliberately AFTER all arithmetic rather than before any. At display magnitudes a double is
 * exact enough for the digits actually shown.
 *
 * @param amount exact value from the API
 * @param opts.currency an ISO 4217 code. Omitted by default: the pricing catalogue's units are the
 *   operator's choice (see deploy/pricing/catalogue.yaml), so the API does not know the currency and
 *   the UI must not invent one. Rendering "$" beside a figure priced in rupees is worse than
 *   rendering no symbol.
 */
export function formatMoney(
  amount: Money | undefined,
  opts: { currency?: string; maximumFractionDigits?: number } = {},
): string {
  if (amount === undefined || amount === "") return "—";

  const n = Number(amount);
  if (!Number.isFinite(n)) return "—";

  const { currency, maximumFractionDigits } = opts;

  // Small figures need more places than large ones. A cluster costing 0.0218 for a day and one
  // costing 4,182.19 for a month are both real, and a fixed two-decimal format renders the first as
  // "0.02" -- which reads as "nothing" when it is the entire point of the row.
  const digits =
    maximumFractionDigits ??
    (Math.abs(n) === 0 ? 2 : Math.abs(n) < 0.01 ? 6 : Math.abs(n) < 1 ? 4 : 2);

  return new Intl.NumberFormat(undefined, {
    style: currency ? "currency" : "decimal",
    currency,
    minimumFractionDigits: Math.min(2, digits),
    maximumFractionDigits: digits,
  }).format(n);
}

/**
 * toPlotValue converts an exact value into a number for a CHART ONLY.
 *
 * Named for its single legitimate use rather than something general like `toNumber`, because the name
 * is the warning. A chart is pixels: a series point can only ever be a float, and the difference
 * between 3.41666666666666666666666686 and 3.4166666666666665 is far below one pixel at any sane
 * scale.
 *
 * NEVER USE THIS FOR ARITHMETIC THAT IS SHOWN AS A FIGURE. Sum plot values and you have recreated
 * exactly the bug the string encoding prevents. Totals come from the API, which computed them in
 * Postgres `numeric` -- see the note on `totals` in internal/httpapi/costs.go, where the same
 * argument is made for not recomputing them in a second SQL query.
 */
export function toPlotValue(amount: Money | undefined): number {
  if (amount === undefined || amount === "") return 0;
  const n = Number(amount);
  return Number.isFinite(n) ? n : 0;
}

/**
 * formatQuantity renders core-hours or GiB-hours.
 *
 * Separate from formatMoney because the two answer different questions and a reader confuses them
 * instantly if they look alike. Money is a bill; a quantity is an amount of resource, and 3.42
 * core-hours wants a unit suffix rather than a currency style.
 */
export function formatQuantity(amount: Money | undefined, unit: string): string {
  if (amount === undefined || amount === "") return "—";
  const n = Number(amount);
  if (!Number.isFinite(n)) return "—";
  const digits = Math.abs(n) < 1 ? 3 : Math.abs(n) < 100 ? 2 : 0;
  return `${new Intl.NumberFormat(undefined, { maximumFractionDigits: digits }).format(n)} ${unit}`;
}

/**
 * formatRatio renders a decimal fraction as a percentage.
 *
 * Takes a string for the same reason everything here does: `coverage` and `change_ratio` arrive as
 * exact decimals. A ratio is the one case where the double conversion is genuinely harmless -- the
 * output has one decimal place -- but it goes through the same door so there is no second convention
 * to remember.
 */
export function formatRatio(ratio: string | null | undefined): string {
  if (ratio === null || ratio === undefined || ratio === "") return "—";
  const n = Number(ratio);
  if (!Number.isFinite(n)) return "—";
  return `${(n * 100).toFixed(1)}%`;
}

/**
 * isNegative reports whether an amount is below zero, without converting it.
 *
 * String inspection rather than `Number(m) < 0`, and the reason is not precision -- a sign check
 * survives a double fine. It is that a codebase where the ONLY comparison operator on Money is a
 * string test never grows an accidental `Number(a) < Number(b)` beside it.
 *
 * Recommendations use this: a negative saving is a reliability fix that COSTS money, and it must be
 * displayed as a cost rather than netted against savings. Same reasoning as the two separate totals
 * in the recommendations response.
 */
export function isNegative(amount: Money | undefined): boolean {
  return amount !== undefined && amount.trimStart().startsWith("-");
}

/** absMoney drops a leading minus, for displaying a required increase as a positive magnitude. */
export function absMoney(amount: Money): Money {
  return asMoney(amount.replace(/^\s*-/, ""));
}
