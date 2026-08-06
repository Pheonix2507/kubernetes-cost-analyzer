package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/recommend"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// Stats is the statistics source the recommendation endpoint needs.
//
// Separate from Reports rather than folded into it: a summary is a SUM and a recommendation needs
// percentiles, peaks and replica counts. Keeping them apart means a dashboard query never computes
// statistics it does not use.
type Stats interface {
	ContainerStats(ctx context.Context, p postgres.ContainerStatsParams) ([]postgres.ContainerStats, error)
}

// recommendationsResponse wraps the advice.
type recommendationsResponse struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	Items []recommend.Recommendation `json:"items"`
	Count int                        `json:"count"`

	// Totals separates savings from costs rather than netting them.
	//
	// A single net figure would let a large right-sizing saving cancel out a critical memory increase,
	// so a page reporting "net saving: $40" would hide the fact that $12 of it is a change someone
	// MUST make for reliability. The two are different kinds of number and are presented as such.
	Totals recommendationTotals `json:"totals"`

	// AnalysedContainers is how many containers were examined, which is the denominator for
	// "how much of my estate has findings". Without it, three recommendations could mean a healthy
	// cluster or a barely-collected one.
	AnalysedContainers int `json:"analysed_containers"`
}

type recommendationTotals struct {
	// PotentialMonthlySaving is the sum of the POSITIVE savings only.
	PotentialMonthlySaving string `json:"potential_monthly_saving"`
	// RequiredMonthlyIncrease is the sum of the NEGATIVE ones, expressed positive.
	//
	// Surfaced separately and deliberately: these are the reliability fixes, and a tool that buried
	// them inside a net saving would be optimising the metric rather than the system.
	RequiredMonthlyIncrease string `json:"required_monthly_increase"`

	Critical int `json:"critical"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`

	// EstimatedRates is true when any recommendation's saving rests on a fallback rate, so the figure
	// is an estimate built on an estimate.
	EstimatedRates bool `json:"estimated_rates"`
}

// handleRecommendations serves GET /api/v1/recommendations.
func handleRecommendations(stats Stats, engine *recommend.Engine, clusters Clusters) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, verr := statsParams(r)
		if verr != nil {
			writeValidationError(w, r, verr)
			return
		}

		// Refuse a response that would mix currencies. The rule the API keeps is simple and
		// explainable: no single response returns money in more than one currency, because no
		// response carries a per-row currency, so a client has no way to tell them apart. When
		// rows gain a currency of their own, this can be relaxed for the listing endpoints.
		if status, cerr := guardCurrency(r.Context(), clusters, params.Filters.Cluster); cerr != nil {
			if status == http.StatusConflict {
				writeError(w, r, status, "mixed_currencies", cerr.Error())
			} else {
				logError(r, "checking fleet currencies", cerr)
				writeError(w, r, status, "internal_error", "could not check fleet currencies")
			}
			return
		}

		containers, err := stats.ContainerStats(r.Context(), params)
		if err != nil {
			logError(r, "querying container stats", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not compute recommendations")
			return
		}

		recs := engine.Analyse(containers)

		resp := recommendationsResponse{
			From:               params.From,
			To:                 params.To,
			Items:              recs,
			Count:              len(recs),
			AnalysedContainers: len(containers),
			Totals:             recommendationTotals{},
		}

		saving, increase := decimal.Zero, decimal.Zero
		for _, rec := range recs {
			switch rec.Severity {
			case recommend.SeverityCritical:
				resp.Totals.Critical++
			case recommend.SeverityWarning:
				resp.Totals.Warning++
			case recommend.SeverityInfo:
				resp.Totals.Info++
			}
			if rec.EstimatedMonthlySaving.IsNegative() {
				increase = increase.Add(rec.EstimatedMonthlySaving.Neg())
			} else {
				saving = saving.Add(rec.EstimatedMonthlySaving)
			}
			if rec.EstimatedRates {
				resp.Totals.EstimatedRates = true
			}
		}
		resp.Totals.PotentialMonthlySaving = saving.StringFixed(4)
		resp.Totals.RequiredMonthlyIncrease = increase.StringFixed(4)

		// Recommendations change only when the collector adds windows, so the same short cache as the
		// cost endpoints applies.
		setCostCacheHeaders(w)
		writeJSON(w, r, http.StatusOK, resp)
	}
}
