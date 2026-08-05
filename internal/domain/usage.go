package domain

import "strings"

// ContainerKey identifies a container in the label space Prometheus uses.
//
// KEYED BY POD NAME, NOT POD UID, AND THAT IS A REAL LIMITATION
// ------------------------------------------------------------
// cAdvisor labels its series with namespace, pod and container. There is no pod UID. So
// joining observed usage to our informer-derived topology must happen on the pod's NAME, even
// though UID is the true identity everywhere else in this codebase.
//
// The risk is name reuse: delete a StatefulSet pod and its replacement carries the identical
// name. Within a single collection window that is vanishingly unlikely, and the worst
// consequence would be attributing a few seconds of one pod's usage to its successor. Worth
// knowing rather than discovering, and it is why the join is performed per-window rather than
// across time.
type ContainerKey struct {
	Namespace string
	Pod       string
	Container string
}

// String renders the key as namespace/pod/container for log messages.
func (k ContainerKey) String() string {
	return strings.Join([]string{k.Namespace, k.Pod, k.Container}, "/")
}

// Usage is what a container actually consumed over a window.
type Usage struct {
	// CPUMillicores is the AVERAGE over the window, not a peak.
	//
	// Average is the correct statistic for cost, which is an integral over time: a container
	// that spends one second at a full core and 299 idle costs the same as one steady at ~3
	// millicores. Right-sizing wants a different statistic entirely -- Phase 6 uses p95, so a
	// recommendation does not throttle the workload at its peak -- and conflating the two is
	// how a cost tool ends up giving advice that causes an incident.
	CPUMillicores int64

	// MemoryBytes is the average working set over the window.
	//
	// WORKING SET, not RSS and not container_memory_usage_bytes. usage_bytes includes the page
	// cache, which the kernel reclaims under pressure, so a container that read a large file
	// once looks permanently enormous. Working set is what the kubelet uses for eviction
	// decisions, which makes it the honest answer to "how much does this actually need".
	MemoryBytes int64

	// CPUMillicoresMax and MemoryBytesMax are the PEAK within the window.
	//
	// Captured at collection time because the resolution only exists then. A percentile computed
	// later over per-window AVERAGES has already had the peaks smoothed out of it, so it
	// systematically understates the true peak -- by more, the burstier the workload. Recommending
	// a request from an understated peak is how a right-sizing tool causes throttling and OOM
	// kills.
	//
	// Cost uses the average; right-sizing uses these. The statistic has to match the question.
	CPUMillicoresMax int64
	MemoryBytesMax   int64
}
