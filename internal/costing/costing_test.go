package costing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/pricing"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// -----------------------------------------------------------------------------
// Test doubles
// -----------------------------------------------------------------------------

type stubInventory struct {
	nodes      []domain.Node
	namespaces []domain.Namespace
	pods       map[string][]domain.Pod
	nodesErr   error
	nsErr      error
	podsErr    error
}

func (s *stubInventory) Nodes() ([]domain.Node, error)           { return s.nodes, s.nodesErr }
func (s *stubInventory) Namespaces() ([]domain.Namespace, error) { return s.namespaces, s.nsErr }
func (s *stubInventory) Pods(ns string) ([]domain.Pod, error) {
	if s.podsErr != nil {
		return nil, s.podsErr
	}
	return s.pods[ns], nil
}

type stubUsage struct {
	byNamespace map[string]map[domain.ContainerKey]domain.Usage
	// failFor makes one namespace's query fail, to exercise partial success.
	failFor string
	// delay simulates query latency, so concurrency is observable.
	delay time.Duration

	mu       sync.Mutex
	inFlight int
	maxSeen  int
	calls    int
}

func (s *stubUsage) ContainerUsage(ctx context.Context, ns string, _, _ time.Time) (map[domain.ContainerKey]domain.Usage, error) {
	s.mu.Lock()
	s.calls++
	s.inFlight++
	if s.inFlight > s.maxSeen {
		s.maxSeen = s.inFlight
	}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()

	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if ns == s.failFor {
		return nil, errors.New("prometheus query timed out")
	}
	return s.byNamespace[ns], nil
}

func (s *stubUsage) peakConcurrency() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxSeen
}

func (s *stubUsage) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type stubPricer struct {
	rates    pricing.Rates
	failFor  string // node name that cannot be priced
	callsFor sync.Map
}

func (s *stubPricer) RatesFor(_ context.Context, n domain.Node) (pricing.Rates, error) {
	c, _ := s.callsFor.LoadOrStore(n.Name, new(atomic.Int64))
	counter, ok := c.(*atomic.Int64)
	if ok {
		counter.Add(1)
	}
	if n.Name == s.failFor {
		return pricing.Rates{}, fmt.Errorf("%w: %s", pricing.ErrNoPrice, n.Name)
	}
	return s.rates, nil
}

func (s *stubPricer) calls(node string) int64 {
	if c, ok := s.callsFor.Load(node); ok {
		if counter, ok := c.(*atomic.Int64); ok {
			return counter.Load()
		}
	}
	return 0
}

func testRates() pricing.Rates {
	return pricing.Rates{
		Currency:         "USD",
		NodeHourly:       decimal.RequireFromString("0.1060"),
		CPUPerCoreHour:   decimal.RequireFromString("0.0371"),
		MemoryPerGiBHour: decimal.RequireFromString("0.003975"),
		Source:           pricing.SourceCatalogue,
		InstanceType:     "m5.large",
	}
}

func testWindow() Window {
	start := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	return Window{Start: start, End: start.Add(time.Hour)}
}

func newTestEngine(t *testing.T, inv Inventory, usage UsageSource, pricer pricing.Provider, workers int) *Engine {
	t.Helper()
	e, err := NewEngine(Options{
		ClusterName: "kca-dev", Inventory: inv, Usage: usage,
		Pricer: pricer, Workers: workers, Log: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

// =============================================================================
// Window alignment -- the property that makes the upsert idempotent
// =============================================================================

// TestAlignedWindow is the test that prevents double-billing across restarts.
//
// The naive window is [now-interval, now). Start at 09:02:37 and you record
// [08:57:37, 09:02:37); restart at 09:04:10 and you record [08:59:10, 09:04:10). Those overlap,
// they share no primary key, so the fact table CANNOT deduplicate them -- the overlapping
// minutes are billed twice and the total grows with every restart.
func TestAlignedWindow(t *testing.T) {
	t.Parallel()

	interval := 5 * time.Minute

	tests := []struct {
		name      string
		now       string
		wantStart string
		wantEnd   string
	}{
		{"mid-interval", "2026-08-05T09:02:37Z", "2026-08-05T08:55:00Z", "2026-08-05T09:00:00Z"},
		{"a few seconds later", "2026-08-05T09:04:59Z", "2026-08-05T08:55:00Z", "2026-08-05T09:00:00Z"},
		{"exactly on a boundary", "2026-08-05T09:05:00Z", "2026-08-05T09:00:00Z", "2026-08-05T09:05:00Z"},
		{"across the hour", "2026-08-05T10:00:01Z", "2026-08-05T09:55:00Z", "2026-08-05T10:00:00Z"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now, err := time.Parse(time.RFC3339, tt.now)
			if err != nil {
				t.Fatalf("bad test time: %v", err)
			}
			w := AlignedWindow(now, interval)
			if got := w.Start.Format(time.RFC3339); got != tt.wantStart {
				t.Errorf("Start = %s, want %s", got, tt.wantStart)
			}
			if got := w.End.Format(time.RFC3339); got != tt.wantEnd {
				t.Errorf("End = %s, want %s", got, tt.wantEnd)
			}
			// The window must be COMPLETE: it must never extend past now.
			if w.End.After(now) {
				t.Errorf("End %s is in the future relative to %s; a partial window would be "+
					"charged at full duration", w.End, now)
			}
		})
	}
}

// TestAlignedWindow_IsStableAcrossRestarts asserts the property directly: any two moments
// within the same interval must produce the SAME window.
func TestAlignedWindow_IsStableAcrossRestarts(t *testing.T) {
	t.Parallel()

	interval := 5 * time.Minute
	base := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	want := AlignedWindow(base.Add(time.Second), interval)

	for offset := time.Duration(0); offset < interval; offset += time.Second {
		got := AlignedWindow(base.Add(offset), interval)
		if !got.Start.Equal(want.Start) || !got.End.Equal(want.End) {
			t.Fatalf("at +%s the window is %s, want %s -- restarts would produce overlapping, "+
				"non-deduplicable windows", offset, got, want)
		}
	}
}

// TestAlignedWindow_IsTimezoneIndependent covers two collectors in different local timezones.
// They must agree on window keys, or the same wall-clock period is stored twice.
func TestAlignedWindow_IsTimezoneIndependent(t *testing.T) {
	t.Parallel()

	instant := time.Date(2026, 8, 5, 9, 2, 37, 0, time.UTC)
	kolkata := time.FixedZone("IST", 5*3600+1800)
	newYork := time.FixedZone("EDT", -4*3600)

	a := AlignedWindow(instant.In(kolkata), 5*time.Minute)
	b := AlignedWindow(instant.In(newYork), 5*time.Minute)

	if !a.Start.Equal(b.Start) || !a.End.Equal(b.End) {
		t.Errorf("windows differ by timezone: %s vs %s", a, b)
	}
}

// =============================================================================
// The join
// =============================================================================

func TestCollect_ProducesAllocations(t *testing.T) {
	t.Parallel()

	w := testWindow()
	inv := &stubInventory{
		nodes: []domain.Node{{
			Name: "w1", InstanceType: "m5.large", Zone: "ap-south-1a", CapacityType: "spot",
		}},
		namespaces: []domain.Namespace{{
			Name: "team-payments", Team: "payments", CostCentre: "cc-1001", Environment: "production",
		}},
		pods: map[string][]domain.Pod{
			"team-payments": {{
				UID: "u1", Name: "api-abc", Namespace: "team-payments", NodeName: "w1",
				QoSClass: "Burstable",
				Workload: domain.Workload{Kind: "Deployment", Name: "api"},
				Containers: []domain.Container{{
					Name: "app", Kind: domain.ContainerKindRegular,
					RequestsCPUMillicores: 500, RequestsMemoryBytes: 512 << 20,
				}},
			}},
		},
	}
	usage := &stubUsage{byNamespace: map[string]map[domain.ContainerKey]domain.Usage{
		"team-payments": {
			{Namespace: "team-payments", Pod: "api-abc", Container: "app"}: {
				CPUMillicores: 2, MemoryBytes: 5 << 20,
			},
		},
	}}

	result, err := newTestEngine(t, inv, usage, &stubPricer{rates: testRates()}, 4).
		Collect(context.Background(), w)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Allocations) != 1 {
		t.Fatalf("got %d allocations, want 1", len(result.Allocations))
	}
	a := result.Allocations[0]

	// Attribution must be a complete snapshot, so relabelling the namespace tomorrow cannot
	// rewrite what this window said.
	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"ClusterName", a.ClusterName, "kca-dev"},
		{"NamespaceName", a.NamespaceName, "team-payments"},
		{"PodName", a.PodName, "api-abc"},
		{"ContainerName", a.ContainerName, "app"},
		{"WorkloadKind", a.WorkloadKind, "Deployment"},
		{"WorkloadName", a.WorkloadName, "api"},
		{"NodeName", a.NodeName, "w1"},
		{"Team", a.Team, "payments"},
		{"CostCentre", a.CostCentre, "cc-1001"},
		{"Environment", a.Environment, "production"},
		{"QoSClass", a.QoSClass, "Burstable"},
		// From the NODE, not the catalogue key: a report grouping by instance type or by
		// spot-vs-on-demand needs the node's own truth.
		{"InstanceType", a.InstanceType, "m5.large"},
		{"CapacityType", a.CapacityType, "spot"},
		{"Zone", a.Zone, "ap-south-1a"},
		{"RateSource", a.RateSource, string(pricing.SourceCatalogue)},
		{"WindowStart", a.WindowStart, w.Start},
		{"WindowEnd", a.WindowEnd, w.End},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}

	// max(request=500, used=2) = 500. The over-provisioned case: you pay for the reservation.
	cpuBillable, memBillable := a.Billable()
	if cpuBillable != 500 || memBillable != 512<<20 {
		t.Errorf("billable = (%d, %d), want (500, %d)", cpuBillable, memBillable, 512<<20)
	}

	// 0.5 core * 1h * 0.0371 = 0.01855
	if !a.CPUCost.Equal(decimal.RequireFromString("0.01855")) {
		t.Errorf("CPUCost = %s, want 0.01855", a.CPUCost)
	}
	// 0.5 GiB * 1h * 0.003975 = 0.0019875
	if !a.MemoryCost.Equal(decimal.RequireFromString("0.0019875")) {
		t.Errorf("MemoryCost = %s, want 0.0019875", a.MemoryCost)
	}
}

// TestCollect_BillsMaxOfRequestAndUsage covers the rule the product rests on, at the engine
// level, including the BestEffort case a request-only formula prices at zero.
func TestCollect_BillsMaxOfRequestAndUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		reqCPU     int64
		usedCPU    int64
		wantBilled int64
		why        string
	}{
		{
			name: "over-provisioned", reqCPU: 500, usedCPU: 2, wantBilled: 500,
			why: "the scheduler reserved 500m whether or not it was touched",
		},
		{
			name: "under-requested", reqCPU: 50, usedCPU: 400, wantBilled: 400,
			why: "it really consumed this, so it must be charged for it",
		},
		{
			name: "BestEffort, no request at all", reqCPU: 0, usedCPU: 300, wantBilled: 300,
			why: "THE critical case. Billing on request alone prices this at zero and smears " +
				"its real cost across every other team",
		},
		{
			name: "idle but reserved", reqCPU: 200, usedCPU: 0, wantBilled: 200,
			why: "reserving capacity costs money even at zero usage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inv := &stubInventory{
				nodes:      []domain.Node{{Name: "w1", InstanceType: "m5.large"}},
				namespaces: []domain.Namespace{{Name: "ns"}},
				pods: map[string][]domain.Pod{"ns": {{
					UID: "u", Name: "p", Namespace: "ns", NodeName: "w1",
					Containers: []domain.Container{{
						Name: "c", Kind: domain.ContainerKindRegular,
						RequestsCPUMillicores: tt.reqCPU,
					}},
				}}},
			}
			usage := &stubUsage{byNamespace: map[string]map[domain.ContainerKey]domain.Usage{
				"ns": {{Namespace: "ns", Pod: "p", Container: "c"}: {CPUMillicores: tt.usedCPU}},
			}}

			result, err := newTestEngine(t, inv, usage, &stubPricer{rates: testRates()}, 2).
				Collect(context.Background(), testWindow())
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if len(result.Allocations) != 1 {
				t.Fatalf("got %d allocations, want 1", len(result.Allocations))
			}
			gotCPU, _ := result.Allocations[0].Billable()
			if gotCPU != tt.wantBilled {
				t.Errorf("billable cpu = %dm, want %dm\nwhy: %s", gotCPU, tt.wantBilled, tt.why)
			}
		})
	}
}

// TestCollect_ExcludesInitContainers covers the container-kind rule. Charging a migration
// container that ran for ten seconds as though it held its reservation for the whole window
// would overstate that pod permanently.
func TestCollect_ExcludesInitContainers(t *testing.T) {
	t.Parallel()

	inv := &stubInventory{
		nodes:      []domain.Node{{Name: "w1", InstanceType: "m5.large"}},
		namespaces: []domain.Namespace{{Name: "ns"}},
		pods: map[string][]domain.Pod{"ns": {{
			UID: "u", Name: "p", Namespace: "ns", NodeName: "w1",
			Containers: []domain.Container{
				{Name: "app", Kind: domain.ContainerKindRegular, RequestsCPUMillicores: 500},
				{Name: "istio-proxy", Kind: domain.ContainerKindSidecar, RequestsCPUMillicores: 100},
				{Name: "migrate", Kind: domain.ContainerKindInit, RequestsCPUMillicores: 2000},
			},
		}}},
	}

	result, err := newTestEngine(t, inv, &stubUsage{}, &stubPricer{rates: testRates()}, 2).
		Collect(context.Background(), testWindow())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(result.Allocations) != 2 {
		t.Fatalf("got %d allocations, want 2 (the init container must be excluded)", len(result.Allocations))
	}
	if result.ContainersSkipped != 1 {
		t.Errorf("ContainersSkipped = %d, want 1", result.ContainersSkipped)
	}

	names := map[string]bool{}
	for _, a := range result.Allocations {
		names[a.ContainerName] = true
	}
	if !names["app"] || !names["istio-proxy"] {
		t.Errorf("expected app and istio-proxy to be billed, got %v", names)
	}
	if names["migrate"] {
		t.Error("the init container was billed; a 2-core migration that ran for ten seconds " +
			"would be charged for the whole window forever")
	}
}

// =============================================================================
// Best-effort behaviour: the design decision that shapes the concurrency
// =============================================================================

// TestCollect_OneNamespaceFailureDoesNotLoseTheRest is the most important test in this file.
//
// errgroup CANCELS its context as soon as any goroutine returns a non-nil error. That is right
// for all-or-nothing work and catastrophically wrong here: one namespace's Prometheus timeout
// would cancel the other 39 mid-flight and discard their data.
//
// Returning err from the closure would look more idiomatic and would silently destroy partial
// results. This test is what stops someone "tidying" it that way.
func TestCollect_OneNamespaceFailureDoesNotLoseTheRest(t *testing.T) {
	t.Parallel()

	nsNames := []string{"ns-a", "ns-b", "ns-broken", "ns-c", "ns-d"}
	inv := &stubInventory{
		nodes: []domain.Node{{Name: "w1", InstanceType: "m5.large"}},
		pods:  map[string][]domain.Pod{},
	}
	for _, n := range nsNames {
		inv.namespaces = append(inv.namespaces, domain.Namespace{Name: n})
		inv.pods[n] = []domain.Pod{{
			UID: "u-" + n, Name: "p-" + n, Namespace: n, NodeName: "w1",
			Containers: []domain.Container{{
				Name: "c", Kind: domain.ContainerKindRegular, RequestsCPUMillicores: 100,
			}},
		}}
	}

	usage := &stubUsage{
		byNamespace: map[string]map[domain.ContainerKey]domain.Usage{},
		failFor:     "ns-broken",
	}

	result, err := newTestEngine(t, inv, usage, &stubPricer{rates: testRates()}, 3).
		Collect(context.Background(), testWindow())

	// NOT a top-level error: the cycle largely succeeded.
	if err != nil {
		t.Fatalf("Collect returned a fatal error for one namespace failure: %v", err)
	}
	// The four healthy namespaces must ALL be present.
	if len(result.Allocations) != 4 {
		t.Errorf("got %d allocations, want 4 (one namespace failed; the rest must survive)",
			len(result.Allocations))
	}
	// And the failure must be REPORTED, not swallowed. Silent partial data is worse than an
	// error, because nobody knows the number is incomplete.
	if len(result.NamespaceErrors) != 1 {
		t.Fatalf("NamespaceErrors = %v, want exactly one entry", result.NamespaceErrors)
	}
	if _, found := result.NamespaceErrors["ns-broken"]; !found {
		t.Errorf("NamespaceErrors does not name ns-broken: %v", result.NamespaceErrors)
	}
	for _, a := range result.Allocations {
		if a.NamespaceName == "ns-broken" {
			t.Error("the failed namespace produced allocations")
		}
	}
}

// TestCollect_UnpriceableNodeSkipsItsPods covers the alternative to pricing at zero. Zero would
// report those pods as free and inflate every other team's apparent share.
func TestCollect_UnpriceableNodeSkipsItsPods(t *testing.T) {
	t.Parallel()

	inv := &stubInventory{
		nodes: []domain.Node{
			{Name: "priced", InstanceType: "m5.large"},
			{Name: "exotic", InstanceType: "m7i.metal-48xl"},
		},
		namespaces: []domain.Namespace{{Name: "ns"}},
		pods: map[string][]domain.Pod{"ns": {
			{UID: "a", Name: "on-priced", Namespace: "ns", NodeName: "priced",
				Containers: []domain.Container{{Name: "c", Kind: domain.ContainerKindRegular, RequestsCPUMillicores: 100}}},
			{UID: "b", Name: "on-exotic", Namespace: "ns", NodeName: "exotic",
				Containers: []domain.Container{{Name: "c", Kind: domain.ContainerKindRegular, RequestsCPUMillicores: 100}}},
		}},
	}

	result, err := newTestEngine(t, inv, &stubUsage{},
		&stubPricer{rates: testRates(), failFor: "exotic"}, 2).
		Collect(context.Background(), testWindow())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if result.NodesPriced != 1 || result.NodesUnpriced != 1 {
		t.Errorf("NodesPriced=%d NodesUnpriced=%d, want 1 and 1", result.NodesPriced, result.NodesUnpriced)
	}
	if len(result.Allocations) != 1 {
		t.Fatalf("got %d allocations, want 1", len(result.Allocations))
	}
	if result.Allocations[0].PodName != "on-priced" {
		t.Errorf("wrong pod billed: %s", result.Allocations[0].PodName)
	}
	// Counted, so an unexpectedly small result has an explanation rather than requiring an
	// investigation.
	if result.PodsOnUnpricedNode != 1 {
		t.Errorf("PodsOnUnpricedNode = %d, want 1", result.PodsOnUnpricedNode)
	}
}

// TestCollect_UnscheduledPodCostsNothing covers the Pending pod. It occupies no node and
// reserves nothing, so zero is correct here -- unlike the unpriced-node case.
func TestCollect_UnscheduledPodCostsNothing(t *testing.T) {
	t.Parallel()

	inv := &stubInventory{
		nodes:      []domain.Node{{Name: "w1", InstanceType: "m5.large"}},
		namespaces: []domain.Namespace{{Name: "ns"}},
		pods: map[string][]domain.Pod{"ns": {{
			UID: "u", Name: "pending", Namespace: "ns", NodeName: "", // not scheduled
			Containers: []domain.Container{{Name: "c", Kind: domain.ContainerKindRegular, RequestsCPUMillicores: 500}},
		}}},
	}

	result, err := newTestEngine(t, inv, &stubUsage{}, &stubPricer{rates: testRates()}, 2).
		Collect(context.Background(), testWindow())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Allocations) != 0 {
		t.Errorf("got %d allocations for an unscheduled pod, want 0", len(result.Allocations))
	}
	if result.PodsUnscheduled != 1 {
		t.Errorf("PodsUnscheduled = %d, want 1", result.PodsUnscheduled)
	}
}

// =============================================================================
// Concurrency
// =============================================================================

// TestCollect_RespectsWorkerLimit proves SetLimit actually bounds concurrency. Without it,
// 500 namespaces would open 500 simultaneous Prometheus queries and take the monitoring stack
// down -- a cost tool causing an outage in the system it monitors.
func TestCollect_RespectsWorkerLimit(t *testing.T) {
	t.Parallel()

	const namespaces = 20
	const workers = 3

	inv := &stubInventory{
		nodes: []domain.Node{{Name: "w1", InstanceType: "m5.large"}},
		pods:  map[string][]domain.Pod{},
	}
	for i := 0; i < namespaces; i++ {
		name := fmt.Sprintf("ns-%02d", i)
		inv.namespaces = append(inv.namespaces, domain.Namespace{Name: name})
		inv.pods[name] = []domain.Pod{{
			UID: "u", Name: "p", Namespace: name, NodeName: "w1",
			Containers: []domain.Container{{Name: "c", Kind: domain.ContainerKindRegular, RequestsCPUMillicores: 10}},
		}}
	}

	// A delay makes overlap observable; without it goroutines might complete before the next
	// starts and the peak would read as 1 regardless of the limit.
	usage := &stubUsage{
		byNamespace: map[string]map[domain.ContainerKey]domain.Usage{},
		delay:       30 * time.Millisecond,
	}

	result, err := newTestEngine(t, inv, usage, &stubPricer{rates: testRates()}, workers).
		Collect(context.Background(), testWindow())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	peak := usage.peakConcurrency()
	if peak > workers {
		t.Errorf("peak concurrency was %d with a limit of %d; SetLimit is not being applied",
			peak, workers)
	}
	// And it must genuinely run in PARALLEL, or the limit is meaningless because everything is
	// serial anyway.
	if peak < 2 {
		t.Errorf("peak concurrency was %d; the namespaces are being processed serially", peak)
	}
	if len(result.Allocations) != namespaces {
		t.Errorf("got %d allocations, want %d", len(result.Allocations), namespaces)
	}
}

// TestCollect_PricesEachNodeOnce proves rates are resolved per node rather than per pod. With
// a cloud pricing provider that is the difference between one API call and one per pod.
func TestCollect_PricesEachNodeOnce(t *testing.T) {
	t.Parallel()

	inv := &stubInventory{
		nodes:      []domain.Node{{Name: "w1", InstanceType: "m5.large"}},
		namespaces: []domain.Namespace{{Name: "ns"}},
		pods:       map[string][]domain.Pod{"ns": {}},
	}
	for i := 0; i < 20; i++ {
		inv.pods["ns"] = append(inv.pods["ns"], domain.Pod{
			UID: fmt.Sprintf("u%d", i), Name: fmt.Sprintf("p%d", i), Namespace: "ns", NodeName: "w1",
			Containers: []domain.Container{{Name: "c", Kind: domain.ContainerKindRegular, RequestsCPUMillicores: 10}},
		})
	}

	pricer := &stubPricer{rates: testRates()}
	if _, err := newTestEngine(t, inv, &stubUsage{}, pricer, 4).
		Collect(context.Background(), testWindow()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if got := pricer.calls("w1"); got != 1 {
		t.Errorf("RatesFor called %d times for one node hosting 20 pods, want 1", got)
	}
}

// TestCollect_SkipsUsageQueryForEmptyNamespaces proves the round trip is avoided. On a cluster
// with many empty namespaces that is most of the work.
func TestCollect_SkipsUsageQueryForEmptyNamespaces(t *testing.T) {
	t.Parallel()

	inv := &stubInventory{
		nodes: []domain.Node{{Name: "w1", InstanceType: "m5.large"}},
		namespaces: []domain.Namespace{
			{Name: "empty-1"}, {Name: "empty-2"}, {Name: "busy"},
		},
		pods: map[string][]domain.Pod{"busy": {{
			UID: "u", Name: "p", Namespace: "busy", NodeName: "w1",
			Containers: []domain.Container{{Name: "c", Kind: domain.ContainerKindRegular}},
		}}},
	}
	usage := &stubUsage{byNamespace: map[string]map[domain.ContainerKey]domain.Usage{}}

	if _, err := newTestEngine(t, inv, usage, &stubPricer{rates: testRates()}, 4).
		Collect(context.Background(), testWindow()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if calls := usage.callCount(); calls != 1 {
		t.Errorf("Prometheus was queried %d times for 3 namespaces of which 2 are empty, want 1", calls)
	}
}

// =============================================================================
// Fatal conditions
// =============================================================================

func TestCollect_RejectsInvalidWindow(t *testing.T) {
	t.Parallel()

	e := newTestEngine(t, &stubInventory{}, &stubUsage{}, &stubPricer{rates: testRates()}, 2)
	start := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)

	for _, w := range []Window{
		{},                         // zero
		{Start: start},             // no end
		{Start: start, End: start}, // zero length
		{Start: start, End: start.Add(-time.Hour)}, // reversed
	} {
		if _, err := e.Collect(context.Background(), w); err == nil {
			t.Errorf("Collect accepted invalid window %s", w)
		}
	}
}

// TestCollect_InventoryFailureIsFatal distinguishes a whole-cycle failure from a partial one.
// Without nodes there is nothing to price against, so producing a partial result would be
// misleading rather than useful.
func TestCollect_InventoryFailureIsFatal(t *testing.T) {
	t.Parallel()

	e := newTestEngine(t, &stubInventory{nodesErr: errors.New("cache not synced")},
		&stubUsage{}, &stubPricer{rates: testRates()}, 2)

	if _, err := e.Collect(context.Background(), testWindow()); err == nil {
		t.Error("Collect succeeded despite being unable to list nodes")
	}
}

// TestCollect_CancelledContextIsFatal covers SIGTERM mid-cycle. The partial data is not worth
// writing, and reporting success would persist a misleadingly small cycle that looks like a
// genuine drop in spend.
func TestCollect_CancelledContextIsFatal(t *testing.T) {
	t.Parallel()

	inv := &stubInventory{
		nodes:      []domain.Node{{Name: "w1", InstanceType: "m5.large"}},
		namespaces: []domain.Namespace{{Name: "ns"}},
		pods: map[string][]domain.Pod{"ns": {{
			UID: "u", Name: "p", Namespace: "ns", NodeName: "w1",
			Containers: []domain.Container{{Name: "c", Kind: domain.ContainerKindRegular}},
		}}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newTestEngine(t, inv, &stubUsage{}, &stubPricer{rates: testRates()}, 2).
		Collect(ctx, testWindow())
	if err == nil {
		t.Error("Collect succeeded with a cancelled context; a truncated cycle would look " +
			"like a genuine drop in spend")
	}
}

func TestNewEngine_RequiresDependencies(t *testing.T) {
	t.Parallel()

	full := Options{
		ClusterName: "c", Inventory: &stubInventory{}, Usage: &stubUsage{},
		Pricer: &stubPricer{}, Log: testLogger(),
	}

	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{"no cluster name", func(o *Options) { o.ClusterName = "" }},
		{"no inventory", func(o *Options) { o.Inventory = nil }},
		{"no usage source", func(o *Options) { o.Usage = nil }},
		{"no pricer", func(o *Options) { o.Pricer = nil }},
		{"no logger", func(o *Options) { o.Log = nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := full
			tt.mutate(&opts)
			if _, err := NewEngine(opts); err == nil {
				t.Error("NewEngine accepted a missing dependency; it would panic in a " +
					"background goroutine and take the process down")
			}
		})
	}

	// The complete set must be accepted, or the test above proves nothing.
	if _, err := NewEngine(full); err != nil {
		t.Errorf("NewEngine rejected a complete set of options: %v", err)
	}
}
