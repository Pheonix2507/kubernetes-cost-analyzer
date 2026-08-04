package httpapi

import (
	"net/http"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
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

// handleListNodes serves GET /api/v1/nodes.
//
// This is the endpoint the pricing engine will consume in Phase 3: instance type,
// region, zone and capacity type are exactly the inputs a rate lookup needs.
func handleListNodes(inv Inventory) http.HandlerFunc {
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
		writeJSON(w, r, http.StatusOK, newListResponse(nodes))
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
