package kube

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/config"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

// These tests use k8s.io/client-go/kubernetes/fake, which implements the full
// kubernetes.Interface backed by an in-memory object tracker -- including WATCH, which
// is what makes it possible to exercise real informers with no API server.
//
// It is not a perfect substitute for a cluster: the fake tracker does not run admission,
// does not populate defaults, and does not compute status fields (notably QOSClass,
// which the API server fills in). So fixtures here set status explicitly. That
// limitation is precisely why convert_test.go tests the translation logic separately as
// pure functions, and these tests focus on the informer LIFECYCLE instead.

func testLogger() *slog.Logger {
	// slog.DiscardHandler (Go 1.24+) rather than a TextHandler over io.Discard: it
	// short-circuits before formatting, so a test logger costs nothing at all.
	return slog.New(slog.DiscardHandler)
}

func testKubeConfig() config.Kube {
	return config.Kube{
		ResyncInterval:   0,
		CacheSyncTimeout: 10 * time.Second,
		QPS:              50,
		Burst:            100,
	}
}

func newTestStore(t *testing.T, objects ...runtime.Object) *Store {
	t.Helper()
	return NewStore(fake.NewClientset(objects...), testKubeConfig(), testLogger())
}

// forbidListReactor makes every List fail with a 403, which is what RBAC missing the
// `list` verb actually looks like. The reflector retries with backoff and never syncs,
// so WaitForCacheSync hits its deadline.
//
// Returning an error is better than blocking the reactor: a blocked reactor would leave
// a goroutine parked for the test binary's lifetime, and this reproduces the real
// failure more faithfully.
func forbidListReactor(action k8stesting.Action) (bool, runtime.Object, error) {
	gvr := action.GetResource()
	return true, nil, apierrors.NewForbidden(
		schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource},
		"", errors.New("RBAC: list is not permitted"))
}

// -----------------------------------------------------------------------------
// Lifecycle
// -----------------------------------------------------------------------------

func TestStore_StartSyncsAndServes(t *testing.T) {
	t.Parallel()

	store := newTestStore(t,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:   "worker-1",
			Labels: map[string]string{labelInstanceType: "m5.large", labelCapacityType: "spot"},
		}, Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1900m")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "team-payments", Labels: map[string]string{labelTeam: "payments"},
		}},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := store.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	if err := store.Check(ctx); err != nil {
		t.Errorf("Check() = %v, want nil after a successful sync", err)
	}

	nodes, err := store.Nodes()
	if err != nil {
		t.Fatalf("Nodes() = %v", err)
	}
	if len(nodes) != 1 || nodes[0].InstanceType != "m5.large" || nodes[0].CapacityType != "spot" {
		t.Errorf("Nodes() = %+v, want one m5.large spot node", nodes)
	}
}

// TestStore_NotReadyBeforeSync pins the startup contract that cmd/api depends on: the
// HTTP server begins listening before caches are warm, so /readyz MUST report down in
// that window rather than serving an empty cluster as though it were real.
func TestStore_NotReadyBeforeSync(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	err := store.Check(context.Background())
	if err == nil {
		t.Fatal("Check() = nil before Start; want an error so /readyz reports down")
	}
	if !errors.Is(err, ErrCachesNotSynced) {
		t.Errorf("Check() = %v, want it to wrap ErrCachesNotSynced", err)
	}
}

// TestStore_StartIsCleanOnContextCancellation is a REGRESSION TEST for a real bug.
//
// THE BUG: WaitForCacheSync returns a map of all-false when its stop channel closes,
// which is indistinguishable from "the API server never answered". Start treated that as
// a sync failure and returned an error.
//
// So pressing Ctrl-C (or the kubelet sending SIGTERM) DURING startup made run() return
// an error and the process exit 1. In Kubernetes that is recorded as a crash: a pod
// terminated during a rollout or a node drain would show as failed rather than as a
// normal shutdown, and it would count towards CrashLoopBackOff.
//
// A cancelled context is a REQUEST TO STOP, not a failure, and must exit 0.
func TestStore_StartIsCleanOnContextCancellation(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	// Already cancelled: the same state Start observes when SIGTERM lands mid-sync.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.Start(ctx)
	if err != nil {
		t.Errorf("Start() with a cancelled context = %v, want nil "+
			"(shutdown during startup is not a failure and must exit 0)", err)
	}
	// And it must NOT claim to be synced, so readiness stays honest on the way down.
	if store.Check(context.Background()) == nil {
		t.Error("Check() = nil after an aborted sync; want an error")
	}
}

func TestStore_StartTimesOutWhenCachesCannotSync(t *testing.T) {
	t.Parallel()

	// A fake client whose List is blocked forever simulates RBAC forbidding `list`, or
	// an unreachable API server. This must FAIL -- it is a genuine inability to serve,
	// as distinct from the cancellation case above.
	client := fake.NewClientset()
	client.PrependReactor("list", "*", forbidListReactor)

	cfg := testKubeConfig()
	cfg.CacheSyncTimeout = 300 * time.Millisecond
	store := NewStore(client, cfg, testLogger())

	err := store.Start(context.Background())
	if err == nil {
		t.Fatal("Start() = nil when caches cannot sync; want an error")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("Start() = %v; a timeout must not be reported as a cancellation", err)
	}
}

// -----------------------------------------------------------------------------
// Ownership resolution against a real informer
// -----------------------------------------------------------------------------

// TestStore_ResolvesPodToDeployment exercises the two-hop resolution through the actual
// ReplicaSet informer, rather than the map-backed stub in convert_test.go. This is what
// proves the informer is registered and its lister populated -- a mistake convert_test
// cannot catch.
func TestStore_ResolvesPodToDeployment(t *testing.T) {
	t.Parallel()

	store := newTestStore(t,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Name:      "api-55758c88bb",
			Namespace: "team-payments",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "Deployment", Name: "api", Controller: ptr(true),
			}},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      "api-55758c88bb-abcde",
			Namespace: "team-payments",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "ReplicaSet", Name: "api-55758c88bb", Controller: ptr(true),
			}},
		}},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := store.Start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}

	pods, err := store.Pods("team-payments")
	if err != nil {
		t.Fatalf("Pods() = %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("got %d pods, want 1", len(pods))
	}
	// The hash-suffixed ReplicaSet must NOT be the reported workload: its name changes
	// on every rollout, which would reset each workload's cost history.
	want := domain.Workload{Kind: "Deployment", Name: "api"}
	if pods[0].Workload != want {
		t.Errorf("Workload = %+v, want %+v", pods[0].Workload, want)
	}
}

// -----------------------------------------------------------------------------
// Cache safety
// -----------------------------------------------------------------------------

// TestStore_DoesNotAliasCacheMaps is a REGRESSION TEST for a real bug.
//
// THE BUG: toNamespace and toPod assigned the Kubernetes object's Labels map straight
// into our struct. Those objects are POINTERS INTO THE SHARED INFORMER CACHE, so the map
// was shared too. Any caller mutating Pod.Labels would silently corrupt the cache for
// every other reader in the process -- and there is no lock to protect it, so it is also
// a data race with the informer's own writes.
//
// The whole justification for the translation layer was that it is a safety boundary.
// Handing out an interior pointer defeated it while looking completely harmless.
func TestStore_DoesNotAliasCacheMaps(t *testing.T) {
	t.Parallel()

	store := newTestStore(t,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "team-payments", Labels: map[string]string{labelTeam: "payments"},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "p1", Namespace: "team-payments",
			Labels: map[string]string{"app": "api"},
		}},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := store.Start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}

	// First read, then mutate what we were given -- exactly what a careless consumer,
	// or a future handler adding a computed label, would do.
	pods, _ := store.Pods("")
	namespaces, _ := store.Namespaces()
	if len(pods) != 1 || len(namespaces) == 0 {
		t.Fatalf("fixture not loaded: %d pods, %d namespaces", len(pods), len(namespaces))
	}
	pods[0].Labels["app"] = "MUTATED"
	pods[0].Labels["injected"] = "yes"
	for i := range namespaces {
		if namespaces[i].Name == "team-payments" {
			namespaces[i].Labels[labelTeam] = "MUTATED"
		}
	}

	// Second, independent read. If the maps were aliased, the cache now carries our
	// mutations and every other reader sees them.
	pods2, _ := store.Pods("")
	namespaces2, _ := store.Namespaces()

	if got := pods2[0].Labels["app"]; got != "api" {
		t.Errorf("pod label app = %q after a caller mutated its map, want %q "+
			"(the informer cache was corrupted)", got, "api")
	}
	if _, injected := pods2[0].Labels["injected"]; injected {
		t.Error("a caller injected a label into the shared informer cache")
	}
	for _, ns := range namespaces2 {
		if ns.Name == "team-payments" && ns.Team != "payments" {
			t.Errorf("namespace team = %q after mutation, want %q", ns.Team, "payments")
		}
	}
}

// TestStore_ResultsAreSorted guards determinism. The cache is a map and Go randomises
// map iteration, so without an explicit sort the same request returns the same objects
// in a different order each time -- which breaks client-side diffing, makes response
// caching useless, and turns any golden-file test into a flake.
func TestStore_ResultsAreSorted(t *testing.T) {
	t.Parallel()

	store := newTestStore(t,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "zzz"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "aaa"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "mmm"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns2"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns2"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: "ns1"}},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := store.Start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}

	// Repeat, because a single pass can pass by luck on a randomised map order.
	for i := 0; i < 20; i++ {
		nodes, _ := store.Nodes()
		if len(nodes) != 3 || nodes[0].Name != "aaa" || nodes[1].Name != "mmm" || nodes[2].Name != "zzz" {
			t.Fatalf("iteration %d: nodes not sorted by name: %v", i, names(nodes))
		}

		pods, _ := store.Pods("")
		// Namespace first, then name.
		want := []string{"ns1/z", "ns2/a", "ns2/b"}
		got := make([]string, 0, len(pods))
		for _, p := range pods {
			got = append(got, p.Namespace+"/"+p.Name)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("iteration %d: pods not sorted: got %v want %v", i, got, want)
			}
		}
	}
}

func TestStore_PodsNamespaceFilter(t *testing.T) {
	t.Parallel()

	store := newTestStore(t,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "team-payments"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "team-search"}},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := store.Start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}

	all, _ := store.Pods("")
	if len(all) != 2 {
		t.Errorf("Pods(\"\") returned %d pods, want 2 (empty must mean all namespaces)", len(all))
	}

	scoped, _ := store.Pods("team-payments")
	if len(scoped) != 1 || scoped[0].Namespace != "team-payments" {
		t.Errorf("Pods(\"team-payments\") = %+v, want exactly the one pod in it", scoped)
	}

	// A namespace that does not exist must be an empty result, not an error: it is a
	// legitimate question with the answer "nothing".
	none, err := store.Pods("does-not-exist")
	if err != nil {
		t.Errorf("Pods() for an unknown namespace returned an error: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("got %d pods for an unknown namespace, want 0", len(none))
	}
}

func names(nodes []domain.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}
