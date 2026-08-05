package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/pricing"
)

// stubInventory is the payoff for defining Inventory as an interface in this package:
// these tests exercise the real routing, middleware, validation and JSON encoding with
// no informer, no API server and no Docker.
type stubInventory struct {
	nodes      []domain.Node
	namespaces []domain.Namespace
	pods       []domain.Pod
	err        error

	// gotNamespace records what the handler passed down, so we can assert the query
	// parameter is actually plumbed through rather than silently dropped.
	gotNamespace string
}

func (s *stubInventory) Nodes() ([]domain.Node, error)           { return s.nodes, s.err }
func (s *stubInventory) Namespaces() ([]domain.Namespace, error) { return s.namespaces, s.err }
func (s *stubInventory) Pods(namespace string) ([]domain.Pod, error) {
	s.gotNamespace = namespace
	return s.pods, s.err
}

// stubPricer returns fixed rates, or an error when errFor matches the node name.
//
// A struct literal rather than a real catalogue: the handler's job is attaching rates and
// handling a pricing failure, not pricing. Testing it against a real provider would couple
// these tests to catalogue contents and make them fail for the wrong reason.
type stubPricer struct {
	rates  pricing.Rates
	errFor string
}

func (s stubPricer) RatesFor(_ context.Context, n domain.Node) (pricing.Rates, error) {
	if s.errFor != "" && n.Name == s.errFor {
		return pricing.Rates{}, errors.New("no price for " + n.Name)
	}
	return s.rates, nil
}

func defaultStubPricer() stubPricer {
	return stubPricer{rates: pricing.Rates{
		Currency:         "USD",
		NodeHourly:       decimal.RequireFromString("0.1060"),
		CPUPerCoreHour:   decimal.RequireFromString("0.0371"),
		MemoryPerGiBHour: decimal.RequireFromString("0.003975"),
		Source:           pricing.SourceCatalogue,
	}}
}

func newTestRouter(inv Inventory) http.Handler {
	return NewRouter(RouterOptions{
		Log: discardLogger(), Readiness: health.NewAggregator(time.Second),
		Inventory: inv, Pricer: defaultStubPricer(), Reports: &stubReports{},
	})
}

func newTestRouterWithPricer(inv Inventory, p pricing.Provider) http.Handler {
	return NewRouter(RouterOptions{
		Log: discardLogger(), Readiness: health.NewAggregator(time.Second),
		Inventory: inv, Pricer: p, Reports: &stubReports{},
	})
}

func TestListNodes(t *testing.T) {
	t.Parallel()

	inv := &stubInventory{nodes: []domain.Node{
		{Name: "worker-1", InstanceType: "m5.large", Zone: "ap-south-1a",
			CapacityType: "on-demand", CapacityCPUMillicores: 2000, AllocatableCPUMillicores: 1900},
		{Name: "worker-2", InstanceType: "m5.xlarge", Zone: "ap-south-1b",
			CapacityType: "spot", CapacityCPUMillicores: 4000, AllocatableCPUMillicores: 3900},
	}}

	rec := httptest.NewRecorder()
	newTestRouter(inv).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var got listResponse[pricedNode]
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if got.Count != 2 || len(got.Items) != 2 {
		t.Fatalf("Count=%d len(Items)=%d, want 2 and 2", got.Count, len(got.Items))
	}
	// The pricing engine in Phase 3 keys on instance type and capacity type, so those
	// two fields surviving serialisation is the point of this endpoint.
	if got.Items[1].InstanceType != "m5.xlarge" || got.Items[1].CapacityType != "spot" {
		t.Errorf("node cloud metadata lost in serialisation: %+v", got.Items[1])
	}
	// Capacity and allocatable must remain distinguishable end to end -- conflating
	// them understates cost by the kubelet's reserve.
	if got.Items[0].CapacityCPUMillicores == got.Items[0].AllocatableCPUMillicores {
		t.Error("capacity and allocatable collapsed into one value")
	}
}

func TestListPods_PassesNamespaceFilter(t *testing.T) {
	t.Parallel()

	inv := &stubInventory{pods: []domain.Pod{{
		UID: "abc", Name: "api-1", Namespace: "team-payments", QoSClass: "Burstable",
		Workload:              domain.Workload{Kind: "Deployment", Name: "api"},
		RequestsCPUMillicores: 500, RequestsMemoryBytes: 536870912,
	}}}

	rec := httptest.NewRecorder()
	newTestRouter(inv).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/api/v1/pods?namespace=team-payments", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if inv.gotNamespace != "team-payments" {
		t.Errorf("handler passed namespace %q down, want %q", inv.gotNamespace, "team-payments")
	}

	var got listResponse[domain.Pod]
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	// Ownership must survive to the client: it is what gives a workload stable cost
	// history across rollouts.
	if got.Items[0].Workload.Kind != "Deployment" || got.Items[0].Workload.Name != "api" {
		t.Errorf("workload attribution lost: %+v", got.Items[0].Workload)
	}
}

func TestListPods_NoFilterMeansAllNamespaces(t *testing.T) {
	t.Parallel()

	inv := &stubInventory{}
	rec := httptest.NewRecorder()
	newTestRouter(inv).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if inv.gotNamespace != "" {
		t.Errorf("namespace = %q, want empty to mean all namespaces", inv.gotNamespace)
	}
}

// TestListPods_RejectsInvalidNamespace covers boundary validation. A malformed value
// cannot match any real namespace, so 400 is both more correct and more useful than
// returning an empty list that looks like "this namespace has nothing in it".
func TestListPods_RejectsInvalidNamespace(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"Team-Payments",    // uppercase
		"team payments",    // space
		"-leading",         // leading hyphen
		"trailing-",        // trailing hyphen
		"team/payments",    // slash
		"team_payments",    // underscore
		"'; DROP TABLE --", // the shape of an injection attempt
	} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			inv := &stubInventory{}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil)
			// Set via the query values so the value is properly encoded rather than
			// accidentally testing URL parsing.
			q := req.URL.Query()
			q.Set("namespace", bad)
			req.URL.RawQuery = q.Encode()

			newTestRouter(inv).ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d for namespace %q, want 400", rec.Code, bad)
			}
			// It must be rejected at the boundary, never reaching the data layer.
			if inv.gotNamespace != "" {
				t.Errorf("invalid namespace %q was passed down to the inventory", inv.gotNamespace)
			}

			var body ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error response is not our envelope: %v", err)
			}
			if body.Error.Code != "invalid_parameter" {
				t.Errorf("error code = %q, want %q", body.Error.Code, "invalid_parameter")
			}
		})
	}
}

// TestListEndpoints_EmptyResultIsArrayNotNull pins down a detail that would otherwise
// bite the frontend forever: a nil Go slice marshals to `null`, and `null.length`
// throws in JavaScript.
func TestListEndpoints_EmptyResultIsArrayNotNull(t *testing.T) {
	t.Parallel()

	// All three fields nil, as an empty cluster would produce.
	inv := &stubInventory{}

	for _, path := range []string{"/api/v1/nodes", "/api/v1/namespaces", "/api/v1/pods"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			newTestRouter(inv).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			// Assert on the raw JSON, because unmarshalling into a Go slice would
			// happily accept null and hide the very thing under test.
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			if string(raw["items"]) != "[]" {
				t.Errorf("items = %s, want [] so the frontend never sees null", raw["items"])
			}
		})
	}
}

func TestListEndpoints_InventoryErrorIs500(t *testing.T) {
	t.Parallel()

	inv := &stubInventory{err: errors.New("informer cache exploded")}

	for _, path := range []string{"/api/v1/nodes", "/api/v1/namespaces", "/api/v1/pods"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			newTestRouter(inv).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", rec.Code)
			}
			// The internal error text must NOT reach the client.
			if body := rec.Body.String(); contains(body, "informer cache exploded") {
				t.Errorf("response leaked the internal error: %s", body)
			}
		})
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Pricing attached to nodes
// -----------------------------------------------------------------------------

// TestListNodes_MoneyIsAJSONString pins a contract that is easy to break and expensive to
// discover.
//
// decimal.Decimal marshals to a QUOTED string by default. That default is correct here:
// JSON numbers are parsed as IEEE-754 doubles by every JavaScript client, so a rate sent as
// a bare number arrives in the browser as the nearest binary double -- reintroducing at the
// very last step the imprecision that numeric(20,10) and decimal.Decimal exist to prevent.
//
// If someone "tidies" these to float64, this test fails.
func TestListNodes_MoneyIsAJSONString(t *testing.T) {
	t.Parallel()

	inv := &stubInventory{nodes: []domain.Node{{Name: "w1", InstanceType: "m5.large"}}}
	rec := httptest.NewRecorder()
	newTestRouter(inv).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// Assert on the RAW JSON. Unmarshalling into a struct would accept either form and hide
	// exactly what is under test.
	var envelope struct {
		Items []struct {
			Pricing map[string]json.RawMessage `json:"pricing"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(envelope.Items) != 1 {
		t.Fatalf("got %d nodes, want 1", len(envelope.Items))
	}

	for _, field := range []string{"node_hourly", "cpu_per_core_hour", "memory_per_gib_hour"} {
		raw, ok := envelope.Items[0].Pricing[field]
		if !ok {
			t.Errorf("pricing.%s is missing from the response", field)
			continue
		}
		if !strings.HasPrefix(string(raw), `"`) {
			t.Errorf("pricing.%s = %s, want a quoted string. A bare JSON number is parsed as "+
				"float64 by every JS client and loses precision", field, raw)
		}
	}
}

// TestListNodes_IncludesRatesAndProvenance checks the values and, critically, that the
// source is surfaced. A cost derived from a guessed fallback must never look identical to
// one derived from a real catalogue entry.
func TestListNodes_IncludesRatesAndProvenance(t *testing.T) {
	t.Parallel()

	inv := &stubInventory{nodes: []domain.Node{{Name: "w1", InstanceType: "m5.large", Zone: "ap-south-1a"}}}
	rec := httptest.NewRecorder()
	newTestRouter(inv).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))

	var got listResponse[pricedNode]
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("got %d nodes, want 1", len(got.Items))
	}
	n := got.Items[0]

	// The embedded domain.Node fields must still be present and flat, not nested.
	if n.Name != "w1" || n.InstanceType != "m5.large" || n.Zone != "ap-south-1a" {
		t.Errorf("node fields lost by embedding: %+v", n.Node)
	}
	if n.Pricing.Currency != "USD" {
		t.Errorf("currency = %q, want USD", n.Pricing.Currency)
	}
	if n.Pricing.NodeHourly != "0.106" {
		t.Errorf("node_hourly = %q, want 0.106", n.Pricing.NodeHourly)
	}
	if n.Pricing.Source != string(pricing.SourceCatalogue) {
		t.Errorf("source = %q, want %q; provenance must reach the client",
			n.Pricing.Source, pricing.SourceCatalogue)
	}
}

// TestListNodes_UnpriceableNodeIsStillReported covers the case that decides whether the
// endpoint is usable on a real cluster.
//
// One node with an unrecognised instance type must not fail the whole request, and must not
// be silently omitted either: the node exists and costs money, so hiding it understates the
// cluster. Reporting it with an explicit error is the only option that neither lies nor hides.
func TestListNodes_UnpriceableNodeIsStillReported(t *testing.T) {
	t.Parallel()

	inv := &stubInventory{nodes: []domain.Node{
		{Name: "priced", InstanceType: "m5.large"},
		{Name: "exotic", InstanceType: "m7i.metal-48xl"},
	}}
	pricer := defaultStubPricer()
	pricer.errFor = "exotic"

	rec := httptest.NewRecorder()
	newTestRouterWithPricer(inv, pricer).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))

	// NOT a 500. One unpriceable node must not deny the whole report.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: one unpriceable node must not fail the request", rec.Code)
	}

	var got listResponse[pricedNode]
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// BOTH nodes present. Omitting the unpriceable one would hide real capacity.
	if got.Count != 2 {
		t.Fatalf("got %d nodes, want 2 (the unpriceable node must not be dropped)", got.Count)
	}

	byName := map[string]pricedNode{}
	for _, n := range got.Items {
		byName[n.Name] = n
	}
	if byName["priced"].Pricing.Error != "" {
		t.Errorf("the priceable node carries an error: %q", byName["priced"].Pricing.Error)
	}
	if byName["exotic"].Pricing.Error == "" {
		t.Error("the unpriceable node carries no error; the caller cannot tell its cost is unknown")
	}
	// And it must NOT be reported as costing zero, which would look like a real figure.
	if byName["exotic"].Pricing.NodeHourly != "" {
		t.Errorf("unpriceable node reports node_hourly=%q; an absent price must be absent, "+
			"not zero", byName["exotic"].Pricing.NodeHourly)
	}
}
