package kube

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	resourcehelper "k8s.io/component-helpers/resource"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

// Everything in this file is a PURE FUNCTION: Kubernetes object in, our type out. No
// network, no cache, no clock.
//
// That is a deliberate design choice, not an accident of style. All the subtle logic
// in Phase 1 lives here -- effective pod requests, ownership resolution, unit
// conversion -- and pure functions mean every one of those subtleties can be tested
// with a hand-built struct literal and no cluster, no fake clientset, and no Docker.
// See convert_test.go: it runs in milliseconds and covers cases that would be
// laborious to reproduce against a real API server.

// Well-known label keys. These are the ones cloud providers set themselves, which is
// why deploy/kind/cluster.yaml fakes exactly these and not invented equivalents: the
// same code then works unchanged against EKS, GKE or AKS.
const (
	labelInstanceType = "node.kubernetes.io/instance-type"
	labelRegion       = "topology.kubernetes.io/region"
	labelZone         = "topology.kubernetes.io/zone"

	// labelCapacityType is OURS (kca.io/), because there is no cross-cloud standard.
	// AWS uses eks.amazonaws.com/capacityType, Karpenter uses karpenter.sh/capacity-type,
	// GKE uses cloud.google.com/gke-spot. Phase 3 will map those onto this one.
	labelCapacityType = "kca.io/capacity-type"

	// Cost allocation dimensions, read from namespace labels.
	labelTeam        = "team"
	labelCostCentre  = "cost-centre"
	labelEnvironment = "environment"
)

// toNode translates a Kubernetes Node.
func toNode(n *corev1.Node) domain.Node {
	out := domain.Node{
		Name:           n.Name,
		InstanceType:   n.Labels[labelInstanceType],
		Region:         n.Labels[labelRegion],
		Zone:           n.Labels[labelZone],
		CapacityType:   n.Labels[labelCapacityType],
		Unschedulable:  n.Spec.Unschedulable,
		KubeletVersion: n.Status.NodeInfo.KubeletVersion,
		CreatedAt:      n.CreationTimestamp.Time,
	}

	// Reading a missing key from a nil or absent map yields the zero Quantity, whose
	// MilliValue() is 0. So a node reporting no capacity reads as zero rather than
	// panicking -- correct, and it will show up as an obvious anomaly rather than a
	// crash at 3am.
	cpuCap := n.Status.Capacity[corev1.ResourceCPU]
	memCap := n.Status.Capacity[corev1.ResourceMemory]
	cpuAlloc := n.Status.Allocatable[corev1.ResourceCPU]
	memAlloc := n.Status.Allocatable[corev1.ResourceMemory]

	// MilliValue for CPU, Value for memory. A CPU quantity of "2" becomes 2000
	// millicores; memory "8Gi" becomes 8589934592 bytes.
	//
	// MilliValue is taken on a POINTER because it has a pointer receiver and may
	// normalise the quantity's internal representation. The map index above copies the
	// value, so we are mutating our own copy, not the informer's cached object --
	// which matters enormously: the cache is SHARED, and mutating an object in it
	// would corrupt what every other reader sees.
	out.CapacityCPUMillicores = cpuCap.MilliValue()
	out.CapacityMemoryBytes = memCap.Value()
	out.AllocatableCPUMillicores = cpuAlloc.MilliValue()
	out.AllocatableMemoryBytes = memAlloc.Value()

	// Ready is a CONDITION, not a field. Kubernetes reports node health as a list of
	// conditions, so we search for the one we care about. Absence means "unknown",
	// which we treat as not ready.
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			out.Ready = cond.Status == corev1.ConditionTrue
			break
		}
	}

	return out
}

// toNamespace translates a Kubernetes Namespace.
func toNamespace(ns *corev1.Namespace) domain.Namespace {
	return domain.Namespace{
		Name:        ns.Name,
		Team:        ns.Labels[labelTeam],
		CostCentre:  ns.Labels[labelCostCentre],
		Environment: ns.Labels[labelEnvironment],
		// COPIED, not assigned. See copyLabels: assigning ns.Labels directly would hand
		// the caller a map that lives inside the shared informer cache.
		Labels:    copyLabels(ns.Labels),
		Phase:     string(ns.Status.Phase),
		CreatedAt: ns.CreationTimestamp.Time,
	}
}

// toPod translates a Kubernetes Pod.
//
// resolveOwner is injected rather than reached for, so this function stays pure and
// testable: the tests pass a closure over a map instead of standing up a ReplicaSet
// informer. It is also honest about the dependency -- resolving ownership needs a
// second lookup, and hiding that inside the function would obscure a real API call.
func toPod(p *corev1.Pod, resolveOwner ownerResolver) domain.Pod {
	requests, limits := podResources(p)

	out := domain.Pod{
		UID:            string(p.UID),
		Name:           p.Name,
		Namespace:      p.Namespace,
		NodeName:       p.Spec.NodeName,
		Phase:          string(p.Status.Phase),
		QoSClass:       string(p.Status.QOSClass),
		Workload:       resolveWorkload(p, resolveOwner),
		ContainerCount: len(p.Spec.Containers),
		Containers:     toContainers(p),
		Labels:         copyLabels(p.Labels),
		CreatedAt:      p.CreationTimestamp.Time,
	}

	cpuReq := requests[corev1.ResourceCPU]
	memReq := requests[corev1.ResourceMemory]
	cpuLim := limits[corev1.ResourceCPU]
	memLim := limits[corev1.ResourceMemory]

	out.RequestsCPUMillicores = cpuReq.MilliValue()
	out.RequestsMemoryBytes = memReq.Value()
	out.LimitsCPUMillicores = cpuLim.MilliValue()
	out.LimitsMemoryBytes = memLim.Value()

	// StartTime is nil while the pod is Pending. Copied into a fresh variable before
	// taking its address: &p.Status.StartTime.Time would hand out a pointer INTO the
	// shared informer cache, and any caller mutating it would corrupt the cache for
	// every other reader.
	if p.Status.StartTime != nil {
		started := p.Status.StartTime.Time
		out.StartedAt = &started
	}

	return out
}

// toContainers extracts per-container reservations, classifying each container by kind.
//
// WHY THE CLASSIFICATION MATTERS MORE THAN THE NUMBERS
// ---------------------------------------------------
// Kubernetes puts sidecars in the SAME LIST as init containers -- spec.initContainers -- and
// distinguishes them only by restartPolicy: Always. So the naive reading of that field is
// wrong in both directions:
//
//	treat all initContainers as init  -> every service-mesh proxy vanishes from the bill, and
//	                                     on a mesh-enabled cluster that is one per pod
//	treat all initContainers as running -> every migration container is charged as though it
//	                                     held its reservation forever, rather than for the ten
//	                                     seconds it actually ran
//
// Both are silent misattributions of the kind that make a cost report quietly wrong, so the
// distinction is made explicitly here and the billing rule lives on ContainerKind.Billable.
func toContainers(p *corev1.Pod) []domain.Container {
	if len(p.Spec.Containers) == 0 && len(p.Spec.InitContainers) == 0 {
		return nil
	}

	out := make([]domain.Container, 0, len(p.Spec.Containers)+len(p.Spec.InitContainers))

	for i := range p.Spec.Containers {
		out = append(out, toContainer(&p.Spec.Containers[i], domain.ContainerKindRegular))
	}
	for i := range p.Spec.InitContainers {
		c := &p.Spec.InitContainers[i]
		// RestartPolicy is a POINTER on a container, so "unset" is distinguishable from "set
		// to a zero value" -- and here unset means an ordinary init container while Always
		// means a sidecar. Those bill differently, which is exactly why the API uses a
		// pointer rather than an empty string.
		kind := domain.ContainerKindInit
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			kind = domain.ContainerKindSidecar
		}
		out = append(out, toContainer(c, kind))
	}

	return out
}

// toContainer reads one container's requests and limits.
func toContainer(c *corev1.Container, kind domain.ContainerKind) domain.Container {
	out := domain.Container{Name: c.Name, Kind: kind}

	// Indexing a nil ResourceList yields the zero Quantity, so a container declaring no
	// resources reads as zero rather than panicking. Zero is CORRECT here and is precisely the
	// BestEffort case that makes max(request, usage) necessary -- billing on requests alone
	// would price such a container at nothing while it consumes real CPU and memory.
	cpuReq := c.Resources.Requests[corev1.ResourceCPU]
	memReq := c.Resources.Requests[corev1.ResourceMemory]
	cpuLim := c.Resources.Limits[corev1.ResourceCPU]
	memLim := c.Resources.Limits[corev1.ResourceMemory]

	out.RequestsCPUMillicores = cpuReq.MilliValue()
	out.RequestsMemoryBytes = memReq.Value()
	out.LimitsCPUMillicores = cpuLim.MilliValue()
	out.LimitsMemoryBytes = memLim.Value()
	return out
}

// podResources computes what the pod actually reserved.
//
// WHY THIS IS NOT sum(container.Resources.Requests)
// -------------------------------------------------
// The obvious loop over spec.Containers is wrong, and wrong in a direction that
// misprices real workloads. Kubernetes' effective pod request is:
//
//	max( sum(regular containers) + sum(sidecar containers),
//	     max over init containers of (that init container + preceding sidecars) )
//	+ pod overhead
//
// Because:
//
//   - INIT CONTAINERS run sequentially, one at a time, BEFORE the app containers, and
//     each exits before the next starts. So they do not add up -- the pod needs enough
//     room for the largest one, not their sum. A migration init container requesting
//     2Gi does not add 2Gi to the pod's footprint for its whole life.
//   - SIDECAR CONTAINERS (init containers with restartPolicy: Always, stable since
//     1.29) DO run alongside the app containers for the pod's whole lifetime, so they
//     ARE additive. A service mesh proxy is the common case, and on a cluster with
//     Istio it is on every single pod -- ignoring it undercounts the entire cluster.
//   - POD OVERHEAD (spec.overhead, from a RuntimeClass) accounts for the sandbox
//     itself. Non-zero for Kata Containers or gVisor, and the scheduler reserves it.
//
// So we use k8s.io/component-helpers/resource, the same computation the scheduler
// uses. Its own documentation says "the computation is part of the API and must be
// reviewed as an API change" -- which is about as clear a signal as you get that
// reimplementing it is a mistake. A hand-rolled version would drift from the
// scheduler's on the next feature, and our numbers would quietly stop matching what
// the cluster actually reserved.
func podResources(p *corev1.Pod) (requests, limits corev1.ResourceList) {
	// The zero PodResourcesOptions gives the default behaviour: include pod overhead,
	// and honour pod-level resources when that feature is in use.
	opts := resourcehelper.PodResourcesOptions{}
	return resourcehelper.PodRequests(p, opts), resourcehelper.PodLimits(p, opts)
}

// ownerResolver looks up a controller's own controller, so a pod can be traced past
// its immediate owner to the workload a human recognises.
//
// It returns nil when the object is not found or has no controller, which the caller
// treats as "the chain ends here" rather than as an error.
type ownerResolver func(namespace, kind, name string) *metav1.OwnerReference

// resolveWorkload walks a pod's ownership chain to the controller a team would name.
//
// WHY THIS TAKES TWO HOPS
// -----------------------
// A pod's OwnerReferences point at its IMMEDIATE controller, and for the most common
// case in Kubernetes that is not the thing anyone cares about:
//
//	Deployment -> ReplicaSet -> Pod        (two hops; the pod's owner is a ReplicaSet)
//	StatefulSet -> Pod                     (one hop)
//	DaemonSet -> Pod                       (one hop)
//	CronJob -> Job -> Pod                  (two hops)
//
// A Deployment-managed pod is owned by a ReplicaSet named like
// "over-provisioned-api-55758c88bb" -- the hash suffix changes on EVERY rollout. Report
// cost against that and each deploy looks like an old service disappearing and a new
// one being born, so no workload has usable history and every trend graph resets.
//
// So for ReplicaSets and Jobs we take a second hop. That is what makes the
// over-replicated fixture legible: its waste is only visible once six pods aggregate
// to one Deployment.
func resolveWorkload(p *corev1.Pod, resolveOwner ownerResolver) domain.Workload {
	// GetControllerOf returns the owner reference with controller: true. A pod can
	// carry several owner references but at most one CONTROLLER, and only the
	// controller represents the managing workload -- the rest are for garbage
	// collection.
	ref := metav1.GetControllerOf(p)
	if ref == nil {
		// A bare pod, created directly with no controller. Legitimate but unusual, and
		// worth surfacing: nothing will recreate it, so it is often a leftover from
		// manual debugging that has been quietly billing for months.
		return domain.Workload{}
	}

	switch ref.Kind {
	case "ReplicaSet", "Job":
		// Take the second hop. If it fails -- because the ReplicaSet was already
		// garbage collected, or our RBAC does not cover it -- fall back to the
		// immediate owner rather than returning nothing. Degraded attribution beats
		// none.
		if resolveOwner != nil {
			if grandparent := resolveOwner(p.Namespace, ref.Kind, ref.Name); grandparent != nil {
				return domain.Workload{Kind: grandparent.Kind, Name: grandparent.Name}
			}
		}
		return domain.Workload{Kind: ref.Kind, Name: ref.Name}
	default:
		// StatefulSet, DaemonSet, or a custom controller: the immediate owner already
		// IS the workload.
		return domain.Workload{Kind: ref.Kind, Name: ref.Name}
	}
}

// copyLabels returns an independent copy of a label map.
//
// WHY THIS IS NOT PARANOIA
// -----------------------
// Listers return POINTERS INTO THE SHARED INFORMER CACHE, and a map field on such an
// object is a reference, so `Labels: p.Labels` hands the caller a map that lives inside
// the cache. Two things then go wrong:
//
//  1. Any caller that mutates the map it was given -- a handler adding a computed label,
//     a future aggregation step normalising keys -- silently corrupts the cache for every
//     other reader in the process.
//  2. It is a genuine DATA RACE. The informer writes to the cache from its own
//     goroutine while HTTP handlers read; nothing locks it, so `go test -race` would
//     eventually flag it, and on a weak memory model the reader can observe a torn map.
//
// The whole justification for translating Kubernetes objects into our own types was that
// the translation is a SAFETY boundary. Copying the scalar fields and then aliasing the
// maps defeated that while looking entirely harmless -- which is exactly why
// TestStore_DoesNotAliasCacheMaps exists.
//
// Returning nil for an empty input keeps `omitempty` working, so a pod with no labels
// serialises without the field rather than as an empty object.
func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// stripForCache removes fields we never read before an object enters the informer
// cache, and is wired in as the informer's TransformFunc.
//
// WHY THIS IS WORTH DOING
// -----------------------
// The cache holds every object of every watched type, in memory, for the process's
// lifetime. managedFields is server-side-apply bookkeeping recording which controller
// last touched which field, and on a heavily-reconciled object it is routinely a third
// of the object's size while being of no interest to us whatsoever. Last-applied
// annotations are similar: kubectl apply stores a complete copy of the previous
// manifest in an annotation, so a pod created that way carries a second serialised
// copy of itself.
//
// On a 10,000-pod cluster this is the difference between roughly 100MB and 60MB of
// resident memory. It is also the first thing to reach for when an informer-based
// service starts getting OOMKilled, and it is far easier to add now than to retrofit
// while debugging a memory limit.
//
// TransformFunc runs ONCE per object as it enters the cache, before any handler sees
// it, so the cost is paid at write time rather than on every read.
func stripForCache(obj any) (any, error) {
	if accessor, ok := obj.(metav1.Object); ok {
		accessor.SetManagedFields(nil)

		annotations := accessor.GetAnnotations()
		if _, found := annotations[corev1.LastAppliedConfigAnnotation]; found {
			// Copy before mutating: the annotations map may be shared, and deleting
			// from the original would be a write to memory another reader owns.
			trimmed := make(map[string]string, len(annotations)-1)
			for k, v := range annotations {
				if k != corev1.LastAppliedConfigAnnotation {
					trimmed[k] = v
				}
			}
			accessor.SetAnnotations(trimmed)
		}
	}
	// Objects that are not metav1.Object (tombstones, for instance) pass through
	// untouched. Returning an error here would poison the whole cache.
	return obj, nil
}
