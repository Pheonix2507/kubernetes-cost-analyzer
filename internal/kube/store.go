package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/config"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
)

// ErrCachesNotSynced means the informer caches have not finished their initial List, so
// any answer we gave would describe an empty cluster. A sentinel so /readyz can be
// tested for the specific condition rather than by matching on message text.
var ErrCachesNotSynced = errors.New("informer caches not yet synced")

// Compile-time proof that *Store can be a readiness dependency. See the same pattern
// in internal/store/postgres: it declares intent where the type lives, so a renamed
// method fails here instead of confusingly at the call site in main.
var _ health.Checker = (*Store)(nil)

// Store is an in-memory, always-current replica of the cluster objects we care about.
//
// WHY AN INFORMER-BACKED CACHE RATHER THAN CALLING List
// ----------------------------------------------------
// The naive approach is clientset.CoreV1().Pods("").List() whenever someone hits our
// API. It is simple, and it is how people take down their own control plane:
//
//   - Every List serialises EVERY pod out of etcd, through the API server, to us. On a
//     5,000-pod cluster that is tens of megabytes per call.
//   - It scales with OUR traffic, not with cluster churn. Ten dashboard users refreshing
//     means ten full cluster dumps.
//
// An informer inverts that: List ONCE at startup, then hold a long-lived Watch and
// apply deltas. Reads are then served from local memory with zero API calls, so our
// read traffic costs the control plane nothing at all. This is how every controller in
// Kubernetes works, and kube-state-metrics is little more than this plus a metrics
// renderer.
//
// WHAT AN INFORMER IS MADE OF
//
//	Reflector ──List, then Watch forever──► DeltaFIFO ──► Indexer (thread-safe store)
//	    │                                                      │
//	    └── owns resourceVersion, reconnects, 410 Gone          └── Lister reads from here
//
// The Reflector handling "410 Gone" is the part worth appreciating. The API server
// keeps only a bounded history of changes; if our watch falls far enough behind that
// our resourceVersion is no longer in that window, it returns 410 and the Reflector
// must re-List from scratch and reconcile. Hand-rolling a Watch means hand-rolling
// that, and getting it wrong means silently serving stale data forever.
type Store struct {
	factory informers.SharedInformerFactory

	// Listers are READ-ONLY views over the shared cache. They perform no I/O.
	//
	// CRITICAL: the objects they return are POINTERS INTO THE SHARED CACHE. They must
	// never be mutated -- every other reader in the process sees the same memory, and
	// there is no lock protecting you. This is why convert.go copies values out into
	// our own types rather than passing Kubernetes objects around: the translation
	// boundary is also the safety boundary.
	nodes      corelisters.NodeLister
	namespaces corelisters.NamespaceLister
	pods       corelisters.PodLister

	// replicaSets exists ONLY to resolve pod ownership one hop further, so a pod
	// reports its Deployment instead of a hash-suffixed ReplicaSet. See
	// resolveWorkload for why that matters.
	//
	// It is a real cost: another watch, and another full object set in memory. Worth
	// it, because without it no workload has stable cost history across a rollout.
	replicaSets appslisters.ReplicaSetLister

	log              *slog.Logger
	cacheSyncTimeout time.Duration

	// synced is atomic.Bool, not a plain bool, because it is WRITTEN by Start on the
	// startup goroutine and READ by Check on every HTTP handler goroutine serving
	// /readyz. A plain bool there is a genuine data race: `go test -race` flags it, and
	// on a weak memory model a handler could observe a stale false indefinitely.
	//
	// atomic.Bool rather than a sync.Mutex because this is a single flag with no
	// invariant tying it to other fields. A mutex would work and would be heavier to
	// read; a RWMutex around one boolean is a common over-reach.
	synced atomic.Bool
}

// NewStore builds the informer factory and the listers. It does NOT start them: call
// Start, then WaitForSync.
//
// Construction and starting are separate on purpose. It lets main wire everything up,
// decide the process is viable, and only then begin background work -- rather than
// having goroutines running while a later dependency might still fail construction.
func NewStore(clientset kubernetes.Interface, cfg config.Kube, log *slog.Logger) *Store {
	// SharedInformerFactory -- the "shared" is the whole point. Ask it for the pod
	// informer from three places and you get ONE informer and ONE watch, not three.
	// Without sharing, each consumer opens its own watch and holds its own copy of
	// every object: triple the API server connections and triple the memory for
	// identical data.
	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		// Resync interval. Zero disables it, which is what we want -- see the long
		// comment on config.Kube.ResyncInterval. Resync re-delivers UNCHANGED objects
		// to handlers; it is a controller's drift-reconciliation tool, not a refresh,
		// and the watch already keeps us current.
		cfg.ResyncInterval,
		// Strip bulky fields before anything is cached. See stripForCache.
		informers.WithTransform(stripForCache),
	)

	return &Store{
		factory:    factory,
		nodes:      factory.Core().V1().Nodes().Lister(),
		namespaces: factory.Core().V1().Namespaces().Lister(),
		pods:       factory.Core().V1().Pods().Lister(),
		// Merely calling .Lister() REGISTERS the informer with the factory. Nothing is
		// watched until Start, and an informer requested after Start has been called
		// will never sync -- a genuinely confusing failure mode, because the lister
		// returns empty results with no error. Register everything here, before Start.
		replicaSets:      factory.Apps().V1().ReplicaSets().Lister(),
		log:              log,
		cacheSyncTimeout: cfg.CacheSyncTimeout,
	}
}

// Start begins the List-and-Watch loops, then blocks until every cache is populated.
//
// Blocking on the initial sync is deliberate: a lister over an unsynced informer
// returns an EMPTY LIST WITH NO ERROR. Serving that would report a cluster containing
// nothing, which for a cost tool means confidently reporting a bill of zero. A hard
// failure is far better than a plausible wrong answer.
func (s *Store) Start(ctx context.Context) error {
	// Start launches one goroutine per registered informer. It is non-blocking, and
	// the informers stop when ctx is cancelled -- which is how our SIGTERM handling
	// in main reaches all the way down to these watches.
	s.factory.Start(ctx.Done())

	// Bound the wait. Without a timeout, an unreachable API server or RBAC that
	// forbids `list` on one resource would hang startup forever, and the pod would sit
	// in Running-but-not-Ready with nothing explaining why.
	syncCtx, cancel := context.WithTimeout(ctx, s.cacheSyncTimeout)
	defer cancel()

	started := time.Now()
	// WaitForCacheSync returns a map of reflector type -> synced bool. Any false entry
	// means that informer never completed its initial List.
	syncResults := s.factory.WaitForCacheSync(syncCtx.Done())

	// DISTINGUISH "ASKED TO STOP" FROM "COULD NOT SYNC" BEFORE TRUSTING THE RESULT.
	//
	// WaitForCacheSync returns all-false whenever its stop channel closes, and it cannot
	// tell you WHY. Cancellation of the parent context looks exactly like an API server
	// that never answered.
	//
	// An earlier version of this treated both as a failure, so pressing Ctrl-C -- or the
	// kubelet sending SIGTERM during a rollout or a node drain -- while caches were
	// still warming made run() return an error and the process exit 1. Kubernetes records
	// that as a crash rather than a normal termination, and it counts towards
	// CrashLoopBackOff. A shutdown request is not a failure.
	//
	// ctx.Err() (the PARENT, not syncCtx) is the discriminator: it is non-nil only when
	// the caller cancelled, whereas a genuine timeout shows up on syncCtx alone.
	if err := ctx.Err(); err != nil {
		s.log.Info("informer sync aborted by shutdown request",
			"waited_ms", time.Since(started).Milliseconds())
		// Deliberately NOT marking synced: readiness must stay honest on the way down.
		return nil
	}

	for typ, ok := range syncResults {
		if !ok {
			return fmt.Errorf("informer cache for %s did not sync within %s "+
				"(check RBAC: list and watch are required)", typ, s.cacheSyncTimeout)
		}
	}

	s.synced.Store(true)
	s.log.Info("informer caches synced",
		"duration_ms", time.Since(started).Milliseconds(),
		"nodes", s.countNodes(),
		"namespaces", s.countNamespaces(),
		"pods", s.countPods(),
	)
	return nil
}

// Name implements health.Checker.
func (s *Store) Name() string { return "kubernetes" }

// Check implements health.Checker.
//
// It reports on the CACHE, not on the API server, and that is the right thing to
// measure. We can serve inventory perfectly well from a warm cache during a brief API
// server blip -- the data goes slightly stale, which for cost is irrelevant. Pinging
// the API server here would take us out of service for an outage we are actually
// tolerating well.
//
// What genuinely makes us unable to serve is an unsynced cache, because then we would
// answer with an empty cluster.
func (s *Store) Check(_ context.Context) error {
	if !s.synced.Load() {
		return ErrCachesNotSynced
	}
	// A cluster with zero nodes is impossible while our watch is healthy, so this
	// almost certainly means the cache was emptied or never really populated. Better
	// to fail readiness than to report a cluster that costs nothing.
	if s.countNodes() == 0 {
		return fmt.Errorf("informer cache reports zero nodes, which cannot be correct")
	}
	return nil
}

// Nodes returns every node, sorted by name.
//
// Sorting is not cosmetic: it makes the API response DETERMINISTIC. The informer cache
// is a map, and Go randomises map iteration order deliberately, so without this the
// same request would return the same nodes in a different order every time. That
// breaks client-side diffing, makes response caching useless, and turns any
// golden-file test into a flake.
func (s *Store) Nodes() ([]Node, error) {
	list, err := s.nodes.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("listing nodes from cache: %w", err)
	}

	out := make([]Node, 0, len(list))
	for _, n := range list {
		out = append(out, toNode(n))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Namespaces returns every namespace, sorted by name.
func (s *Store) Namespaces() ([]Namespace, error) {
	list, err := s.namespaces.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("listing namespaces from cache: %w", err)
	}

	out := make([]Namespace, 0, len(list))
	for _, ns := range list {
		out = append(out, toNamespace(ns))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Pods returns pods, optionally filtered to one namespace, sorted by namespace then
// name.
//
// An empty namespace means all namespaces. Using the namespace-scoped lister when one
// is given is not just tidier -- PodLister.Pods(ns).List() reads a namespace INDEX
// maintained by the informer, so it does not scan every pod in the cluster.
func (s *Store) Pods(namespace string) ([]Pod, error) {
	var (
		list []*corev1.Pod
		err  error
	)
	if namespace == "" {
		list, err = s.pods.List(labels.Everything())
	} else {
		list, err = s.pods.Pods(namespace).List(labels.Everything())
	}
	if err != nil {
		return nil, fmt.Errorf("listing pods from cache: %w", err)
	}

	out := make([]Pod, 0, len(list))
	for _, p := range list {
		out = append(out, toPod(p, s.resolveOwner))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// resolveOwner is the ownerResolver that convert.go depends on, backed by the
// ReplicaSet cache.
//
// It is a METHOD VALUE passed as a function value (s.resolveOwner above), which is how
// the pure translation functions get access to a cache without importing anything
// about it. convert.go stays testable with a map-backed closure; production gets the
// real lookup. This is dependency injection with no framework and no interface -- just
// a function type.
func (s *Store) resolveOwner(namespace, kind, name string) *metav1.OwnerReference {
	if kind != "ReplicaSet" {
		// Jobs would need a Job informer to walk Job -> CronJob. Deliberately not
		// watched: our only Job fixture is garbage-collected seconds after it
		// completes, and an extra cluster-wide watch is not worth it until Phase 6
		// actually reports on CronJob cost. resolveWorkload falls back to the
		// immediate owner, so a Job-owned pod still attributes to its Job.
		return nil
	}

	rs, err := s.replicaSets.ReplicaSets(namespace).Get(name)
	if err != nil {
		// Not found is expected and benign: a ReplicaSet scaled to zero is garbage
		// collected while its last pod may still be terminating. Logged at debug
		// because it is normal churn, not a problem.
		s.log.Debug("could not resolve replicaset owner",
			"namespace", namespace, "replicaset", name, "error", err)
		return nil
	}
	return metav1.GetControllerOf(rs)
}

func (s *Store) countNodes() int {
	list, err := s.nodes.List(labels.Everything())
	if err != nil {
		return 0
	}
	return len(list)
}

func (s *Store) countNamespaces() int {
	list, err := s.namespaces.List(labels.Everything())
	if err != nil {
		return 0
	}
	return len(list)
}

func (s *Store) countPods() int {
	list, err := s.pods.List(labels.Everything())
	if err != nil {
		return 0
	}
	return len(list)
}
