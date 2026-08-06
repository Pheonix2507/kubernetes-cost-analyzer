package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// Clusters lists the clusters reporting into this database.
//
// A port rather than the concrete repository, for the reason every other port here exists: the
// handler tests supply a fake and never touch Postgres, so they run in microseconds and can
// construct fleets that would be tedious to insert.
type Clusters interface {
	ListClusters(ctx context.Context) ([]postgres.ClusterRow, error)
}

// clusterResponse is one cluster as the API presents it.
type clusterResponse struct {
	Name string `json:"name"`
	// Provider and Region are DERIVED from the cluster's nodes, so they can be empty: a cluster
	// whose nodes span two regions has no single region, and reporting the commonest one would
	// attribute the whole cluster to a region hosting only part of it. Empty means "the nodes do
	// not agree", which is the truth.
	Provider string `json:"provider"`
	Region   string `json:"region"`
	// Account is configured, since nothing in the Kubernetes API names the billing account.
	Account string `json:"account"`
	// Currency is the ISO 4217 code every cost figure for this cluster is denominated in.
	Currency  string    `json:"currency"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// clusterListResponse wraps the list.
//
// `currencies` is included rather than left for the client to derive, because it answers the one
// question that changes what a client may safely do: whether costs can be aggregated across the
// fleet at all. A dashboard can read one field instead of reducing over the list and reimplementing
// the server's rule.
type clusterListResponse struct {
	Items []clusterResponse `json:"items"`
	Count int               `json:"count"`
	// Currencies is every distinct currency in the fleet, sorted. More than one means fleet-wide
	// aggregation is refused, and a client should scope its queries to a single cluster.
	Currencies []string `json:"currencies"`
	// Aggregatable is the same fact stated as the decision it drives. Two fields for one truth is
	// usually a smell; here it is deliberate, because `len(currencies) <= 1` is a rule the server
	// owns and every client would otherwise re-derive, differently.
	Aggregatable bool `json:"aggregatable"`
}

func handleListClusters(clusters Clusters) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := clusters.ListClusters(r.Context())
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "could not list clusters")
			return
		}

		items := make([]clusterResponse, 0, len(rows))
		for _, c := range rows {
			items = append(items, clusterResponse{
				Name:      c.Name,
				Provider:  c.Provider,
				Region:    c.Region,
				Account:   c.Account,
				Currency:  c.Currency,
				FirstSeen: c.FirstSeen,
				LastSeen:  c.LastSeen,
			})
		}

		currencies := distinctCurrencies(rows)
		writeJSON(w, r, http.StatusOK, clusterListResponse{
			Items:        items,
			Count:        len(items),
			Currencies:   currencies,
			Aggregatable: len(currencies) <= 1,
		})
	}
}

// distinctCurrencies returns the sorted set of currencies in the fleet.
func distinctCurrencies(rows []postgres.ClusterRow) []string {
	seen := make(map[string]struct{}, 4)
	for _, c := range rows {
		if c.Currency != "" {
			seen[c.Currency] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// guardCurrency refuses a query that would sum across currencies.
//
// WHY THIS EXISTS AT ALL
// ---------------------
// 100 USD + 100 EUR is not 200 of anything. It is the kind of wrong answer that no test catches and
// no reader questions, because the number is the right ORDER OF MAGNITUDE and carries no unit. Once
// a fleet spans providers or regions whose catalogues are denominated differently, every unscoped
// aggregate is quietly meaningless.
//
// WHY REFUSE RATHER THAN CONVERT
// A tempting alternative is to normalise to a reporting currency using an exchange rate. That is
// worse, and not by a little: an exchange rate is a second source of truth that moves daily, so the
// same historical month would report different totals depending on when you asked. Refusing gives an
// answer that is either correct or absent, which is the only honest pair of options for money.
//
// WHY 409 AND NOT 400
// The request is not malformed -- the same URL is perfectly valid against a single-currency fleet,
// and will be valid again if the fleet becomes one. 409 Conflict says the request conflicts with the
// current state of the resource, which is exactly the situation. A 400 would tell the client to fix
// its request when there may be nothing wrong with it.
func guardCurrency(ctx context.Context, clusters Clusters, cluster string) (int, error) {
	// A query already scoped to one cluster reads one currency by construction, so there is
	// nothing to guard and no reason to spend a round trip on it.
	if cluster != "" {
		return 0, nil
	}

	rows, err := clusters.ListClusters(ctx)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("checking fleet currencies: %w", err)
	}

	currencies := distinctCurrencies(rows)
	if len(currencies) <= 1 {
		return 0, nil
	}
	return http.StatusConflict, fmt.Errorf(
		"this fleet reports in %v, so a total across all clusters would sum different currencies; "+
			"pass ?cluster=<name> to scope the query (see GET /api/v1/clusters)", currencies)
}

// unknownFleet stands in when no cluster repository is wired.
//
// WHY A STUB RATHER THAN A NIL CHECK IN EVERY HANDLER
// -------------------------------------------------
// Four handlers consult the guard. Four nil checks is four places to forget one, and the one
// forgotten is a nil-interface call that the panic middleware turns into a 500 -- which is exactly
// how this was discovered. Substituting once, in the router, means the handlers can assume a
// non-nil port.
//
// WHY IT IS SAFE TO REPORT AN EMPTY FLEET
// An empty list yields zero distinct currencies, so the guard passes and every endpoint behaves as
// it did before Phase 11 -- which was correct behaviour for a single cluster. The guard only ever
// ADDS a refusal, so losing it cannot produce a wrong answer that was previously right.
//
// It is still WRONG to run a real API this way, because a genuinely mixed-currency fleet would
// then be summed silently. That is why the router logs a warning rather than substituting quietly:
// a safety net that disappears without saying so is worse than one that was never there, since
// everybody assumes it is still catching things.
type unknownFleet struct{}

// ListClusters reports an empty fleet, which the guard reads as "at most one currency".
func (unknownFleet) ListClusters(context.Context) ([]postgres.ClusterRow, error) { return nil, nil }
