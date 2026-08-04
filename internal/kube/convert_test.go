package kube

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These tests need no cluster, no fake clientset and no Docker, because everything in
// convert.go is a pure function. That is the entire return on keeping the translation
// layer free of I/O: the subtle logic -- effective pod requests, ownership chains, unit
// conversion -- is the part most likely to be wrong, and it is also the part that is
// hardest to exercise against a real API server. Reproducing a sidecar-plus-init-container
// pod on a live cluster is a chore; here it is a struct literal.

// qty is a helper for readable resource quantities in test fixtures.
func qty(s string) resource.Quantity {
	return resource.MustParse(s)
}

// -----------------------------------------------------------------------------
// Nodes
// -----------------------------------------------------------------------------

func TestToNode(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "kca-dev-worker2",
			CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{
				labelInstanceType: "m5.xlarge",
				labelRegion:       "ap-south-1",
				labelZone:         "ap-south-1b",
				labelCapacityType: "spot",
			},
		},
		Spec: corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{
			// Capacity deliberately differs from Allocatable, which is the case that
			// matters: they are NOT interchangeable, and billing the wrong one
			// understates cost by whatever the kubelet reserved.
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    qty("4"),
				corev1.ResourceMemory: qty("16Gi"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    qty("3900m"),
				corev1.ResourceMemory: qty("15Gi"),
			},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.36.1"},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}

	got := toNode(n)

	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"Name", got.Name, "kca-dev-worker2"},
		{"InstanceType", got.InstanceType, "m5.xlarge"},
		{"Region", got.Region, "ap-south-1"},
		{"Zone", got.Zone, "ap-south-1b"},
		{"CapacityType", got.CapacityType, "spot"},
		// "4" cores -> 4000 millicores. Exact integer arithmetic, no float rounding.
		{"CapacityCPUMillicores", got.CapacityCPUMillicores, int64(4000)},
		{"CapacityMemoryBytes", got.CapacityMemoryBytes, int64(16 * 1024 * 1024 * 1024)},
		{"AllocatableCPUMillicores", got.AllocatableCPUMillicores, int64(3900)},
		{"AllocatableMemoryBytes", got.AllocatableMemoryBytes, int64(15 * 1024 * 1024 * 1024)},
		{"Ready", got.Ready, true},
		{"Unschedulable", got.Unschedulable, true},
		{"KubeletVersion", got.KubeletVersion, "v1.36.1"},
		{"CreatedAt", got.CreatedAt, created},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
}

// TestToNode_MissingLabelsAndConditions covers the bare-metal / unlabelled case. A node
// with no cloud metadata must translate to empty strings and zeros, never panic --
// otherwise our service dies on any cluster that is not the one we developed against.
func TestToNode_MissingLabelsAndConditions(t *testing.T) {
	t.Parallel()

	got := toNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "bare"}})

	if got.InstanceType != "" || got.Zone != "" || got.CapacityType != "" {
		t.Errorf("expected empty cloud metadata, got %+v", got)
	}
	if got.CapacityCPUMillicores != 0 || got.CapacityMemoryBytes != 0 {
		t.Errorf("expected zero capacity for a node reporting none, got %+v", got)
	}
	// No Ready condition at all means unknown, which we must treat as not ready.
	if got.Ready {
		t.Error("Ready = true for a node with no Ready condition; want false")
	}
}

// -----------------------------------------------------------------------------
// Namespaces
// -----------------------------------------------------------------------------

func TestToNamespace_LiftsCostDimensions(t *testing.T) {
	t.Parallel()

	got := toNamespace(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "team-payments",
			Labels: map[string]string{
				labelTeam:       "payments",
				labelCostCentre: "cc-1001",
				"environment":   "production",
				"extra":         "kept",
			},
		},
		Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	})

	if got.Team != "payments" || got.CostCentre != "cc-1001" || got.Environment != "production" {
		t.Errorf("cost dimensions not lifted from labels: %+v", got)
	}
	// The full label map is retained so a user can group by a dimension we did not
	// anticipate without us shipping a new field.
	if got.Labels["extra"] != "kept" {
		t.Error("original labels were not retained")
	}
	if got.Phase != "Active" {
		t.Errorf("Phase = %q, want %q", got.Phase, "Active")
	}
}

// -----------------------------------------------------------------------------
// Pod resources -- the subtle part
// -----------------------------------------------------------------------------

// TestPodResources is the most valuable test in this package. Each case is a real
// misprising that the naive sum(spec.Containers) implementation would produce.
func TestPodResources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		spec        corev1.PodSpec
		wantCPUmc   int64
		wantMemByte int64
		why         string
	}{
		{
			name: "single container",
			spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    qty("500m"),
					corev1.ResourceMemory: qty("512Mi"),
				}},
			}}},
			wantCPUmc:   500,
			wantMemByte: 512 * 1024 * 1024,
			why:         "baseline",
		},
		{
			name: "multiple containers are summed",
			spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: qty("500m"), corev1.ResourceMemory: qty("256Mi")}}},
				{Name: "helper", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: qty("250m"), corev1.ResourceMemory: qty("128Mi")}}},
			}},
			wantCPUmc:   750,
			wantMemByte: 384 * 1024 * 1024,
			why:         "regular containers run concurrently, so they add up",
		},
		{
			name: "init container is a MAX, not a sum",
			spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{
					Name: "migrate",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: qty("2"), corev1.ResourceMemory: qty("2Gi")}},
				}},
				Containers: []corev1.Container{{
					Name: "app",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: qty("500m"), corev1.ResourceMemory: qty("512Mi")}},
				}},
			},
			// max(app=2000m? no: max(sum(regular)=500m, init=2000m)) = 2000m
			wantCPUmc:   2000,
			wantMemByte: 2 * 1024 * 1024 * 1024,
			why: "the init container exits before the app starts, so the pod needs the " +
				"LARGER of the two, not 2500m. A naive sum would over-bill by 500m " +
				"forever, for a container that ran for ten seconds.",
		},
		{
			name: "sidecar (restartable init) IS additive",
			spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{
					Name:          "istio-proxy",
					RestartPolicy: ptr(corev1.ContainerRestartPolicyAlways), // = sidecar
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: qty("100m"), corev1.ResourceMemory: qty("128Mi")}},
				}},
				Containers: []corev1.Container{{
					Name: "app",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: qty("500m"), corev1.ResourceMemory: qty("512Mi")}},
				}},
			},
			wantCPUmc:   600,
			wantMemByte: 640 * 1024 * 1024,
			why: "a sidecar runs for the pod's whole life, so it DOES add. Treating it " +
				"like a normal init container undercounts every pod in a service mesh.",
		},
		{
			name: "pod overhead is included",
			spec: corev1.PodSpec{
				Overhead: corev1.ResourceList{
					corev1.ResourceCPU: qty("50m"), corev1.ResourceMemory: qty("64Mi")},
				Containers: []corev1.Container{{
					Name: "app",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: qty("500m"), corev1.ResourceMemory: qty("512Mi")}},
				}},
			},
			wantCPUmc:   550,
			wantMemByte: 576 * 1024 * 1024,
			why:         "the sandbox itself is reserved by the scheduler (Kata, gVisor)",
		},
		{
			name:        "no requests at all",
			spec:        corev1.PodSpec{Containers: []corev1.Container{{Name: "freeloader"}}},
			wantCPUmc:   0,
			wantMemByte: 0,
			why: "the BestEffort case. Zero is CORRECT here -- and it is exactly why " +
				"cost must be billed on max(request, usage), not on request alone.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pod := &corev1.Pod{Spec: tt.spec}
			requests, _ := podResources(pod)

			cpu := requests[corev1.ResourceCPU]
			mem := requests[corev1.ResourceMemory]

			if got := cpu.MilliValue(); got != tt.wantCPUmc {
				t.Errorf("cpu = %dm, want %dm\nwhy this matters: %s", got, tt.wantCPUmc, tt.why)
			}
			if got := mem.Value(); got != tt.wantMemByte {
				t.Errorf("memory = %d bytes, want %d\nwhy this matters: %s", got, tt.wantMemByte, tt.why)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Ownership resolution
// -----------------------------------------------------------------------------

func TestResolveWorkload(t *testing.T) {
	t.Parallel()

	// A resolver backed by a plain map. No informer, no cluster -- this is what the
	// injected ownerResolver function type buys us.
	resolver := func(namespace, kind, name string) *metav1.OwnerReference {
		if namespace == "team-payments" && kind == "ReplicaSet" && name == "api-55758c88bb" {
			return &metav1.OwnerReference{Kind: "Deployment", Name: "api"}
		}
		return nil
	}

	tests := []struct {
		name  string
		owner *metav1.OwnerReference
		want  Workload
		why   string
	}{
		{
			name:  "deployment-managed pod resolves past its ReplicaSet",
			owner: &metav1.OwnerReference{Kind: "ReplicaSet", Name: "api-55758c88bb", Controller: ptr(true)},
			want:  Workload{Kind: "Deployment", Name: "api"},
			why: "the ReplicaSet hash changes on EVERY rollout. Reporting cost against " +
				"it makes each deploy look like a new service, so no workload keeps history.",
		},
		{
			name:  "StatefulSet is already the workload",
			owner: &metav1.OwnerReference{Kind: "StatefulSet", Name: "postgres", Controller: ptr(true)},
			want:  Workload{Kind: "StatefulSet", Name: "postgres"},
			why:   "one hop only; no ReplicaSet in between",
		},
		{
			name:  "DaemonSet is already the workload",
			owner: &metav1.OwnerReference{Kind: "DaemonSet", Name: "node-exporter", Controller: ptr(true)},
			want:  Workload{Kind: "DaemonSet", Name: "node-exporter"},
			why:   "one hop only",
		},
		{
			name:  "unresolvable ReplicaSet degrades to the ReplicaSet itself",
			owner: &metav1.OwnerReference{Kind: "ReplicaSet", Name: "already-gc-ed", Controller: ptr(true)},
			want:  Workload{Kind: "ReplicaSet", Name: "already-gc-ed"},
			why: "a ReplicaSet scaled to zero is garbage collected while its last pod " +
				"terminates. Degraded attribution beats none at all.",
		},
		{
			name:  "bare pod has no workload",
			owner: nil,
			want:  Workload{},
			why: "legitimate but unusual: nothing will recreate it, so it is often a " +
				"leftover from manual debugging that has been billing quietly for months.",
		},
		{
			name: "non-controller owner references are ignored",
			// Controller: false. A pod may carry several owner references but at most
			// one CONTROLLER; the others exist for garbage collection and do not
			// represent the managing workload.
			owner: &metav1.OwnerReference{Kind: "Deployment", Name: "not-the-controller", Controller: ptr(false)},
			want:  Workload{},
			why:   "GetControllerOf must only match the reference with controller: true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-payments"}}
			if tt.owner != nil {
				pod.OwnerReferences = []metav1.OwnerReference{*tt.owner}
			}

			got := resolveWorkload(pod, resolver)
			if got != tt.want {
				t.Errorf("resolveWorkload() = %+v, want %+v\nwhy this matters: %s", got, tt.want, tt.why)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Cache hygiene
// -----------------------------------------------------------------------------

// TestStripForCache verifies the informer TransformFunc drops the bulky fields. On a
// large cluster this is the difference between an OOMKill and a comfortable memory
// limit, and it is far easier to verify here than to diagnose in production.
func TestStripForCache(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "p",
		ManagedFields: []metav1.ManagedFieldsEntry{
			{Manager: "kube-controller-manager"}, {Manager: "kubectl"},
		},
		Annotations: map[string]string{
			corev1.LastAppliedConfigAnnotation: `{"a":"whole copy of the manifest"}`,
			"keep-me":                          "yes",
		},
	}}

	out, err := stripForCache(pod)
	if err != nil {
		t.Fatalf("stripForCache returned an error: %v", err)
	}
	got, ok := out.(*corev1.Pod)
	if !ok {
		t.Fatalf("stripForCache changed the type to %T", out)
	}

	if len(got.ManagedFields) != 0 {
		t.Errorf("ManagedFields still present: %+v", got.ManagedFields)
	}
	if _, found := got.Annotations[corev1.LastAppliedConfigAnnotation]; found {
		t.Error("last-applied-configuration annotation was not stripped")
	}
	// Stripping must be surgical: unrelated annotations carry real information, and
	// some of them will be cost-allocation dimensions.
	if got.Annotations["keep-me"] != "yes" {
		t.Error("stripForCache removed an unrelated annotation")
	}
}

// TestStripForCache_NonObjectPassesThrough guards against poisoning the cache. A
// TransformFunc that errors on an unexpected value would break the whole informer, and
// cache tombstones are not metav1.Object.
func TestStripForCache_NonObjectPassesThrough(t *testing.T) {
	t.Parallel()

	out, err := stripForCache("not an object")
	if err != nil {
		t.Errorf("stripForCache errored on a non-object: %v", err)
	}
	if out != "not an object" {
		t.Errorf("value was altered: %v", out)
	}
}

// ptr returns a pointer to v. Kubernetes API types use pointers for optional fields to
// distinguish "unset" from "set to the zero value" -- for RestartPolicy, unset means a
// normal init container while Always means a sidecar, and those bill differently.
func ptr[T any](v T) *T { return &v }
