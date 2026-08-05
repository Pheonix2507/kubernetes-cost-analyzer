// Package costing joins topology, observed usage and rates into cost.
//
// WHY THIS PACKAGE EXISTS
// -----------------------
// Three packages each hold one third of the answer and none of them can produce a number:
//
//	internal/kube     what exists and what it RESERVED
//	internal/prom     what it actually USED
//	internal/pricing  what a unit of either COSTS
//
// This is the only place that knows all three, and it is deliberately the only place. Keeping
// the join here means each of those packages stays independently testable, and it means the
// billing rule -- max(request, usage) -- lives in exactly one function rather than being
// reimplemented in the collector, the API and a report.
//
// EVERYTHING HERE IS BEST-EFFORT, WHICH IS A DESIGN DECISION AND NOT LAZINESS
// -------------------------------------------------------------------------
// A collection cycle touches a Kubernetes cache, a Prometheus instance and a pricing
// catalogue, across dozens of namespaces. Something will fail. The choice is between
// abandoning the cycle and recording what did succeed, and for cost data partial coverage is
// far more useful than a gap: forty namespaces priced and two missing beats nothing at all,
// so long as the two are REPORTED rather than silently absent.
//
// That single decision shapes the concurrency below, and it is why errgroup is used for
// bounding and waiting but deliberately NOT for error propagation.
package costing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/pricing"
)

// Window is the half-open interval [Start, End) a collection covers.
//
// Half-open for the same reason the fact table is: with closed intervals consecutive windows
// share an endpoint, and every range query double-counts the boundary sample.
type Window struct {
	Start time.Time
	End   time.Time
}

// Duration returns the window length.
func (w Window) Duration() time.Duration { return w.End.Sub(w.Start) }

// Valid reports whether the window is usable.
func (w Window) Valid() bool {
	return !w.Start.IsZero() && !w.End.IsZero() && w.End.After(w.Start)
}

// String renders the window for logs.
func (w Window) String() string {
	return fmt.Sprintf("[%s, %s)", w.Start.Format(time.RFC3339), w.End.Format(time.RFC3339))
}

// AlignedWindow returns the most recent COMPLETE window of length interval, ending at or
// before now.
//
// TWO THINGS THIS GETS RIGHT THAT THE OBVIOUS VERSION DOES NOT
// -----------------------------------------------------------
// ALIGNMENT. The naive window is [now-interval, now). Start the collector at 09:02:37 and it
// records [08:57:37, 09:02:37); restart it at 09:04:10 and it records [08:59:10, 09:04:10).
// Those overlap, they share no key, and the primary key on (window_start, pod, container)
// therefore cannot deduplicate them -- so the overlapping minutes are counted TWICE and the
// bill inflates on every restart. Truncating to the interval means every process, on every
// machine, at any moment, agrees on where the boundaries are, which is what makes the upsert
// actually idempotent.
//
// COMPLETENESS. It returns the PREVIOUS window, not the current one. The current interval has
// not finished, so pricing it would charge a partial window at full duration. Prometheus also
// scrapes on its own schedule, so the last few seconds of even a just-closed window may not be
// ingested -- which is why the caller should allow a little lag beyond the boundary before
// collecting.
func AlignedWindow(now time.Time, interval time.Duration) Window {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	// UTC before truncating. time.Truncate operates on the absolute instant, but doing this in
	// UTC makes the boundaries reproducible regardless of the process's local timezone -- two
	// collectors in different timezones must agree on the same window key.
	end := now.UTC().Truncate(interval)
	return Window{Start: end.Add(-interval), End: end}
}

// Inventory is the topology this engine needs. Satisfied by *kube.Store.
//
// Declared here, in the CONSUMER, and deliberately narrower than what *kube.Store offers: the
// engine has no business calling Start or Check.
type Inventory interface {
	Nodes() ([]domain.Node, error)
	Namespaces() ([]domain.Namespace, error)
	Pods(namespace string) ([]domain.Pod, error)
}

// UsageSource provides observed consumption. Satisfied by *prom.Client.
type UsageSource interface {
	ContainerUsage(ctx context.Context, namespace string, start, end time.Time) (map[domain.ContainerKey]domain.Usage, error)
}

// Engine produces cost allocations for a window.
type Engine struct {
	clusterName string
	inventory   Inventory
	usage       UsageSource
	pricer      pricing.Provider
	workers     int
	log         *slog.Logger
}

// Options configures an Engine.
type Options struct {
	ClusterName string
	Inventory   Inventory
	Usage       UsageSource
	Pricer      pricing.Provider
	// Workers bounds how many namespaces are processed concurrently.
	Workers int
	Log     *slog.Logger
}

// NewEngine builds an Engine, validating that every dependency is present.
//
// Checked at construction rather than at first use: a nil dependency discovered during a
// collection cycle is a panic in a background goroutine, which takes the process down. The
// same mistake caught here is a clear error at startup.
func NewEngine(opts Options) (*Engine, error) {
	var errs []error
	if opts.ClusterName == "" {
		errs = append(errs, errors.New("cluster name is required: it is denormalised onto every fact row"))
	}
	if opts.Inventory == nil {
		errs = append(errs, errors.New("inventory is required"))
	}
	if opts.Usage == nil {
		errs = append(errs, errors.New("usage source is required"))
	}
	if opts.Pricer == nil {
		errs = append(errs, errors.New("pricer is required"))
	}
	if opts.Log == nil {
		errs = append(errs, errors.New("logger is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("building cost engine: %w", err)
	}

	workers := opts.Workers
	if workers <= 0 {
		workers = 4
	}

	return &Engine{
		clusterName: opts.ClusterName,
		inventory:   opts.Inventory,
		usage:       opts.Usage,
		pricer:      opts.Pricer,
		workers:     workers,
		log:         opts.Log,
	}, nil
}

// Result is the outcome of one collection cycle.
//
// It carries FAILURES alongside the data on purpose. A function returning only
// ([]Allocation, error) forces a caller to choose between "everything worked" and "nothing
// did", and neither describes a cycle where 38 of 40 namespaces succeeded. Partial success has
// to be representable, or the caller cannot distinguish an empty cluster from a broken
// Prometheus.
type Result struct {
	Window      Window
	Allocations []domain.ContainerAllocation

	// NamespaceErrors records namespaces that could not be collected. Non-fatal individually,
	// and a Prometheus outage shows up as every namespace appearing here.
	NamespaceErrors map[string]error

	// Counters that explain the shape of the result, so an unexpectedly small Allocations
	// slice has an explanation rather than requiring an investigation.
	NodesPriced        int
	NodesUnpriced      int
	PodsUnscheduled    int
	PodsOnUnpricedNode int
	ContainersSkipped  int
}

// Summary renders the result as log attributes.
func (r Result) Summary() []any {
	return []any{
		"window", r.Window.String(),
		"allocations", len(r.Allocations),
		"namespace_errors", len(r.NamespaceErrors),
		"nodes_priced", r.NodesPriced,
		"nodes_unpriced", r.NodesUnpriced,
		"pods_unscheduled", r.PodsUnscheduled,
		"pods_on_unpriced_node", r.PodsOnUnpricedNode,
		"containers_skipped", r.ContainersSkipped,
	}
}

// Collect produces allocations for the given window.
//
// The error return is reserved for failures that make the WHOLE cycle meaningless -- an
// invalid window, or an inventory that cannot be read at all. Per-namespace problems land in
// Result.NamespaceErrors and do not prevent the rest from being recorded.
func (e *Engine) Collect(ctx context.Context, w Window) (Result, error) {
	if !w.Valid() {
		return Result{}, fmt.Errorf("invalid window %s: end must be after start and neither may be zero", w)
	}

	result := Result{Window: w, NamespaceErrors: map[string]error{}}

	// -----------------------------------------------------------------------------
	// 1. Price every node ONCE, up front.
	//
	// A node hosts many pods, and pricing is a map lookup plus a little decimal arithmetic --
	// but with a cloud pricing provider (Phase 11) it becomes a network call. Doing it per
	// pod would multiply that by the pod count, so the shape that stays correct under a
	// slower provider is to resolve rates per node before the fan-out.
	//
	// Deliberately SERIAL and before the goroutines start: building this map concurrently
	// would need a mutex for no benefit, since there are tens of nodes rather than thousands.
	// -----------------------------------------------------------------------------
	nodes, err := e.inventory.Nodes()
	if err != nil {
		return Result{}, fmt.Errorf("listing nodes: %w", err)
	}

	// Keyed by node NAME and carrying the node's own attributes alongside its rates. The
	// fact table denormalises zone and capacity type so a report can answer "what is spot
	// saving us" and "what are we paying for cross-zone placement" without a join -- and
	// without those values being rewritten if the node is later relabelled.
	nodeRates := make(map[string]pricedNode, len(nodes))
	for _, n := range nodes {
		rates, rateErr := e.pricer.RatesFor(ctx, n)
		if rateErr != nil {
			// The node is left OUT of the map, so its pods are skipped and counted rather
			// than priced at zero. Zero would report those pods as free and, because the
			// missing money still has to go somewhere in a percentage breakdown, would inflate
			// every other team's apparent share.
			result.NodesUnpriced++
			e.log.Warn("skipping pods on an unpriceable node",
				"node", n.Name, "instance_type", n.InstanceType, "error", rateErr)
			continue
		}
		nodeRates[n.Name] = pricedNode{node: n, rates: rates}
		result.NodesPriced++
	}

	// -----------------------------------------------------------------------------
	// 2. Namespace attribution, also resolved once.
	// -----------------------------------------------------------------------------
	namespaces, err := e.inventory.Namespaces()
	if err != nil {
		return Result{}, fmt.Errorf("listing namespaces: %w", err)
	}

	// -----------------------------------------------------------------------------
	// 3. Fan out over namespaces.
	//
	// WHY SHARD BY NAMESPACE AT ALL, RATHER THAN ONE CLUSTER-WIDE QUERY
	//
	//   FAULT ISOLATION, which is the strongest reason. One namespace with pathological
	//   cardinality can make its query time out; sharded, that costs one namespace and the
	//   other 39 are still recorded. Unsharded, the entire cycle is lost.
	//
	//   MEMORY. A cluster-wide query on a large cluster returns tens of thousands of series in
	//   a single response, which is a spike in both Prometheus and this process.
	//
	// The cost is more round trips, which is exactly what the concurrency below recovers.
	// -----------------------------------------------------------------------------
	var (
		mu          sync.Mutex
		allocations []domain.ContainerAllocation
	)

	// errgroup.WithContext + SetLimit IS the bounded worker pool.
	//
	// The classic hand-rolled version is a buffered channel used as a semaphore plus a
	// WaitGroup plus a separate error channel -- perhaps thirty lines, and the usual bugs are
	// a WaitGroup.Add inside the goroutine instead of outside, and a deadlock when nobody
	// drains the error channel. SetLimit does the same thing correctly in one line.
	//
	// THE IMPORTANT SUBTLETY: errgroup CANCELS ITS CONTEXT as soon as any goroutine returns a
	// non-nil error, and Wait returns that first error. That is exactly right for all-or-
	// nothing work, and exactly WRONG here -- one namespace failing would cancel the other
	// 39 mid-flight and lose their data.
	//
	// So the closures below ALWAYS return nil, and per-namespace errors are recorded under the
	// mutex instead. errgroup is being used purely as a bounded pool with a barrier, which is
	// a legitimate and common use, but it has to be a conscious choice: returning err from the
	// closure here would look more idiomatic and would silently destroy partial results.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(e.workers)

	for _, ns := range namespaces {
		// Go 1.22+ gives a fresh loop variable per iteration, so capturing ns directly is
		// safe. Before 1.22 every goroutine would have shared one variable and processed the
		// LAST namespace N times.
		g.Go(func() error {
			nsAllocations, stats, nsErr := e.collectNamespace(gctx, w, ns, nodeRates)

			// A MUTEX IS GENUINELY REQUIRED HERE, and it is worth contrasting with
			// internal/health, where the same problem needed none.
			//
			// There, each goroutine wrote results[i] for its own distinct i -- separate memory
			// locations, and the slice header was never modified, so no lock was needed. Here
			// we APPEND to a shared slice, which mutates the header's length and can reallocate
			// the backing array. That IS a data race, and `go test -race` catches it
			// immediately.
			//
			// Same codebase, two different correct answers, and the difference is whether the
			// shared value is being mutated or merely indexed.
			mu.Lock()
			defer mu.Unlock()

			if nsErr != nil {
				result.NamespaceErrors[ns.Name] = nsErr
				return nil // NOT propagated -- see the note above
			}
			allocations = append(allocations, nsAllocations...)
			result.PodsUnscheduled += stats.podsUnscheduled
			result.PodsOnUnpricedNode += stats.podsOnUnpricedNode
			result.ContainersSkipped += stats.containersSkipped
			return nil
		})
	}

	// Cannot return a real error, since every closure returns nil. Checked anyway, because
	// ignoring an error return is exactly the habit that hides the next one.
	if err := g.Wait(); err != nil {
		return Result{}, fmt.Errorf("collecting namespaces: %w", err)
	}

	// If the parent context was cancelled, the namespace errors are all just "context
	// cancelled" and the partial data is not worth writing. Reported as a real failure so the
	// caller does not persist a misleadingly small cycle.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Result{}, fmt.Errorf("collection cancelled: %w", ctxErr)
	}

	result.Allocations = allocations
	return result, nil
}

// pricedNode pairs a node with its resolved rates.
//
// Both are needed at allocation time: the rates to compute cost, and the node's own attributes
// to denormalise onto the fact row.
type pricedNode struct {
	node  domain.Node
	rates pricing.Rates
}

// namespaceStats counts what was skipped while processing one namespace.
type namespaceStats struct {
	podsUnscheduled    int
	podsOnUnpricedNode int
	containersSkipped  int
}

// collectNamespace produces allocations for one namespace.
func (e *Engine) collectNamespace(
	ctx context.Context, w Window, ns domain.Namespace, nodeRates map[string]pricedNode,
) ([]domain.ContainerAllocation, namespaceStats, error) {
	var stats namespaceStats

	// Pods come from the informer cache -- no API call, no I/O.
	pods, err := e.inventory.Pods(ns.Name)
	if err != nil {
		return nil, stats, fmt.Errorf("listing pods: %w", err)
	}
	if len(pods) == 0 {
		// An empty namespace is not an error, and skipping the Prometheus query for it saves a
		// round trip. On a cluster with many empty namespaces that is most of the work avoided.
		return nil, stats, nil
	}

	// The expensive step: one Prometheus round trip for this namespace.
	usage, err := e.usage.ContainerUsage(ctx, ns.Name, w.Start, w.End)
	if err != nil {
		return nil, stats, fmt.Errorf("querying usage: %w", err)
	}

	out := make([]domain.ContainerAllocation, 0, len(pods))
	duration := w.Duration()

	for _, pod := range pods {
		// An unscheduled pod reserves nothing and occupies no node, so it costs nothing. Not
		// an error, and counted so a large number of them is visible -- it usually means the
		// cluster cannot schedule, which is worth noticing.
		if pod.NodeName == "" {
			stats.podsUnscheduled++
			continue
		}

		pn, priced := nodeRates[pod.NodeName]
		if !priced {
			stats.podsOnUnpricedNode++
			continue
		}

		for _, c := range pod.Containers {
			// Init containers are excluded here: they ran once and exited, so charging them
			// for the full window would overstate the pod permanently. See
			// domain.ContainerKind.
			if !c.Kind.Billable() {
				stats.containersSkipped++
				continue
			}

			observed := usage[domain.ContainerKey{
				Namespace: pod.Namespace, Pod: pod.Name, Container: c.Name,
			}]

			out = append(out, e.allocation(w, duration, ns, pod, c, observed, pn))
		}
	}

	return out, stats, nil
}

// allocation builds one fact row.
//
// This is where max(request, usage) is applied and where the attribution snapshot is taken.
// Every string copied onto the row is copied DELIBERATELY: a fact is an immutable historical
// statement, so relabelling a namespace tomorrow must not rewrite what this window said today.
func (e *Engine) allocation(
	w Window, duration time.Duration,
	ns domain.Namespace, pod domain.Pod, c domain.Container,
	observed domain.Usage, pn pricedNode,
) domain.ContainerAllocation {
	rates := pn.rates
	alloc := domain.ContainerAllocation{
		WindowStart:   w.Start,
		WindowEnd:     w.End,
		ContainerName: c.Name,

		ClusterName:   e.clusterName,
		NamespaceName: pod.Namespace,
		PodName:       pod.Name,
		WorkloadKind:  pod.Workload.Kind,
		WorkloadName:  pod.Workload.Name,
		NodeName:      pod.NodeName,
		Team:          ns.Team,
		CostCentre:    ns.CostCentre,
		Environment:   ns.Environment,
		// The NODE's instance-type label, not the catalogue key that matched. They differ
		// for a fallback: the node genuinely IS an m7i.metal-48xl even though nothing in the
		// catalogue priced it, and a report grouping by instance type should say so. The
		// caveat lives in RateSource, which is the honest place for it.
		InstanceType: pn.node.InstanceType,
		CapacityType: pn.node.CapacityType,
		Zone:         pn.node.Zone,
		QoSClass:     pod.QoSClass,
		RateSource:   string(rates.Source),

		CPUMillicoresRequested: c.RequestsCPUMillicores,
		MemoryBytesRequested:   c.RequestsMemoryBytes,
		CPUMillicoresUsed:      observed.CPUMillicores,
		MemoryBytesUsed:        observed.MemoryBytes,

		CPUCostPerCoreHour:   rates.CPUPerCoreHour,
		MemoryCostPerGiBHour: rates.MemoryPerGiBHour,
	}

	// THE BILLING RULE, applied exactly once in the codebase.
	//
	// Billable() is a method on the allocation rather than a calculation here, so the value
	// stored in the database and the value used to compute cost cannot diverge -- and the
	// CHECK constraint on the table independently verifies the stored figure really is the max.
	cpuBillable, memBillable := alloc.Billable()

	cost := pricing.Cost(rates, cpuBillable, memBillable, duration)
	alloc.CPUCost = cost.CPU
	alloc.MemoryCost = cost.Memory

	return alloc
}
