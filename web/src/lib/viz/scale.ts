/**
 * Y-axis scale maths.
 *
 * WHY THIS IS NOT IN LineChart.tsx, WHERE IT STARTED
 * -------------------------------------------------
 * It is pure arithmetic with no JSX and no React, and it turned out to be the only genuinely
 * defect-prone part of the chart -- it silently clipped data for months. Living in a .tsx file it
 * could not be unit tested without configuring a JSX transform for the test runner, which is a lot
 * of machinery to reach one numeric function.
 *
 * The general rule it illustrates: the part of a component you most want to test is usually the part
 * that is not a component.
 */
/**
 * niceTicks picks round y-axis values.
 *
 * Round numbers, because the ticks carry every value that is not directly labelled -- 0 / 0.05 / 0.10
 * is readable and 0 / 0.0417 / 0.0834 is not, even though both are correct. The 1/2/5 progression is
 * the standard choice: those are the multipliers whose multiples humans read without arithmetic.
 */
export function niceTicks(max: number, count = 4): number[] {
  if (max <= 0) return [0];
  const rough = max / count;
  const mag = 10 ** Math.floor(Math.log10(rough));
  const step = [1, 2, 5, 10].map((m) => m * mag).find((s) => s >= rough) ?? 10 * mag;

  // THE TOP TICK IS ROUNDED UP TO COVER max, AND THAT IS A BUG FIX.
  //
  // This previously looped `for (v = 0; v <= max + step / 2; v += step)`, which stops BELOW max
  // whenever max sits more than half a step above the last round value. The consequence was not a
  // cosmetic axis problem: LineChartSVG scales by the last tick, so any point above it was plotted
  // outside the plot area and CLIPPED by the frame.
  //
  // Found in a screenshot. A namespace costing 0.1155 a day produced ticks of 0 / 0.05 / 0.10, and
  // the five days sitting at 0.1155 vanished -- the line left the top of the chart on one date and
  // reappeared four days later, which reads as missing data rather than as an axis that is too
  // short. A chart that silently drops the largest values is worse than one that fails to render,
  // because a gap invites you to go looking for a collection outage.
  const top = Math.ceil(max / step) * step;
  const out: number[] = [];
  for (let v = 0; v <= top + step / 2; v += step) out.push(Number(v.toFixed(10)));
  return out;
}
