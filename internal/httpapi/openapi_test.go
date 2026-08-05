package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// TestOpenAPISpec_MatchesTheCode is what makes a hand-written spec defensible.
//
// A generated spec cannot drift from the implementation; a hand-written one can, and a documented
// enum that no longer matches what the server accepts is worse than no documentation at all -- a
// client trusts it, sends a value the API rejects, and the spec is the thing that lied.
//
// So the spec's enums are compared against the SAME allow-lists the query builder uses. Adding a
// grouping without documenting it fails here, which is most of the drift protection a generator
// would give, without the contract becoming an unreviewed output.
func TestOpenAPISpec_MatchesTheCode(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "api", "openapi.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read the spec at %s: %v", path, err)
	}

	// Parsed into a generic tree rather than a typed OpenAPI struct: a full model would need a
	// dependency, and every assertion here is a lookup rather than validation.
	var spec map[string]any
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("the spec is not valid YAML: %v", err)
	}

	tests := []struct {
		name       string
		path       []string
		wantValues []string
		why        string
	}{
		{
			name:       "group_by enum",
			path:       []string{"paths", "/api/v1/costs/summary", "get", "parameters"},
			wantValues: postgres.GroupByOptions(),
			why: "a documented grouping the server rejects means the spec lied to a client, and a " +
				"grouping the server accepts but the spec omits is invisible",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := navigate(t, spec, tt.path...)
			list, ok := params.([]any)
			if !ok {
				t.Fatalf("%v is not a list", tt.path)
			}

			documented := enumFor(list, "group_by")
			if documented == nil {
				t.Fatalf("group_by has no enum in the spec")
			}

			assertSameSet(t, documented, tt.wantValues, tt.why)
		})
	}

	// The sort enum, checked the same way.
	params, _ := navigate(t, spec, "paths", "/api/v1/costs/summary", "get", "parameters").([]any)
	sortEnum := enumFor(params, "sort")
	if sortEnum == nil {
		t.Fatal("sort has no enum in the spec")
	}
	assertSameSet(t, sortEnum, postgres.SortFieldOptions(),
		"a documented sort field the server rejects is a 400 the client could not have predicted")
}

// TestOpenAPISpec_DocumentsEveryRoute catches the commonest drift: a route added to the router and
// never written down. An undocumented endpoint is one no client can discover and nobody maintains.
func TestOpenAPISpec_DocumentsEveryRoute(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("cannot read the spec: %v", err)
	}
	var spec struct {
		Paths map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}

	// Every route NewRouter registers. Kept here by hand deliberately: an automatic list read from
	// the mux would drift in lockstep with the router and prove nothing.
	routes := []string{
		"/healthz", "/readyz", "/version",
		"/api/v1/nodes", "/api/v1/namespaces", "/api/v1/pods",
		"/api/v1/costs/summary", "/api/v1/allocations",
	}
	for _, route := range routes {
		if _, documented := spec.Paths[route]; !documented {
			t.Errorf("route %s is served but not documented in api/openapi.yaml", route)
		}
	}
	// And the reverse: a documented route that no longer exists sends clients to a 404.
	if len(spec.Paths) != len(routes) {
		t.Errorf("the spec documents %d paths but the router serves %d; a documented route that "+
			"does not exist sends clients to a 404", len(spec.Paths), len(routes))
	}
}

// TestOpenAPISpec_ProbesAreUnauthenticated asserts the spec records the security exemption. The
// kubelet cannot present a credential, so a spec claiming probes need auth would mislead anyone
// writing a Deployment.
func TestOpenAPISpec_ProbesAreUnauthenticated(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("cannot read the spec: %v", err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}

	for _, probe := range []string{"/healthz", "/readyz", "/version"} {
		get, ok := navigate(t, spec, "paths", probe, "get").(map[string]any)
		if !ok {
			t.Fatalf("%s has no get operation", probe)
		}
		sec, present := get["security"]
		if !present {
			t.Errorf("%s does not override security; it would inherit the global bearerAuth "+
				"requirement, which the kubelet cannot satisfy", probe)
			continue
		}
		list, ok := sec.([]any)
		if !ok || len(list) != 0 {
			t.Errorf("%s security = %v, want an empty list meaning no authentication", probe, sec)
		}
	}
}

// navigate walks a nested map by key, failing the test with the path so far on a miss.
func navigate(t *testing.T, root any, path ...string) any {
	t.Helper()
	cur := root
	for i, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%v is not a map", path[:i])
		}
		cur, ok = m[key]
		if !ok {
			t.Fatalf("%v is missing from the spec", path[:i+1])
		}
	}
	return cur
}

// enumFor finds a named parameter's enum values in a parameter list.
func enumFor(params []any, name string) []string {
	for _, p := range params {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if m["name"] != name {
			continue
		}
		schema, ok := m["schema"].(map[string]any)
		if !ok {
			return nil
		}
		values, ok := schema["enum"].([]any)
		if !ok {
			return nil
		}
		out := make([]string, 0, len(values))
		for _, v := range values {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// assertSameSet compares two string sets, reporting each difference by direction so the failure
// says which side is wrong.
func assertSameSet(t *testing.T, documented, implemented []string, why string) {
	t.Helper()

	docSet := map[string]bool{}
	for _, d := range documented {
		docSet[d] = true
	}
	implSet := map[string]bool{}
	for _, i := range implemented {
		implSet[i] = true
	}

	var missing, extra []string
	for _, i := range implemented {
		if !docSet[i] {
			missing = append(missing, i)
		}
	}
	for _, d := range documented {
		if !implSet[d] {
			extra = append(extra, d)
		}
	}

	if len(missing) > 0 {
		t.Errorf("the code accepts values the spec does not document: %s\nwhy this matters: %s",
			strings.Join(missing, ", "), why)
	}
	if len(extra) > 0 {
		t.Errorf("the spec documents values the code rejects: %s\nwhy this matters: %s",
			strings.Join(extra, ", "), why)
	}
}
