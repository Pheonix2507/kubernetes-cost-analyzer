package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/recommend"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// stubClusters is a fleet with no database behind it, which is the whole reason Clusters is a port:
// constructing a two-currency fleet here is three lines, whereas inserting one would need two
// clusters, their nodes and their facts.
type stubClusters struct {
	rows []postgres.ClusterRow
	err  error

	calls int
}

func (s *stubClusters) ListClusters(context.Context) ([]postgres.ClusterRow, error) {
	s.calls++
	return s.rows, s.err
}

func cluster(name, currency string) postgres.ClusterRow {
	return postgres.ClusterRow{
		Name: name, Provider: "kind", Region: "ap-south-1", Currency: currency,
		FirstSeen: time.Now().Add(-24 * time.Hour), LastSeen: time.Now(),
	}
}

func TestListClusters(t *testing.T) {
	t.Parallel()

	clusters := &stubClusters{rows: []postgres.ClusterRow{
		cluster("prod-eu", "EUR"), cluster("prod-in", "INR"),
	}}
	rr := httptest.NewRecorder()
	NewRouter(RouterOptions{Clusters: clusters, Log: discardLogger()}).
		ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var got clusterListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.Count != 2 {
		t.Errorf("count = %d, want 2", got.Count)
	}
	// Sorted, and both present. The order matters because this list populates a selector, where a
	// stable order means the same cluster stays in the same place between renders.
	if len(got.Currencies) != 2 || got.Currencies[0] != "EUR" || got.Currencies[1] != "INR" {
		t.Errorf("currencies = %v, want [EUR INR]", got.Currencies)
	}
	// THE FIELD THAT CHANGES CLIENT BEHAVIOUR. A fleet with two currencies cannot be summed.
	if got.Aggregatable {
		t.Error("aggregatable = true for a two-currency fleet; a client would then sum EUR and INR")
	}
}

func TestListClusters_SingleCurrencyFleetIsAggregatable(t *testing.T) {
	t.Parallel()

	clusters := &stubClusters{rows: []postgres.ClusterRow{
		cluster("a", "USD"), cluster("b", "USD"), cluster("c", "USD"),
	}}
	rr := httptest.NewRecorder()
	NewRouter(RouterOptions{Clusters: clusters, Log: discardLogger()}).
		ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil))

	var got clusterListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(got.Currencies) != 1 {
		t.Errorf("currencies = %v, want exactly one entry for three USD clusters", got.Currencies)
	}
	if !got.Aggregatable {
		t.Error("aggregatable = false for a fleet that reports entirely in USD")
	}
}

// TestGuardCurrency_RefusesUnscopedAggregateAcrossCurrencies is the point of the whole guard.
//
// The failure it prevents is silent: summing 100 USD and 100 EUR yields 200, which is the right order
// of magnitude, carries no unit, and no reader questions it.
func TestGuardCurrency_RefusesUnscopedAggregateAcrossCurrencies(t *testing.T) {
	t.Parallel()

	// Every endpoint that returns money. All four are guarded because no response carries a
	// per-row currency, so a client cannot tell mixed figures apart even in a plain listing.
	endpoints := []string{
		"/api/v1/costs/summary",
		"/api/v1/allocations",
		"/api/v1/recommendations",
		"/api/v1/costs/trend",
	}

	for _, endpoint := range endpoints {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			clusters := &stubClusters{rows: []postgres.ClusterRow{
				cluster("prod-eu", "EUR"), cluster("prod-us", "USD"),
			}}
			rr := httptest.NewRecorder()
			NewRouter(RouterOptions{
				Reports: &stubReports{}, Stats: &stubReports{}, Trends: &stubTrends{},
				Recommender: recommend.NewEngine(recommend.DefaultThresholds()), Clusters: clusters, Log: discardLogger(),
			}).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, endpoint, nil))

			// 409 rather than 400: the request is not malformed, and the same URL is valid
			// against a single-currency fleet.
			if rr.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body: %s", rr.Code, rr.Body.String())
			}
			body := rr.Body.String()
			for _, want := range []string{"mixed_currencies", "EUR", "USD", "cluster="} {
				if !contains(body, want) {
					t.Errorf("body does not mention %q, so the client is not told how to fix it: %s", want, body)
				}
			}
		})
	}
}

// TestGuardCurrency_ScopedQueryIsAllowed asserts the escape hatch works, and that it costs NOTHING:
// a query already naming a cluster reads one currency by construction, so the guard must not spend a
// database round trip proving it.
func TestGuardCurrency_ScopedQueryIsAllowed(t *testing.T) {
	t.Parallel()

	clusters := &stubClusters{rows: []postgres.ClusterRow{
		cluster("prod-eu", "EUR"), cluster("prod-us", "USD"),
	}}
	rr := httptest.NewRecorder()
	NewRouter(RouterOptions{Reports: &stubReports{}, Clusters: clusters, Log: discardLogger()}).
		ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
			"/api/v1/costs/summary?cluster=prod-eu", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; a scoped query spans one currency: %s", rr.Code, rr.Body.String())
	}
	if clusters.calls != 0 {
		t.Errorf("ListClusters was called %d times for a scoped query; the guard should short-circuit "+
			"without a round trip", clusters.calls)
	}
}

// TestGuardCurrency_FleetLookupFailureIsNotA409 separates "cannot aggregate" from "cannot tell".
//
// Reporting 409 when the fleet query itself failed would tell the client to scope its request, which
// would not help: the next scoped request works, the operator concludes the fleet is mixed-currency,
// and the actual database fault goes uninvestigated.
func TestGuardCurrency_FleetLookupFailureIsNotA409(t *testing.T) {
	t.Parallel()

	clusters := &stubClusters{err: errors.New("connection refused")}
	rr := httptest.NewRecorder()
	NewRouter(RouterOptions{Reports: &stubReports{}, Clusters: clusters, Log: discardLogger()}).
		ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the fleet cannot be read", rr.Code)
	}
}

// TestClusterFilterReachesTheQuery closes the loop between the query string and the SQL builder. The
// filter being ACCEPTED is not the same as it being APPLIED, and a filter silently dropped returns a
// fleet-wide total under a single cluster's heading.
func TestClusterFilterReachesTheQuery(t *testing.T) {
	t.Parallel()

	reports := &stubReports{}
	rr := httptest.NewRecorder()
	NewRouter(RouterOptions{Reports: reports, Clusters: &stubClusters{}, Log: discardLogger()}).
		ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
			"/api/v1/costs/summary?cluster=prod-eu", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if got := reports.gotSummaryParams.Filters.Cluster; got != "prod-eu" {
		t.Errorf("Filters.Cluster reached the repository as %q, want %q", got, "prod-eu")
	}
}
