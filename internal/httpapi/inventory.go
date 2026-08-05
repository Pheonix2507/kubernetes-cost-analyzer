package httpapi

import (
	"net/http"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/pricing"
)

// Inventory is the read-only cluster topology this API serves.
//
// WHY AN INTERFACE HERE RATHER THAN TAKING *kube.Store DIRECTLY
// -------------------------------------------------------------
// Declared in the CONSUMING package and satisfied implicitly by *kube.Store, which is
// the Go idiom and has two concrete payoffs:
//
//  1. TESTABILITY. Handler tests pass a struct literal returning fixed data. No
//     informer, no fake clientset, no cluster -- see inventory_test.go.
//  2. It states exactly what the HTTP layer needs. Three read methods. *kube.Store also
//     has Start and Check, and a handler has no business calling either.
//
// It returns internal/domain types rather than internal/kube ones. Those types lived in
// kube until Phase 2, when Postgres became a third package speaking the same concepts and
// a persistence layer importing a Kubernetes client package stopped making sense. See the
// package comment on internal/domain.
type Inventory interface {
	Nodes() ([]domain.Node, error)
	Namespaces() ([]domain.Namespace, error)
	Pods(namespace string) ([]domain.Pod, error)
}

// listResponse wraps every collection response.
//
// WHY AN ENVELOPE AND NOT A BARE JSON ARRAY
// -----------------------------------------
// Returning `[{...},{...}]` works until the first time you need to add anything
// alongside the data -- a total count, a pagination cursor, a warning that one
// informer is stale. At that point the response shape must change from array to
// object, which breaks every existing client. An envelope costs six characters now and
// makes Phase 5's pagination a purely additive change.
//
// Generic over T so nodes, namespaces and pods share one shape without three near
// identical structs. This is the case generics are genuinely for: the container's
// behaviour is identical and only the element type differs.
type listResponse[T any] struct {
	Items []T `json:"items"`
	// Count is the number of items in THIS response. Once Phase 5 adds pagination it
	// will be joined by a separate total, and the distinction will matter -- so the
	// name is deliberately not "total".
	Count int `json:"count"`
}

// newListResponse guarantees Items is never nil.
//
// A nil slice marshals to JSON `null`, not `[]`, and `null.length` throws in
// JavaScript. The frontend would need a defensive check on every list it renders,
// forever, because of a Go representation detail. Fix it once, here.
func newListResponse[T any](items []T) listResponse[T] {
	if items == nil {
		items = []T{}
	}
	return listResponse[T]{Items: items, Count: len(items)}
}

// nodeRates is the pricing attached to a node in the API response.
//
// WHY EVERY MONETARY FIELD IS A JSON STRING, NOT A NUMBER
// ------------------------------------------------------
// decimal.Decimal marshals to a quoted string by default, and that default is correct here.
// JSON numbers are parsed as IEEE-754 doubles by every JavaScript client, so a rate of
// 0.0039750 sent as a bare number arrives in the browser as the nearest binary double --
// reintroducing, at the very last step, exactly the imprecision the numeric column and the
// decimal type exist to prevent.
//
// A string crosses the wire exactly. The frontend formats it for display and never does
// arithmetic on it; aggregation happens in SQL, where it is exact.
type nodeRates struct {
	Currency         string `json:"currency"`
	NodeHourly       string `json:"node_hourly"`
	CPUPerCoreHour   string `json:"cpu_per_core_hour"`
	MemoryPerGiBHour string `json:"memory_per_gib_hour"`
	// Source is "catalogue", "explicit_rates" or "fallback". Surfaced deliberately: a cost
	// derived from a guessed fallback rate must never be indistinguishable from one derived
	// from a real catalogue entry, or someone will take a fabricated figure to a finance
	// meeting.
	Source string `json:"source"`
	// Error explains why a node could not be priced, when it could not.
	Error string `json:"error,omitempty"`
}

// pricedNode is a node plus its rates.
//
// Composed by EMBEDDING domain.Node rather than copying its fields, so a field added to the
// domain type appears here automatically and cannot be forgotten. The embedded struct's json
// tags are promoted, so the wire shape stays flat.
type pricedNode struct {
	domain.Node
	Pricing nodeRates `json:"pricing"`
}

// handleListNodes serves GET /api/v1/nodes, with rates attached.
//
// This is where the whole chain becomes visible in one response: an informer read the node's
// labels, the catalogue turned its instance type into a price, and the split turned that
// price into per-resource rates.
func handleListNodes(inv Inventory, pricer pricing.Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nodes, err := inv.Nodes()
		if err != nil {
			// The error is logged with full detail and the client gets a generic
			// message. Cache errors can name internal hosts, and a 500 body is not the
			// place to disclose them -- the request ID in the response ties the two
			// together for whoever debugs it.
			logError(r, "listing nodes", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not list nodes")
			return
		}

		out := make([]pricedNode, 0, len(nodes))
		for _, n := range nodes {
			priced := pricedNode{Node: n}

			rates, rateErr := pricer.RatesFor(r.Context(), n)
			if rateErr != nil {
				// A NODE THAT CANNOT BE PRICED IS STILL REPORTED, with the reason.
				//
				// Failing the whole request because one node in fifty has an unrecognised
				// instance type would make the endpoint useless, and omitting the node
				// silently would hide capacity that genuinely exists and genuinely costs
				// money. Reporting it with an explicit error is the only option that neither
				// lies nor hides.
				priced.Pricing = nodeRates{Error: rateErr.Error()}
				out = append(out, priced)
				continue
			}

			priced.Pricing = nodeRates{
				Currency:         rates.Currency,
				NodeHourly:       rates.NodeHourly.String(),
				CPUPerCoreHour:   rates.CPUPerCoreHour.String(),
				MemoryPerGiBHour: rates.MemoryPerGiBHour.String(),
				Source:           string(rates.Source),
			}
			out = append(out, priced)
		}

		writeJSON(w, r, http.StatusOK, newListResponse(out))
	}
}

// handleListNamespaces serves GET /api/v1/namespaces.
func handleListNamespaces(inv Inventory) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespaces, err := inv.Namespaces()
		if err != nil {
			logError(r, "listing namespaces", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not list namespaces")
			return
		}
		writeJSON(w, r, http.StatusOK, newListResponse(namespaces))
	}
}

// handleListPods serves GET /api/v1/pods, optionally filtered by ?namespace=.
//
// Pods are where cost actually lands, so this is the endpoint Phase 4 joins against
// Prometheus usage.
func handleListPods(inv Inventory) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace := r.URL.Query().Get("namespace")

		// Validate before use. A namespace name is a DNS-1123 label, so anything else
		// cannot match a real namespace and is either a client bug or someone probing.
		// Rejecting it with 400 is both more correct and more useful than passing it
		// down to return a confusing empty list.
		//
		// This is not an injection defence -- the lister takes a string, not SQL -- but
		// validating at the boundary is the habit that makes the SQL in Phase 2 safe
		// by default rather than by luck.
		if namespace != "" && !isValidDNS1123Label(namespace) {
			writeError(w, r, http.StatusBadRequest, "invalid_parameter",
				"namespace must be a valid DNS-1123 label")
			return
		}

		pods, err := inv.Pods(namespace)
		if err != nil {
			logError(r, "listing pods", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not list pods")
			return
		}
		writeJSON(w, r, http.StatusOK, newListResponse(pods))
	}
}

// isValidDNS1123Label reports whether s is a valid Kubernetes name segment: lowercase
// alphanumerics and hyphens, 1-63 characters, not starting or ending with a hyphen.
//
// Hand-rolled rather than importing k8s.io/apimachinery/pkg/util/validation, because
// that package pulls a surprising amount of transitive weight into the HTTP layer for
// one regex, and this keeps internal/httpapi free of any Kubernetes dependency beyond
// the types it serves.
func isValidDNS1123Label(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			continue
		case c == '-':
			// A leading or trailing hyphen is invalid.
			if i == 0 || i == len(s)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
