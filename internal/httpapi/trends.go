package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// Trends is the historical source the trend and report endpoints need.
//
// Separate from Reports and Stats for the same reason those are separate from each other: a trend
// reads the daily rollup, a summary reads the fact table, and a recommendation reads percentiles.
// One interface covering all three would make every handler depend on queries it never calls.
type Trends interface {
	Trend(ctx context.Context, p postgres.TrendParams) ([]postgres.TrendSeries, postgres.TrendSource, error)
	MonthlyReports(ctx context.Context, p postgres.MonthlyReportParams) ([]postgres.MonthlyReport, error)
}

// trendResponse wraps a time series.
type trendResponse struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Interval string    `json:"interval"`
	GroupBy  string    `json:"group_by"`

	// Source names which table answered, and it is deliberately part of the CONTRACT rather than a
	// debug field.
	//
	// The two sources do not answer identically: the rollup has no pod grain, no percentiles, and only
	// covers days the rollup job has processed -- so a series from it can legitimately stop short of
	// today. A client comparing this chart against /costs/summary needs to know whether they came from
	// the same place, and "the trend disagrees with the summary" should be answerable from the response
	// rather than by reading our source.
	Source string `json:"source"`

	Series []postgres.TrendSeries `json:"series"`
	Count  int                    `json:"count"`

	// Comparison is present only when compare=previous_period was requested.
	Comparison *trendComparison `json:"comparison,omitempty"`
}

// trendComparison is period-over-period change.
//
// WHY THIS IS COMPUTED SERVER-SIDE
// A client could subtract two totals itself, and would then have to decide what a change from zero
// means, how to render an infinite ratio, and whether a shorter previous period is comparable. Those
// are the same decisions for every client, so making them once here means every consumer answers the
// question the same way -- and the awkward cases below are answered explicitly rather than by whatever
// JavaScript does with a division by zero.
type trendComparison struct {
	PreviousFrom time.Time `json:"previous_from"`
	PreviousTo   time.Time `json:"previous_to"`

	Current  string `json:"current_total_cost"`
	Previous string `json:"previous_total_cost"`
	// Change is current - previous. Negative means it got cheaper.
	Change string `json:"change"`
	// ChangeRatio is change / previous, as a decimal fraction. NULL when the previous period cost
	// nothing, because the ratio is then undefined -- and a client rendering "+Inf%" or "+100%" for
	// "this is new" would be stating something the data does not support.
	ChangeRatio *string `json:"change_ratio"`

	// Comparable is false when the previous period has materially less data than the current one, so
	// the comparison is between a full period and a partial one.
	//
	// Surfaced rather than silently returned, because this is the single most misleading thing a trend
	// endpoint can do: a period during which collection started shows a huge apparent increase that is
	// entirely an artefact of when the collector was deployed.
	Comparable bool   `json:"comparable"`
	Note       string `json:"note,omitempty"`
}

// handleTrend serves GET /api/v1/costs/trend.
func handleTrend(trends Trends) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, compare, verr := trendParams(r)
		if verr != nil {
			writeValidationError(w, r, verr)
			return
		}

		series, source, err := trends.Trend(r.Context(), params)
		if err != nil {
			logError(r, "querying trend", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error", "could not compute the trend")
			return
		}

		// NIL NORMALISED TO EMPTY, here rather than trusting the repository.
		//
		// RollupRepository does return a non-nil slice and has a test for it, so this looks redundant. It
		// is not: the JSON contract "items is always an array" belongs to the HANDLER, and the Trends
		// interface does not -- and cannot usefully -- document a non-nil guarantee for every future
		// implementation. A stub in this package's own tests returned nil and produced `"series": null`,
		// which is exactly the client-side nil-check bug the contract exists to prevent.
		//
		// Guarantee what you serialise; do not inherit it.
		if series == nil {
			series = []postgres.TrendSeries{}
		}

		resp := trendResponse{
			From: params.From, To: params.To,
			Interval: string(params.Interval),
			GroupBy:  string(params.GroupBy),
			Source:   string(source),
			Series:   series,
			Count:    len(series),
		}

		if compare {
			cmp, err := comparePreviousPeriod(r.Context(), trends, params, series)
			if err != nil {
				logError(r, "querying comparison period", err)
				// A DEGRADED RESPONSE, not a 500. The trend itself succeeded, and the comparison is an
				// enrichment -- failing the whole request because an optional extra failed would deny
				// the caller data we already have in hand. The field is simply absent, which the
				// omitempty on Comparison makes indistinguishable from "not requested"... so it is
				// logged, because a silently missing enrichment is exactly the kind of thing that
				// degrades permanently without anyone noticing.
				resp.Comparison = nil
			} else {
				resp.Comparison = cmp
			}
		}

		setCostCacheHeaders(w)
		writeJSON(w, r, http.StatusOK, resp)
	}
}

// comparePreviousPeriod totals the immediately preceding window of equal length.
//
// EQUAL LENGTH, IMMEDIATELY PRECEDING -- not "last month" or "last week".
//
// Calendar-aligned comparison sounds friendlier and is ambiguous: a 10-day range has no obvious
// previous month, and comparing 31 days of August against 30 of September makes September look 3%
// cheaper for no reason. Shifting back by exactly the requested span means the two periods are always
// the same length, so the difference is about cost rather than about the calendar.
func comparePreviousPeriod(
	ctx context.Context, trends Trends, params postgres.TrendParams, current []postgres.TrendSeries,
) (*trendComparison, error) {
	span := params.To.Sub(params.From)

	prev := params
	prev.To = params.From
	prev.From = params.From.Add(-span)

	prevSeries, _, err := trends.Trend(ctx, prev)
	if err != nil {
		return nil, err
	}

	total := func(s []postgres.TrendSeries) (decimal.Decimal, int64) {
		sum, windows := decimal.Zero, int64(0)
		for _, series := range s {
			sum = sum.Add(series.TotalCost)
			for _, p := range series.Points {
				windows += p.Windows
			}
		}
		return sum, windows
	}

	curTotal, curWindows := total(current)
	prevTotal, prevWindows := total(prevSeries)

	cmp := &trendComparison{
		PreviousFrom: prev.From, PreviousTo: prev.To,
		Current:    curTotal.StringFixed(10),
		Previous:   prevTotal.StringFixed(10),
		Change:     curTotal.Sub(prevTotal).StringFixed(10),
		Comparable: true,
	}

	// The ratio is omitted rather than invented when there is nothing to divide by. "Cost went from 0
	// to 5" is a real and common situation -- a new workload -- and it has no percentage increase.
	if prevTotal.IsPositive() {
		ratio := curTotal.Sub(prevTotal).Div(prevTotal).StringFixed(6)
		cmp.ChangeRatio = &ratio
	} else {
		cmp.Note = "the previous period has no cost, so a percentage change is undefined"
		cmp.Comparable = false
	}

	// The coverage check. Compared on WINDOW COUNTS rather than on cost, because cost legitimately
	// varies between periods and window count should not -- so a large gap in windows means missing
	// data rather than a real change.
	//
	// The 20% threshold is a judgement rather than a derived value: below that, normal churn as pods
	// start and stop explains it. Above it, something was not collected.
	if prevWindows > 0 && curWindows > 0 {
		ratio := float64(prevWindows) / float64(curWindows)
		if ratio < 0.8 || ratio > 1.25 {
			cmp.Comparable = false
			cmp.Note = "the two periods have materially different window counts, so part of the " +
				"difference is missing data rather than changed cost"
		}
	} else if prevWindows == 0 {
		cmp.Comparable = false
		cmp.Note = "the previous period has no data at all -- collection may not have been running"
	}

	return cmp, nil
}

// monthlyReportsResponse wraps the statements.
type monthlyReportsResponse struct {
	Items []postgres.MonthlyReport `json:"items"`
	Count int                      `json:"count"`

	// Provisional counts how many of these are not yet frozen, which is what tells a reader whether
	// the page can still change. A statement page mixing frozen and provisional rows without saying so
	// invites someone to quote a number that is still moving.
	Provisional int `json:"provisional_count"`

	// LowestCoverage is the worst coverage among the returned statements.
	//
	// The single most important number on this response and the easiest to overlook, so it is hoisted
	// to the top level. A month with 40% coverage produces a total that is confidently too low, and
	// nothing about the total itself reveals that.
	LowestCoverage string `json:"lowest_coverage"`
}

// handleMonthlyReports serves GET /api/v1/reports/monthly.
func handleMonthlyReports(trends Trends) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, verr := monthlyReportParams(r)
		if verr != nil {
			writeValidationError(w, r, verr)
			return
		}

		items, err := trends.MonthlyReports(r.Context(), params)
		if err != nil {
			logError(r, "querying monthly reports", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not read the monthly reports")
			return
		}

		if items == nil {
			// See the note in handleTrend: the handler owns the JSON contract.
			items = []postgres.MonthlyReport{}
		}

		resp := monthlyReportsResponse{Items: items, Count: len(items)}
		lowest := decimal.NewFromInt(1)
		for _, m := range items {
			if !m.Finalised() {
				resp.Provisional++
			}
			if m.Coverage.LessThan(lowest) {
				lowest = m.Coverage
			}
		}
		if len(items) == 0 {
			// Not 1.0000 for an empty page: claiming perfect coverage of nothing is the kind of
			// default that reads as a real measurement.
			lowest = decimal.Zero
		}
		resp.LowestCoverage = lowest.StringFixed(4)

		// A FINALISED statement is immutable, so it could be cached for a year. A provisional one
		// changes on every rollup. Rather than vary the header per row -- which a single response
		// cannot do -- the short cost cache applies to both. Correct for the provisional case and
		// merely unambitious for the frozen one, which is the right way round to be wrong.
		setCostCacheHeaders(w)
		writeJSON(w, r, http.StatusOK, resp)
	}
}
