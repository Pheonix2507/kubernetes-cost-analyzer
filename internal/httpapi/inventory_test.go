package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
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

func newTestRouter(inv Inventory) http.Handler {
	return NewRouter(discardLogger(), health.NewAggregator(time.Second), inv)
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

	var got listResponse[domain.Node]
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
