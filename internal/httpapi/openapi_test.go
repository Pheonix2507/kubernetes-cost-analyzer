package httpapi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/recommend"
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

// TestOpenAPISpec_RecommendationEnumsMatchTheEngine extends the same drift guard to the response
// body, which the group_by and sort checks do not cover.
//
// The failure this prevents is quieter than a rejected parameter. A client writes a switch over the
// five documented kinds and renders a label for each. We add a sixth rule, the server starts emitting
// it, and their UI drops those findings on the floor -- no error, no 400, just advice that silently
// never reaches anybody. Response enums drift more dangerously than request enums, because nothing
// rejects an undocumented value.
func TestOpenAPISpec_RecommendationEnumsMatchTheEngine(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("cannot read the spec: %v", err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}

	tests := []struct {
		field       string
		implemented []string
		why         string
	}{
		{"kind", recommend.KindOptions(),
			"a client switching over the documented kinds silently drops any kind we add without documenting"},
		{"severity", recommend.SeverityOptions(),
			"severity drives how a finding is displayed and whether it is escalated; an unknown value falls through"},
		{"confidence", recommend.ConfidenceOptions(),
			"confidence is what tells a reader whether to apply a recommendation or collect more data first"},
	}

	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			node := navigate(t, spec, "components", "schemas", "Recommendation", "properties", tt.field, "enum")
			list, ok := node.([]any)
			if !ok {
				t.Fatalf("Recommendation.%s has no enum in the spec", tt.field)
			}
			documented := make([]string, 0, len(list))
			for _, v := range list {
				documented = append(documented, fmt.Sprint(v))
			}
			assertSameSet(t, documented, tt.implemented, tt.why)
		})
	}
}

// TestOpenAPISpec_TrendEnumsMatchTheCode extends the drift guard to the trend endpoint.
//
// The interval enum is the one that matters most here, because the unit is INTERPOLATED into
// date_trunc rather than bound as a parameter. So the spec's list, the parser's allow-list and the
// repository's map all have to be the same set -- and this asserts the spec against the map the SQL
// actually uses, not against a hand-maintained copy.
func TestOpenAPISpec_TrendEnumsMatchTheCode(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("cannot read the spec: %v", err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}

	params, ok := navigate(t, spec, "paths", "/api/v1/costs/trend", "get", "parameters").([]any)
	if !ok {
		t.Fatal("the trend parameters are not a list")
	}

	t.Run("interval", func(t *testing.T) {
		documented := enumFor(params, "interval")
		if documented == nil {
			t.Fatal("interval has no enum in the spec")
		}
		assertSameSet(t, documented, postgres.IntervalOptions(),
			"the interval is interpolated into date_trunc, so a documented value the code rejects is a "+
				"400 no client could have predicted -- and one the code accepts but the spec omits is invisible")
	})

	t.Run("group_by", func(t *testing.T) {
		documented := enumFor(params, "group_by")
		if documented == nil {
			t.Fatal("group_by has no enum on the trend endpoint")
		}
		// The SAME allow-list the summary uses. The trend routes some groupings to the fact table rather
		// than refusing them, so every grouping must be documented here too.
		assertSameSet(t, documented, postgres.GroupByOptions(),
			"the trend accepts every grouping the summary does -- it routes pod and container to the "+
				"fact table rather than refusing them")
	})

	t.Run("source is documented as an enum", func(t *testing.T) {
		// The response field, not a parameter. Checked because a client that switches on `source` and
		// meets an undocumented third value has no branch for it.
		node := navigate(t, spec, "components", "schemas", "TrendResponse", "properties", "source", "enum")
		list, ok := node.([]any)
		if !ok {
			t.Fatal("TrendResponse.source has no enum")
		}
		got := make([]string, 0, len(list))
		for _, v := range list {
			got = append(got, fmt.Sprint(v))
		}
		assertSameSet(t, got,
			[]string{string(postgres.TrendSourceRollup), string(postgres.TrendSourceFacts)},
			"source is part of the contract, so its possible values must be documented")
	})
}

// TestOpenAPISpec_MonthlyScopesMatchTheCode guards the scope enum against the three constants the
// database CHECK constraint uses.
//
// Three places have to agree: the constraint, the repository's validation, and the spec. A fourth scope
// added to the code and forgotten in the spec would be invisible to every client.
func TestOpenAPISpec_MonthlyScopesMatchTheCode(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("cannot read the spec: %v", err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}

	want := []string{postgres.ScopeCluster, postgres.ScopeNamespace, postgres.ScopeTeam}

	params, _ := navigate(t, spec, "paths", "/api/v1/reports/monthly", "get", "parameters").([]any)
	documented := enumFor(params, "scope_kind")
	if documented == nil {
		t.Fatal("scope_kind has no enum in the spec")
	}
	assertSameSet(t, documented, want,
		"scope_kind is constrained by the database, so a value the spec documents and the constraint "+
			"rejects would be a 500 rather than a 400")

	// And on the response schema, so a client can exhaustively switch on what it receives.
	node := navigate(t, spec, "components", "schemas", "MonthlyReport", "properties", "scope_kind", "enum")
	list, ok := node.([]any)
	if !ok {
		t.Fatal("MonthlyReport.scope_kind has no enum")
	}
	got := make([]string, 0, len(list))
	for _, v := range list {
		got = append(got, fmt.Sprint(v))
	}
	assertSameSet(t, got, want, "the response enum must match the request enum and the constraint")
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
		"/api/v1/costs/summary", "/api/v1/allocations", "/api/v1/recommendations",
		"/api/v1/costs/trend", "/api/v1/reports/monthly",
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
