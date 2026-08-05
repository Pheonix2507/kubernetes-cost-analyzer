package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// zeroDecimal is the additive identity for the running totals below. A named helper rather than
// decimal.Zero inline, so the intent reads as "start an accumulation" rather than "a zero".
func zeroDecimal() decimal.Decimal { return decimal.Zero }

func itoa(n int) string { return strconv.Itoa(n) }

// Reports is the read side of the cost data this API serves.
//
// Declared in the consuming package and satisfied by *postgres.ReportRepository, so handler tests
// need no database. Same pattern as Inventory.
type Reports interface {
	CostSummary(ctx context.Context, p postgres.CostSummaryParams) ([]postgres.CostSummaryRow, error)
	Allocations(ctx context.Context, p postgres.AllocationsParams) (postgres.AllocationsPage, error)
}

// summaryResponse wraps a cost summary.
//
// The requested range and grouping are ECHOED BACK, which is not redundant. A client that omitted
// `from` received a default it never specified, and a chart labelled with a range the server chose
// is the only way that range is knowable. It also makes a stored response self-describing --
// pasting one into a ticket carries its own context.
type summaryResponse struct {
	From    time.Time                 `json:"from"`
	To      time.Time                 `json:"to"`
	GroupBy string                    `json:"group_by"`
	Items   []postgres.CostSummaryRow `json:"items"`
	Count   int                       `json:"count"`
	// Totals lets a client render a headline figure without summing the rows itself -- and
	// without being wrong when the row set was truncated by `limit`.
	Totals summaryTotals `json:"totals"`
}

// summaryTotals is the aggregate across every returned row.
type summaryTotals struct {
	TotalCost string `json:"total_cost"`
	CPUCost   string `json:"cpu_cost"`
	MemCost   string `json:"memory_cost"`
	// EstimatedRates is true if ANY returned group was priced from a fallback guess, so a
	// dashboard can badge the whole figure as approximate rather than making the user inspect
	// each row.
	EstimatedRates bool `json:"estimated_rates"`
	// Truncated is true when `limit` cut the result short, which means Totals covers only what
	// was returned. Without this flag a client cannot tell a complete total from a partial one,
	// and a partial total presented as complete is a wrong number.
	Truncated bool `json:"truncated"`
}

// handleCostSummary serves GET /api/v1/costs/summary.
//
// The primary reporting endpoint: aggregated cost over a time range, grouped by one dimension.
func handleCostSummary(reports Reports) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, verr := summaryParams(r)
		if verr != nil {
			writeValidationError(w, r, verr)
			return
		}

		rows, err := reports.CostSummary(r.Context(), params)
		if err != nil {
			logError(r, "querying cost summary", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error", "could not compute cost summary")
			return
		}

		resp := summaryResponse{
			From:    params.From,
			To:      params.To,
			GroupBy: string(params.GroupBy),
			Items:   rows,
			Count:   len(rows),
			Totals:  totals(rows, params.Limit),
		}

		// Cost data changes only when the collector runs, so a short cache is free correctness.
		// See setCostCacheHeaders.
		setCostCacheHeaders(w)
		writeJSON(w, r, http.StatusOK, resp)
	}
}

// totals sums the returned rows.
//
// Summed in Go rather than by a second SQL aggregate, deliberately: a separate query would need
// the same predicates and could see different data, so the headline figure could disagree with the
// rows beneath it. Summing what was actually returned makes that impossible by construction.
func totals(rows []postgres.CostSummaryRow, limit int) summaryTotals {
	t := summaryTotals{Truncated: limit > 0 && len(rows) >= limit}
	total, cpu, mem := zeroDecimal(), zeroDecimal(), zeroDecimal()
	for _, row := range rows {
		total = total.Add(row.TotalCost)
		cpu = cpu.Add(row.CPUCost)
		mem = mem.Add(row.MemoryCost)
		if row.EstimatedRates {
			t.EstimatedRates = true
		}
	}
	// Rendered as strings for the same reason every other monetary field is: a JSON number is
	// parsed as a double by every browser.
	t.TotalCost = total.String()
	t.CPUCost = cpu.String()
	t.MemCost = mem.String()
	return t
}

// handleAllocations serves GET /api/v1/allocations: raw fact rows, cursor-paginated.
//
// The escape hatch beneath the summary. When a figure looks wrong, this is where you go to see the
// rows it was computed from -- which is why it returns requested, used AND billable separately
// rather than only the billable amount the cost was derived from.
func handleAllocations(reports Reports) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, verr := allocationsParams(r)
		if verr != nil {
			writeValidationError(w, r, verr)
			return
		}

		page, err := reports.Allocations(r.Context(), params)
		if err != nil {
			logError(r, "querying allocations", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error", "could not list allocations")
			return
		}

		type response struct {
			From  time.Time                `json:"from"`
			To    time.Time                `json:"to"`
			Items []postgres.AllocationRow `json:"items"`
			Count int                      `json:"count"`
			// NextCursor is omitted entirely on the last page, so "is there more?" is answered by
			// the field's PRESENCE. An empty string would force every client to check for both
			// absence and emptiness.
			NextCursor string `json:"next_cursor,omitempty"`
		}

		setCostCacheHeaders(w)
		writeJSON(w, r, http.StatusOK, response{
			From:       params.From,
			To:         params.To,
			Items:      page.Rows,
			Count:      len(page.Rows),
			NextCursor: page.NextCursor,
		})
	}
}

// costCacheSeconds is how long a cost response may be cached.
//
// The collector writes every five minutes, so a response is at most that stale anyway. Sixty
// seconds is well inside that and turns a dashboard that polls -- or a user hammering refresh --
// into one query rather than dozens. Deliberately shorter than the collection interval so a new
// window becomes visible promptly rather than up to ten minutes late.
const costCacheSeconds = 60

// setCostCacheHeaders marks a cost response cacheable.
//
// PRIVATE, not public. These responses are scoped to whoever authenticated, and a shared proxy
// caching one team's cost report and serving it to another would be a data leak dressed as an
// optimisation. `private` tells intermediaries not to store it while still allowing the browser to.
func setCostCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, max-age="+itoa(costCacheSeconds))
	// Vary on the auth header: two callers with different keys may be entitled to different data,
	// and a cache that ignored this could serve one the other's response.
	w.Header().Set("Vary", "Authorization")
}
