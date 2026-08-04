// Package domain holds the vocabulary the whole application shares.
//
// WHY THIS PACKAGE EXISTS NOW AND NOT IN PHASE 1
// ---------------------------------------------
// These types started in internal/kube, which was correct while kube was their only
// producer AND only consumer. Inventing a domain package before that would have been
// architecture on speculation.
//
// Phase 2 changed the facts. There are now three packages that speak these concepts:
//
//	internal/kube            produces them from the Kubernetes API
//	internal/httpapi         serves them over HTTP
//	internal/store/postgres  persists them
//
// Leaving them in internal/kube would mean the PERSISTENCE layer imports a Kubernetes
// CLIENT package, which inverts the dependency direction: storing a row has nothing to do
// with client-go, and that import would drag informers and REST config into a package
// whose job is SQL.
//
// So the shared vocabulary moves to the middle, and everything points inward at it. This
// package imports nothing but the standard library, which is what makes that possible --
// it can never introduce a cycle.
//
// Extracted from three real implementations rather than designed up front. That ordering
// is the point: the shape of the abstraction was discovered, not guessed.
package domain

import "time"

// The types in this file are OUR representation of cluster objects, not Kubernetes'.
//
// WHY NOT JUST RETURN *corev1.Pod
// -------------------------------
// It would be less code, and it is a mistake, for three reasons:
//
//  1. A corev1.Pod serialises to several kilobytes of JSON, most of it irrelevant --
//     managedFields, resourceVersion, every default the API server filled in. Our API
//     consumers would have to know Kubernetes' schema to use our product.
//  2. It welds our public API to Kubernetes' API. A field deprecated upstream becomes
//     a breaking change for our clients, and we would have no way to shield them.
//  3. Translating FORCES US TO DECIDE what we actually need. That is where the real
//     thinking happens: which of capacity and allocatable to bill, how to express CPU
//     without floating point, what a pod's "owner" means. Passing the object through
//     defers all of those decisions to whoever consumes it, and they will each answer
//     differently.
//
// This is the anti-corruption layer from domain-driven design, and it is the single
// most valuable boundary in a system that integrates with someone else's API.
//
// A NOTE ON UNITS, WHICH IS NOT PEDANTRY
// --------------------------------------
// CPU is int64 MILLICORES and memory is int64 BYTES. Never float64.
//
// Kubernetes CPU quantities are exact in milli-units: "100m" is 100, and "0.1" is
// also 100. Represent that as float64 and you inherit binary floating point, where
// 0.1 + 0.2 != 0.3. Cost figures get summed across thousands of pods and then
// reported to finance; accumulated representation error in a number someone reconciles
// against an invoice is not a rounding curiosity, it is a credibility problem.
// Integers make the arithmetic exact.

// Node is a cluster node, with the cloud metadata that makes it priceable.
type Node struct {
	Name string `json:"name"`

	// InstanceType, Region, Zone and CapacityType come from the well-known node
	// labels a cloud provider sets (and which deploy/kind/cluster.yaml fakes locally).
	// InstanceType is the KEY the Phase 3 pricing engine looks up.
	InstanceType string `json:"instance_type"`
	Region       string `json:"region"`
	Zone         string `json:"zone"`
	// CapacityType is "on-demand" or "spot". Spot is typically 60-90% cheaper and can
	// be reclaimed at short notice, so it changes both the price and the advice.
	CapacityType string `json:"capacity_type"`

	// CAPACITY vs ALLOCATABLE -- the distinction that decides whether your cost
	// numbers are right.
	//
	// Capacity is what the machine physically has. Allocatable is capacity minus
	// kube-reserved, system-reserved and the eviction threshold: what the scheduler is
	// willing to hand out. Allocatable is typically 5-15% lower.
	//
	// YOU BILL CAPACITY. You rented the whole instance; the cloud provider charges for
	// all of it, including the slice the kubelet reserved for itself. But you compute
	// UTILISATION against allocatable, because that is the pool pods actually compete
	// for.
	//
	// Conflating them understates cost by whatever the reserve is, and that gap is
	// itself a finding: the difference between capacity and the sum of pod requests is
	// waste nobody is looking at.
	CapacityCPUMillicores    int64 `json:"capacity_cpu_millicores"`
	CapacityMemoryBytes      int64 `json:"capacity_memory_bytes"`
	AllocatableCPUMillicores int64 `json:"allocatable_cpu_millicores"`
	AllocatableMemoryBytes   int64 `json:"allocatable_memory_bytes"`

	// Ready reflects the node's Ready condition. A NotReady node still costs money,
	// which is exactly why it is worth surfacing.
	Ready bool `json:"ready"`
	// Unschedulable is true when the node is cordoned. Also still costs money.
	Unschedulable bool `json:"unschedulable"`

	KubeletVersion string    `json:"kubelet_version"`
	CreatedAt      time.Time `json:"created_at"`
}

// Namespace is a cluster namespace and its cost-allocation dimensions.
type Namespace struct {
	Name string `json:"name"`

	// Team, CostCentre and Environment are lifted out of labels into first-class
	// fields because they are the dimensions cost is reported BY. Leaving them in a
	// generic label map would push the "which label means the owner?" decision onto
	// every consumer, and they would not all choose the same one.
	Team        string `json:"team,omitempty"`
	CostCentre  string `json:"cost_centre,omitempty"`
	Environment string `json:"environment,omitempty"`

	// Labels is retained in full so a user can group by a dimension we did not
	// anticipate, without us shipping a new field for it.
	Labels    map[string]string `json:"labels,omitempty"`
	Phase     string            `json:"phase"`
	CreatedAt time.Time         `json:"created_at"`
}

// Workload identifies the controller that owns a pod.
//
// Cost is reported at this level, not per pod. Pods are cattle: a Deployment rolling
// out replaces every pod UID, and per-pod cost history would fragment on every deploy.
// The Deployment is the thing that persists and that a team recognises as "their
// service".
type Workload struct {
	// Kind is "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", or "" for a
	// bare pod with no controller.
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
}

// Pod is a running pod with its reserved resources and ownership resolved.
type Pod struct {
	// UID, not name, is the identity. Names are reused: delete a StatefulSet pod and
	// its replacement has the identical name but is a different pod with a different
	// lifetime. Keying cost history on name would silently merge the two.
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// NodeName is empty for a Pending pod that has not been scheduled. An unscheduled
	// pod reserves nothing and costs nothing.
	NodeName string `json:"node_name,omitempty"`

	Phase string `json:"phase"`

	// QoSClass is Guaranteed, Burstable or BestEffort. Read from pod.Status, never
	// recomputed -- the control plane already decided, and a second implementation
	// would eventually disagree with it.
	//
	// It matters for cost because it predicts EVICTION ORDER under node pressure:
	// BestEffort dies first, then Burstable exceeding its requests, then Guaranteed.
	// A BestEffort pod is also unbillable by request, since it declares none.
	QoSClass string `json:"qos_class"`

	// Workload is the resolved controller. See resolveWorkload for why this needs a
	// ReplicaSet lookup rather than just reading OwnerReferences.
	Workload Workload `json:"workload"`

	// Requests are what the scheduler RESERVED, and therefore what you are billed for.
	// Computed by the same code path the scheduler uses -- see podResources.
	RequestsCPUMillicores int64 `json:"requests_cpu_millicores"`
	RequestsMemoryBytes   int64 `json:"requests_memory_bytes"`

	// Limits are the enforcement ceiling. Not billed, but the difference between
	// limit and request is the burst headroom, and a CPU limit equal to the request
	// means guaranteed throttling.
	LimitsCPUMillicores int64 `json:"limits_cpu_millicores"`
	LimitsMemoryBytes   int64 `json:"limits_memory_bytes"`

	ContainerCount int               `json:"container_count"`
	Labels         map[string]string `json:"labels,omitempty"`

	// StartedAt is when the pod began running, and is nil while Pending. Cost is a
	// rate multiplied by DURATION, so this is what makes it a bill rather than a
	// snapshot.
	StartedAt *time.Time `json:"started_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}
