package recommend

import (
	"fmt"
	"math"

	"github.com/shopspring/decimal"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// hoursPerMonth converts an hourly rate to a monthly figure.
//
// 730 = 365 * 24 / 12, the average month. Not 720 (30 days), which understates by 1.4% and is the
// number most people reach for. Cloud invoices are monthly, so the recommendation should be
// comparable to one.
const hoursPerMonth = 730

const bytesPerGiB = 1 << 30

// =============================================================================
// setRequests: no reservation declared at all
// =============================================================================

// setRequests fires when a container declares no requests but consumes resources.
//
// WHY THIS IS FIRST AND WHY IT MATTERS MOST FOR CORRECTNESS
// -------------------------------------------------------
// A container with no requests is BestEffort: the first thing the kubelet evicts under node pressure,
// and invisible to any cost model based on requests. It is also unschedulable in any meaningful sense
// -- the scheduler reserves nothing for it, so it can land anywhere and be killed at any time.
//
// The recommendation INCREASES the visible bill, because the cost stops being smeared across
// everyone else and starts being attributed here. That is the honest direction: the money was always
// being spent.
func (e *Engine) setRequests(s postgres.ContainerStats) (Recommendation, bool) {
	// Both must be absent. A container with a CPU request but no memory request is a different
	// finding, and treating it as "no requests" would produce advice that ignores half its config.
	if s.CPURequestedMillicores > 0 || s.MemRequestedBytes > 0 {
		return Recommendation{}, false
	}
	// It must actually be doing something. A container that requests nothing AND uses nothing is a
	// pause container or a sidecar that never activates -- flagging it would be noise.
	if s.CPUP95Millicores == 0 && s.MemP95Bytes == 0 {
		return Recommendation{}, false
	}

	cpu := e.proposedCPU(s)
	mem := e.proposedMemory(s)

	r := e.base(s, KindSetRequests, SeverityWarning)
	r.Summary = fmt.Sprintf("%s declares no resource requests but is using CPU and memory", s.Container)
	r.Current = "no requests set (BestEffort)"
	r.Proposed = fmt.Sprintf("cpu: %dm, memory: %s", cpu, humanBytes(mem))
	r.Rationale = fmt.Sprintf(
		"Observed p95 of %dm CPU and %s memory over %.1f hours, with no requests declared. "+
			"This makes the container BestEffort -- the first thing the kubelet evicts under node "+
			"pressure -- and means its cost cannot be attributed to this workload, so it is silently "+
			"spread across every other team's share instead. "+
			"Setting requests will INCREASE this workload's reported cost, because the spend was "+
			"already happening and was simply invisible here. "+
			"A LimitRange with defaultRequest in this namespace prevents the whole class of problem.",
		s.CPUP95Millicores, humanBytes(s.MemP95Bytes), s.Duration().Hours())

	// Deliberately NOT presented as a saving. Attributing existing spend is not a cost increase, and
	// putting a negative number here would imply the change costs money it does not.
	r.EstimatedMonthlySaving = decimal.Zero
	return r, true
}

// =============================================================================
// idle: doing essentially nothing
// =============================================================================

// idle fires when a container has never meaningfully used its reservation.
//
// WHY THIS IS A SEPARATE RULE FROM rightSize, WHEN BOTH SEE "USAGE << REQUEST"
// ---------------------------------------------------------------------------
// Because the ACTIONS differ, and confusing them gives dangerous advice:
//
//	over-provisioned -> does real work, merely oversized. RESIZE it. Saving: the difference.
//	                    Risk of acting: low.
//	idle             -> does nothing at all. DELETE it. Saving: 100%.
//	                    Risk of acting: it might be a disaster-recovery standby, a quarterly batch
//	                    job, or a canary nobody remembers deploying.
//
// So this compares against the PEAK, not the average -- "never exceeded 5 millicores in 24 hours" is a
// far stronger claim than "averaged 5 millicores" -- and it demands a much longer observation window
// than any other rule, because deletion is the most destructive thing here.
func (e *Engine) idle(s postgres.ContainerStats) (Recommendation, bool) {
	t := e.thresholds

	// Nothing to reclaim if nothing was reserved. That case is setRequests, not this.
	if s.CPURequestedMillicores == 0 && s.MemRequestedBytes == 0 {
		return Recommendation{}, false
	}

	// The peak data must be trustworthy. Rows predating migration 000003 carry max = 0, which would
	// make every historical container look idle and produce a flood of confident deletion advice.
	if s.PeakCoverage < 0.9 {
		return Recommendation{}, false
	}

	// A LONGER floor than every other rule, because "delete this" deserves the most evidence.
	if s.Duration() < t.IdleMinObservation {
		return Recommendation{}, false
	}

	if s.CPUMaxMillicores > t.IdleCPUMillicores || s.MemMaxBytes > t.IdleMemoryBytes {
		return Recommendation{}, false
	}

	// SeverityInfo, not warning. An idle workload is wasting money, not breaking anything, and the
	// action carries real risk -- so it belongs below any reliability finding in the list. Marking it
	// urgent would encourage exactly the hasty deletion this rule should discourage.
	r := e.base(s, KindIdle, SeverityInfo)
	r.Summary = fmt.Sprintf("%s appears idle and may be deletable", s.Container)
	r.Current = fmt.Sprintf("cpu: %dm, memory: %s reserved",
		s.CPURequestedMillicores, humanBytes(s.MemRequestedBytes))
	r.Proposed = "delete the workload, or scale it to zero"
	r.Rationale = fmt.Sprintf(
		"Peak usage over %.1f hours was %dm CPU and %s memory -- below the idle floor of %dm and %s -- "+
			"while reserving %dm and %s. This is not merely oversized: it appears to do nothing at all. "+
			"BEFORE DELETING, confirm with the owner. A workload can be legitimately idle: a "+
			"disaster-recovery standby, a quarterly batch job, or a consumer waiting on a queue that is "+
			"currently empty. This analysis covers %.1f hours, so anything on a weekly or monthly cycle "+
			"would look identical to something abandoned.",
		s.Duration().Hours(), s.CPUMaxMillicores, humanBytes(s.MemMaxBytes),
		t.IdleCPUMillicores, humanBytes(t.IdleMemoryBytes),
		s.CPURequestedMillicores, humanBytes(s.MemRequestedBytes),
		s.Duration().Hours())

	// The whole reservation is reclaimable, since the proposal is to remove the workload.
	r.EstimatedMonthlySaving = e.monthlyCostOf(s,
		s.CPURequestedMillicores, s.MemRequestedBytes).Mul(decimal.NewFromInt(int64(s.Replicas)))
	return r, true
}

// =============================================================================
// rightSize: reserving far more than it needs
// =============================================================================

// rightSize fires when p95 usage is well below the request.
//
// THE PROPOSED VALUE IS p95 x HEADROOM, AND BOTH PARTS MATTER
// ----------------------------------------------------------
// p95 of the per-window PEAKS, not of the averages: averaging has already smoothed the bursts away, so
// a request derived from averages throttles the workload. See the note in stats_repo and migration
// 000003.
//
// Times a headroom factor, because p95 means by definition that 5% of windows EXCEEDED it. Sizing
// exactly at p95 guarantees throttling one window in twenty -- so the recommendation would cause the
// very problem it claims to optimise.
func (e *Engine) rightSize(s postgres.ContainerStats) (Recommendation, bool) {
	t := e.thresholds

	if s.CPURequestedMillicores == 0 && s.MemRequestedBytes == 0 {
		return Recommendation{}, false
	}
	// Without trustworthy peaks there is no safe basis for a reduction. Silence beats guessing.
	if s.PeakCoverage < 0.9 {
		return Recommendation{}, false
	}

	cpuOver := s.CPURequestedMillicores > 0 &&
		ratio(s.CPUP95Millicores, s.CPURequestedMillicores) < t.RightSizeMaxUtilisation
	memOver := s.MemRequestedBytes > 0 &&
		ratio(s.MemP95Bytes, s.MemRequestedBytes) < t.RightSizeMaxUtilisation

	if !cpuOver && !memOver {
		return Recommendation{}, false
	}

	proposedCPU := s.CPURequestedMillicores
	if cpuOver {
		proposedCPU = e.proposedCPU(s)
	}
	proposedMem := s.MemRequestedBytes
	if memOver {
		proposedMem = e.proposedMemory(s)
	}

	// A proposal must never exceed the current request. Headroom on a p95 that sits just under the
	// threshold can arithmetically overshoot, and recommending an INCREASE under a "right-size" label
	// would be incoherent.
	if proposedCPU > s.CPURequestedMillicores {
		proposedCPU = s.CPURequestedMillicores
	}
	if proposedMem > s.MemRequestedBytes {
		proposedMem = s.MemRequestedBytes
	}

	saving := e.monthlyCostOf(s,
		s.CPURequestedMillicores-proposedCPU,
		s.MemRequestedBytes-proposedMem,
	).Mul(decimal.NewFromInt(int64(s.Replicas)))

	// Suppress trivial findings. Shaving 3% off one container is noise that buries the finding worth
	// acting on -- and a list of forty marginal recommendations gets ignored wholesale.
	current := e.monthlyCostOf(s, s.CPURequestedMillicores, s.MemRequestedBytes).
		Mul(decimal.NewFromInt(int64(s.Replicas)))
	if current.IsPositive() {
		if saving.Div(current).LessThan(decimal.NewFromFloat(t.MinSavingRatio)) {
			return Recommendation{}, false
		}
	}

	r := e.base(s, KindRightSize, SeverityInfo)
	r.Summary = fmt.Sprintf("%s reserves substantially more than it uses", s.Container)
	// ONLY the resources actually being changed appear in the proposal.
	//
	// Restating an unchanged value produced genuinely contradictory output: for a container whose CPU
	// is over-provisioned while its memory is UNDER-requested, this rule would print the current
	// memory alongside a reduced CPU, while underRequested printed a larger memory for the same
	// container. Two recommendations, two different memory figures, no indication that only one of them
	// was proposing a memory change at all.
	r.Current = changeSummary(cpuOver, memOver, s.CPURequestedMillicores, s.MemRequestedBytes)
	r.Proposed = changeSummary(cpuOver, memOver, proposedCPU, proposedMem)
	r.Rationale = fmt.Sprintf(
		"Over %.1f hours across %d windows, p95 peak usage was %dm CPU (%.0f%% of the %dm requested) "+
			"and %s memory (%.0f%% of %s). The proposal is p95 x %.2f headroom. "+
			"Headroom is not padding: p95 means 5%% of windows EXCEEDED it, so sizing exactly at p95 "+
			"would throttle the workload one window in twenty. "+
			"Note this is p95 of per-window PEAKS, not of averages -- averaging smooths bursts away, and "+
			"a request derived from averages gets CPU-throttled or OOMKilled.",
		s.Duration().Hours(), s.WindowCount,
		s.CPUP95Millicores, ratio(s.CPUP95Millicores, s.CPURequestedMillicores)*100, s.CPURequestedMillicores,
		humanBytes(s.MemP95Bytes), ratio(s.MemP95Bytes, s.MemRequestedBytes)*100, humanBytes(s.MemRequestedBytes),
		t.HeadroomFactor)
	r.EstimatedMonthlySaving = saving
	return r, true
}

// =============================================================================
// underRequested: using more than it asked for
// =============================================================================

// underRequested fires when usage exceeds the request.
//
// WHY A COST TOOL MUST REPORT THIS, EVEN THOUGH ACTING ON IT COSTS MORE
// --------------------------------------------------------------------
// Every other rule saves money. This one spends it, and a tool whose advice is all one-directional is
// not trusted with production -- because the obvious suspicion is that it optimises the metric rather
// than the system.
//
// THE CPU/MEMORY ASYMMETRY IS THE HEART OF THIS RULE. They are not symmetric resources:
//
//	CPU is COMPRESSIBLE. Exceed the request and the container is throttled -- it gets slower. A
//	performance problem, so: WARNING.
//	MEMORY is NOT. Exceed the request and the kubelet may evict the pod; exceed the LIMIT and the
//	kernel OOMKills it. An availability problem, so: CRITICAL.
//
// Treating them alike would either cry wolf about throttling or under-report an imminent OOM kill.
func (e *Engine) underRequested(s postgres.ContainerStats) (Recommendation, bool) {
	// setRequests covers the no-request case with better advice.
	if s.CPURequestedMillicores == 0 && s.MemRequestedBytes == 0 {
		return Recommendation{}, false
	}
	if s.PeakCoverage < 0.9 {
		return Recommendation{}, false
	}

	// Compared against p95, not max. A single startup spike above the request is normal and not worth
	// a critical finding; p95 exceeding the request means it happens routinely.
	cpuUnder := s.CPURequestedMillicores > 0 && s.CPUP95Millicores > s.CPURequestedMillicores
	memUnder := s.MemRequestedBytes > 0 && s.MemP95Bytes > s.MemRequestedBytes

	if !cpuUnder && !memUnder {
		return Recommendation{}, false
	}

	// Memory decides the severity, because memory is the one that kills pods.
	severity := SeverityWarning
	if memUnder {
		severity = SeverityCritical
	}

	proposedCPU := s.CPURequestedMillicores
	if cpuUnder {
		proposedCPU = e.proposedCPU(s)
	}
	proposedMem := s.MemRequestedBytes
	if memUnder {
		proposedMem = e.proposedMemory(s)
	}

	// NEGATIVE, because raising a request costs money. Presenting that honestly is what makes the
	// positive savings credible.
	extra := e.monthlyCostOf(s,
		proposedCPU-s.CPURequestedMillicores,
		proposedMem-s.MemRequestedBytes,
	).Mul(decimal.NewFromInt(int64(s.Replicas)))

	r := e.base(s, KindUnderRequested, severity)

	risk := "CPU throttling, so the workload runs slower than it should"
	if memUnder {
		risk = "eviction under node memory pressure, and an OOM kill if it exceeds its limit -- " +
			"memory is incompressible, so the kernel cannot slow it down instead"
	}

	r.Summary = fmt.Sprintf("%s uses more than it requests and risks %s", s.Container,
		map[bool]string{true: "eviction", false: "throttling"}[memUnder])
	// Only the under-requested resources, for the same reason as rightSize above.
	r.Current = changeSummary(cpuUnder, memUnder, s.CPURequestedMillicores, s.MemRequestedBytes)
	r.Proposed = changeSummary(cpuUnder, memUnder, proposedCPU, proposedMem)
	r.Rationale = fmt.Sprintf(
		"p95 peak usage over %.1f hours was %dm CPU and %s memory, against requests of %dm and %s. "+
			"The risk is %s. "+
			"The scheduler also places this pod using the UNDERSTATED figure, so it believes the node "+
			"has more free capacity than it does -- which means this container can get innocent "+
			"neighbours evicted alongside it. "+
			"Applying this recommendation INCREASES cost by roughly %s per month. That is the correct "+
			"trade: the resources are already being consumed, and the request is what makes the "+
			"scheduler account for them.",
		s.Duration().Hours(), s.CPUP95Millicores, humanBytes(s.MemP95Bytes),
		s.CPURequestedMillicores, humanBytes(s.MemRequestedBytes),
		risk, extra.StringFixed(4))
	r.EstimatedMonthlySaving = extra.Neg()
	return r, true
}

// =============================================================================
// overReplicated: each pod is fine, there are simply too many
// =============================================================================

// overReplicated fires when a workload's per-pod sizing is reasonable but the replica count is not.
//
// WHY THIS RULE EXISTS SEPARATELY, AND WHY IT IS THE MOST DANGEROUS ONE HERE
// ------------------------------------------------------------------------
// Every other rule looks at one container's sizing. This one looks at the COUNT, which is waste on an
// axis per-container analysis cannot see: six perfectly-sized replicas where two would do are six
// findings of "nothing wrong" and one workload wasting four-thirds of its spend. That is exactly what
// deploy/demo-workloads/60-over-replicated.yaml exists to demonstrate.
//
// It is also the rule most able to cause harm, because replica count encodes REQUIREMENTS the metrics
// cannot see: availability targets, anti-affinity spread, headroom for a traffic spike the observation
// window never contained, or a PodDisruptionBudget that a scale-down would violate.
//
// So it is deliberately the most conservative rule: it never proposes fewer than MinReplicasToKeep,
// it requires low utilisation across the WHOLE workload, and its rationale states plainly that it
// cannot see the reasons the count might be correct.
func (e *Engine) overReplicated(s postgres.ContainerStats) (Recommendation, bool) {
	t := e.thresholds

	// THE REPLICA COUNT MUST ACTUALLY BE A FREE PARAMETER.
	//
	// This check was missing and the first live run made the consequence obvious: the tool advised
	// scaling kindnet and kps-prometheus-node-exporter to 2 replicas. Both are DAEMONSETS. Their pod
	// count is one per node by definition -- there is no replicas field to set, so the advice was
	// impossible to act on, and if it had been possible it would have been dangerous: removing a
	// kindnet pod leaves a node with no network plugin.
	//
	// The rule was reasoning from `count(DISTINCT pod_name) = 3` without asking WHY there were three.
	// For a Deployment three replicas is a choice; for a DaemonSet it is a consequence of the cluster
	// having three nodes. Same number, completely different meaning -- and a number whose meaning
	// depends on context cannot be interpreted without that context.
	//
	// A DaemonSet that is over-provisioned is still reported: right_size fires on its per-pod requests,
	// which IS the actionable change. So gating here costs no coverage, it just stops the tool giving
	// advice about a field that does not exist.
	if !replicaCountIsTunable(s.WorkloadKind) {
		return Recommendation{}, false
	}

	// More than the floor, or there is nothing to propose. A 2-replica workload at the floor is
	// already minimal.
	if s.Replicas <= t.MinReplicasToKeep {
		return Recommendation{}, false
	}
	if s.CPURequestedMillicores == 0 {
		return Recommendation{}, false
	}
	if s.PeakCoverage < 0.9 {
		return Recommendation{}, false
	}

	utilisation := ratio(s.CPUP95Millicores, s.CPURequestedMillicores)
	if utilisation > t.OverReplicationMaxUtilisation {
		return Recommendation{}, false
	}

	// How many replicas would carry the observed total at a comfortable utilisation.
	//
	// Uses the aggregate load rather than per-pod utilisation: the question is "could fewer pods absorb
	// what all of them are doing", which is a capacity question, not a per-pod one.
	totalP95 := s.CPUP95Millicores * int64(s.Replicas)
	perReplicaCapacity := int64(float64(s.CPURequestedMillicores) * t.OverReplicationMaxUtilisation * 2)
	proposed := t.MinReplicasToKeep
	if perReplicaCapacity > 0 {
		needed := int((totalP95 + perReplicaCapacity - 1) / perReplicaCapacity) // ceiling division
		if needed > proposed {
			proposed = needed
		}
	}
	if proposed >= s.Replicas {
		return Recommendation{}, false
	}

	removed := int64(s.Replicas - proposed)
	saving := e.monthlyCostOf(s, s.CPURequestedMillicores, s.MemRequestedBytes).
		Mul(decimal.NewFromInt(removed))

	// WARNING rather than info: the finding is real and the saving usually material, but acting on it
	// needs a human who knows the availability requirements.
	r := e.base(s, KindOverReplicated, SeverityWarning)
	r.Container = "" // a workload-level finding, not a container-level one
	r.Summary = fmt.Sprintf("%s/%s runs %d replicas that are each lightly used",
		s.WorkloadKind, s.WorkloadName, s.Replicas)
	r.Current = fmt.Sprintf("%d replicas", s.Replicas)
	r.Proposed = fmt.Sprintf("%d replicas", proposed)
	r.Rationale = fmt.Sprintf(
		"Each of the %d replicas reached a p95 peak of only %dm against a %dm request (%.0f%% utilised) "+
			"over %.1f hours. The per-pod sizing is reasonable -- no per-container rule flags it -- so the "+
			"waste is only visible at the replica count, which is why this is reported separately. "+
			"CONFIRM BEFORE SCALING DOWN. The replica count may encode requirements this analysis cannot "+
			"see: an availability target, anti-affinity spread across zones, headroom for a traffic spike "+
			"outside the observed window, or a PodDisruptionBudget a scale-down would violate. "+
			"The proposal never goes below %d replicas, because a single replica has no availability "+
			"during a rolling update or a node drain.",
		s.Replicas, s.CPUP95Millicores, s.CPURequestedMillicores, utilisation*100,
		s.Duration().Hours(), t.MinReplicasToKeep)
	r.EstimatedMonthlySaving = saving
	return r, true
}

// =============================================================================
// Shared arithmetic
// =============================================================================

// replicaCountIsTunable reports whether a workload kind has a replica count an operator can choose.
//
// An ALLOW-LIST rather than a deny-list of DaemonSet, and that is the important detail. A deny-list
// treats every unrecognised kind as tunable, so the first custom resource we meet -- an Argo Rollout,
// a KEDA ScaledObject, a Flink or Spark operator's CR -- silently inherits scale-down advice we have no
// basis for. The failure mode of an allow-list is a missing recommendation; the failure mode of a
// deny-list is confident wrong advice about someone else's CRD.
//
// Excluded on purpose:
//
//	DaemonSet  -- one pod per node, not a choice. This is the one that actually fired.
//	Job        -- parallelism is the job's own contract, and the pods are finite anyway
//	CronJob    -- same, per invocation
//	Node       -- how static pods surface here: owned by the Node, with no controller to scale
//	""         -- a bare pod has no controller at all, so there is nothing to change
func replicaCountIsTunable(kind string) bool {
	switch kind {
	case "Deployment", "ReplicaSet", "StatefulSet", "ReplicationController":
		return true
	default:
		return false
	}
}

// proposedCPU is p95 x headroom, rounded UP, floored at 1 millicore.
//
// The floor matters: a container using well under a millicore would otherwise be proposed a request of
// 0, which means BestEffort -- turning a right-size recommendation into the setRequests problem.
//
// WHY CEIL AND NOT A PLAIN int64 CONVERSION
// -----------------------------------------
// A Go float-to-int conversion truncates toward zero, and that silently erased the headroom for every
// small p95 -- which is the COMMON case, not an edge one. At the 1.2 default:
//
//	p95=1m -> 1.2 -> 1m    p95=3m -> 3.6 -> 3m
//	p95=2m -> 2.4 -> 2m    p95=4m -> 4.8 -> 4m
//
// Four of the first five values got NO headroom whatsoever. The proposal landed exactly at p95, and
// p95 means 5% of windows exceeded it, so acting on that advice throttles the container one window in
// twenty. Every low-usage container in this cluster -- an nginx sits at 0-2m -- was in that band.
//
// Rounding UP is the only defensible direction for a safety margin: rounding a margin down is the same
// as not having one. The cost of a millicore of over-provisioning is roughly 0.004 cents a month; the
// cost of CFS throttling a latency-sensitive service is a pager.
func (e *Engine) proposedCPU(s postgres.ContainerStats) int64 {
	v := int64(math.Ceil(float64(s.CPUP95Millicores) * e.thresholds.HeadroomFactor))
	if v < 1 {
		v = 1
	}
	return v
}

// proposedMemory is p95 x headroom, rounded UP to the next MiB and floored at 16 MiB.
//
// Rounded up because memory requests are written in MiB or GiB in real manifests, and a proposal of
// "37.4 MiB" cannot be pasted into one. Floored because no container survives on less -- a runtime
// alone needs more than that, and proposing below it would cause an immediate OOM kill.
func (e *Engine) proposedMemory(s postgres.ContainerStats) int64 {
	// Ceil for the same reason as proposedCPU. The MiB round-up below happens to hide the truncation
	// here, but relying on one rounding step to fix another is how the CPU bug survived review.
	v := int64(math.Ceil(float64(s.MemP95Bytes) * e.thresholds.HeadroomFactor))
	const mib = 1024 * 1024
	v = ((v + mib - 1) / mib) * mib
	if v < 16*mib {
		v = 16 * mib
	}
	return v
}

// monthlyCostOf converts a resource quantity into a monthly figure at this container's rates.
//
// Negative inputs are honoured rather than clamped, which is what lets underRequested express a cost
// INCREASE using the same arithmetic as a saving.
func (e *Engine) monthlyCostOf(s postgres.ContainerStats, cpuMillicores, memBytes int64) decimal.Decimal {
	hours := decimal.NewFromInt(hoursPerMonth)

	cores := decimal.NewFromInt(cpuMillicores).DivRound(decimal.NewFromInt(1000), 18)
	gib := decimal.NewFromInt(memBytes).DivRound(decimal.NewFromInt(bytesPerGiB), 18)

	cpuCost := cores.Mul(hours).Mul(s.CPUCostPerCoreHour)
	memCost := gib.Mul(hours).Mul(s.MemCostPerGiBHour)

	// Four decimal places: a monthly figure in currency units, where more precision is noise and less
	// would hide a genuinely small saving entirely.
	return cpuCost.Add(memCost).Round(4)
}

// ratio is used / requested, guarding against division by zero.
//
// float64 is acceptable HERE and only here: this is a threshold comparison and a display percentage,
// never money. Using decimal for it would be precision theatre.
func ratio(used, requested int64) float64 {
	if requested <= 0 {
		return 0
	}
	return float64(used) / float64(requested)
}

// changeSummary renders only the resources a recommendation is actually changing.
//
// A proposal that restates an unchanged value reads as a claim about it, and when two rules fire on
// the same container for different resources the result is two conflicting figures with nothing to say
// which one is being proposed.
func changeSummary(includeCPU, includeMem bool, cpuMillicores, memBytes int64) string {
	switch {
	case includeCPU && includeMem:
		return fmt.Sprintf("cpu: %dm, memory: %s", cpuMillicores, humanBytes(memBytes))
	case includeCPU:
		return fmt.Sprintf("cpu: %dm", cpuMillicores)
	case includeMem:
		return fmt.Sprintf("memory: %s", humanBytes(memBytes))
	default:
		// Unreachable: a rule only fires when at least one resource is involved. Returning a marker
		// rather than an empty string means a future rule that reaches here is obvious rather than
		// silently blank.
		return "no change"
	}
}

// humanBytes renders a byte count the way a Kubernetes manifest would.
//
// Binary units (MiB, GiB), matching what Kubernetes accepts and reports. Rendering "40MB" when the
// manifest needs "40Mi" produces a value someone will paste and get wrong by 5%.
func humanBytes(b int64) string {
	switch {
	case b >= bytesPerGiB:
		return fmt.Sprintf("%.1fGi", float64(b)/float64(bytesPerGiB))
	case b >= 1024*1024:
		return fmt.Sprintf("%dMi", b/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%dKi", b/1024)
	default:
		return fmt.Sprintf("%dB", b)
	}
}
