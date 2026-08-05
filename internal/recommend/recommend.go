// Package recommend turns observed behaviour into actionable advice.
//
// WHY THIS IS THE HARDEST PACKAGE IN THE PROJECT
// ---------------------------------------------
// Everything before it reports facts. This one makes CLAIMS about what someone should change, and it
// can be wrong in two directions that are not symmetric:
//
//	A FALSE POSITIVE -- flagging a workload that is correctly sized -- costs trust. Engineers
//	learn the tool cries wolf, stop reading it, and then it cannot help them with the real waste
//	either. This is why deploy/demo-workloads includes right-sized-worker, whose entire job is to
//	NOT be flagged.
//
//	A DANGEROUS RECOMMENDATION -- telling someone to shrink a request below what the workload
//	peaks at -- causes an incident. That is worse than useless: the tool actively harms the system
//	it was brought in to improve, and no amount of correct advice afterwards recovers from it.
//
// So the bias throughout is CONSERVATIVE: stay silent when the evidence is thin, leave headroom on
// every proposed reduction, and never recommend an action whose failure mode is an outage without
// saying so.
//
// EVERY RULE IS A PURE FUNCTION over ContainerStats. No database, no clock, no I/O -- so each rule's
// exact threshold behaviour is testable with a struct literal, and the fixtures can be graded as a
// table test rather than by inspecting a live cluster.
package recommend

import (
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// Kind identifies what a recommendation is about.
type Kind string

// The kinds of recommendation this engine produces.
const (
	// KindRightSize means the request is far above what the workload needs. Reduce it.
	KindRightSize Kind = "right_size"
	// KindIdle means the workload does essentially nothing. Consider deleting it.
	KindIdle Kind = "idle"
	// KindUnderRequested means usage exceeds the request. RAISE it -- this one costs more money.
	KindUnderRequested Kind = "under_requested"
	// KindSetRequests means no request is declared at all, so the cost is unattributable.
	KindSetRequests Kind = "set_requests"
	// KindOverReplicated means the per-pod sizing is fine but there are too many pods.
	KindOverReplicated Kind = "over_replicated"
)

// KindOptions lists every kind this engine can emit.
//
// WHY A LIST AND NOT JUST THE CONSTANTS
// -------------------------------------
// api/openapi.yaml documents these as an enum, and a hand-written spec can drift from the code in a
// way a generated one cannot. A documented enum that no longer matches what the server emits is
// worse than no documentation: a client writes a switch over five kinds, we add a sixth, and their
// UI silently drops findings it has no branch for.
//
// So openapi_test.go compares the spec's enum against THIS list. Adding a rule without documenting
// it fails the build, which is the cheapest possible place to catch it.
func KindOptions() []string {
	return []string{
		string(KindRightSize), string(KindIdle), string(KindUnderRequested),
		string(KindSetRequests), string(KindOverReplicated),
	}
}

// Severity says how much attention a recommendation deserves.
//
// Deliberately NOT ordered by money. A memory under-request that risks an OOM kill is critical even
// though acting on it INCREASES cost, while a large saving on an over-provisioned batch job is merely
// informational. Sorting advice by savings would put the dangerous findings at the bottom.
type Severity string

// The severities, from least to most urgent.
const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// SeverityOptions lists the severities, least to most urgent. See KindOptions for why this exists.
func SeverityOptions() []string {
	return []string{string(SeverityInfo), string(SeverityWarning), string(SeverityCritical)}
}

// severityRank orders severities for sorting.
var severityRank = map[Severity]int{SeverityCritical: 0, SeverityWarning: 1, SeverityInfo: 2}

// Confidence reflects how much evidence supports a recommendation.
//
// Surfaced rather than used only as a filter, because the right response differs: a high-confidence
// right-size can be applied, whereas a low-confidence one means "look at this, and collect more data
// before acting".
type Confidence string

// The confidence levels.
const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

// ConfidenceOptions lists the confidence levels, lowest first. See KindOptions for why this exists.
func ConfidenceOptions() []string {
	return []string{string(ConfidenceLow), string(ConfidenceMedium), string(ConfidenceHigh)}
}

// Recommendation is one piece of advice.
type Recommendation struct {
	Kind       Kind       `json:"kind"`
	Severity   Severity   `json:"severity"`
	Confidence Confidence `json:"confidence"`

	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workload_kind,omitempty"`
	WorkloadName string `json:"workload_name,omitempty"`
	Container    string `json:"container_name,omitempty"`

	// Summary is one line a human can act on.
	Summary string `json:"summary"`
	// Rationale explains WHY, including the numbers. A recommendation nobody understands is a
	// recommendation nobody applies -- and one that cannot be argued with cannot be trusted either.
	Rationale string `json:"rationale"`

	// Current and Proposed describe the change in the workload's own units, so the value can be
	// pasted into a manifest without conversion.
	Current  string `json:"current,omitempty"`
	Proposed string `json:"proposed,omitempty"`

	// EstimatedMonthlySaving may be NEGATIVE, which is the point.
	//
	// A tool whose advice is all one-directional is not trusted with production. Raising an
	// under-requested memory limit costs money and is still the right thing to do, and presenting
	// that honestly as a negative saving is what makes the positive ones credible.
	EstimatedMonthlySaving decimal.Decimal `json:"estimated_monthly_saving"`

	// Evidence is the observation that produced this.
	Evidence Evidence `json:"evidence"`

	// EstimatedRates is true when the underlying cost came from a fallback rate, so the saving figure
	// is itself an estimate built on an estimate.
	EstimatedRates bool `json:"estimated_rates"`
}

// Evidence is the data behind a recommendation, so it can be checked rather than believed.
type Evidence struct {
	ObservedFrom  time.Time `json:"observed_from"`
	ObservedTo    time.Time `json:"observed_to"`
	ObservedHours float64   `json:"observed_hours"`
	Windows       int       `json:"windows"`
	Replicas      int       `json:"replicas"`

	CPURequestedMillicores int64 `json:"cpu_requested_millicores"`
	CPUAvgMillicores       int64 `json:"cpu_avg_millicores"`
	CPUP95Millicores       int64 `json:"cpu_p95_millicores"`
	CPUMaxMillicores       int64 `json:"cpu_max_millicores"`

	MemRequestedBytes int64 `json:"memory_requested_bytes"`
	MemAvgBytes       int64 `json:"memory_avg_bytes"`
	MemP95Bytes       int64 `json:"memory_p95_bytes"`
	MemMaxBytes       int64 `json:"memory_max_bytes"`
}

// Thresholds are the engine's tunable policy.
//
// EXPOSED AS CONFIGURATION rather than buried as constants, for the same reason the CPU/memory split
// is a value in a YAML file: these numbers are judgement calls, not facts. A platform team running
// latency-critical services wants far more headroom than one running batch jobs, and a tool that
// hides its thresholds cannot be argued with.
type Thresholds struct {
	// MinWindows and MinObservation are the evidence floor.
	//
	// THE MOST IMPORTANT THRESHOLDS HERE. Without them the engine would recommend deleting a service
	// from twenty minutes of data collected overnight -- and a nightly batch job looks completely
	// idle for twenty-three hours a day. Every rule refuses to fire below these.
	MinWindows     int
	MinObservation time.Duration

	// HeadroomFactor multiplies p95 to produce a proposed request.
	//
	// 1.2 means "leave 20% above the observed p95". Not optional: p95 by definition means 5% of
	// windows exceeded it, so sizing exactly at p95 guarantees throttling one window in twenty.
	HeadroomFactor float64

	// RightSizeMaxUtilisation is the p95-against-request ratio below which a container is considered
	// over-provisioned. 0.5 means "using under half of what it reserved".
	//
	// Set generously on purpose. A tighter threshold produces more findings AND more false positives,
	// and the fixture set exists to keep this honest: right-sized-worker sits around 0.7 and must
	// stay silent.
	RightSizeMaxUtilisation float64

	// MinSavingRatio suppresses trivial findings. Shaving 3% off one container is noise that buries
	// the finding worth acting on.
	MinSavingRatio float64

	// IdleCPUMillicores and IdleMemoryBytes are the ceilings below which a container counts as doing
	// nothing. Compared against the PEAK, not the average: "never exceeded 5 millicores" is a far
	// stronger claim than "averaged 5 millicores".
	IdleCPUMillicores int64
	IdleMemoryBytes   int64
	// IdleMinObservation is a LONGER floor than MinObservation, because "delete this" is the most
	// destructive advice here and deserves the most evidence.
	IdleMinObservation time.Duration

	// MinReplicasToKeep floors any scale-down recommendation.
	//
	// 2, not 1. A single replica has no availability during a rolling update or a node drain, so
	// recommending a scale to 1 trades a small saving for an outage window -- and a cost tool that
	// causes downtime has failed regardless of what it saved.
	MinReplicasToKeep int
	// OverReplicationMaxUtilisation is the per-pod utilisation below which a workload's replica count
	// looks excessive.
	OverReplicationMaxUtilisation float64
}

// DefaultThresholds returns a deliberately conservative policy.
func DefaultThresholds() Thresholds {
	return Thresholds{
		// 12 windows at the default 5-minute interval is an hour. Enough to distinguish a genuine
		// pattern from a momentary blip, and low enough that the tool says something useful on its
		// first day.
		MinWindows:     12,
		MinObservation: time.Hour,

		HeadroomFactor:          1.2,
		RightSizeMaxUtilisation: 0.5,
		MinSavingRatio:          0.10,

		// 1 millicore, NOT 5 -- and this was a bug the fixtures caught.
		//
		// A 5m floor looks reasonable and is wrong: the over-provisioned-api fixture is an nginx that
		// idles at 2-5m CPU and 8Mi memory while genuinely serving requests. At a 5m floor it fell
		// below the threshold and the engine recommended DELETING A WORKING SERVICE -- the most
		// damaging false positive this package can produce.
		//
		// CPU does the discriminating, deliberately: a container doing any work at all burns CPU,
		// whereas even a completely dead one holds memory. `sleep infinity` measures 0m; nginx
		// measures 2-5m. A 1m floor separates them cleanly, and the memory floor stays generous
		// because a bare Go or JVM runtime holds several MiB while doing nothing.
		//
		// This errs toward MISSING idle workloads rather than flagging working ones. That asymmetry is
		// intentional: a missed saving costs money, and a wrongly-recommended deletion costs an outage.
		IdleCPUMillicores: 1,
		// 4 MiB, and this number was corrected twice by real measurements.
		//
		// At 16 MiB the over-provisioned-api fixture -- an nginx that genuinely serves its readiness
		// probe -- fell below the floor and was recommended for DELETION. Measured: it holds 6 MiB and
		// its CPU peak rounds to 0, because 2 millicores of a core is 0.002 and our peak resolution is
		// whole millicores. So CPU could not discriminate at all, and memory had to.
		//
		// Measured values that this floor separates: 'sleep infinity' holds 335 KiB; nginx holds 6 MiB.
		//
		// BE HONEST ABOUT THE LIMITATION. This is tuned against two containers, and a Go service idling
		// at 8 MiB would not be detected as idle -- a false negative. That is the safe direction, but it
		// means idle detection here is weaker than it looks. Doing it properly needs TRAFFIC data: a
		// workload behind a Service receiving no requests is a far stronger signal than any resource
		// threshold, and that is what the Services grant in deploy/rbac was reserved for.
		IdleMemoryBytes: 4 * 1024 * 1024,
		// 24 hours, because a workload idle for one hour may simply be between batches. This still is
		// not really long enough -- a weekly job looks idle for six days -- which is why the idle
		// rationale says so explicitly rather than pretending otherwise.
		IdleMinObservation: 24 * time.Hour,

		MinReplicasToKeep:             2,
		OverReplicationMaxUtilisation: 0.3,
	}
}

// Engine produces recommendations from container statistics.
type Engine struct {
	thresholds Thresholds
}

// NewEngine returns an engine, filling in any unset threshold from the defaults.
//
// Zero-value fields are replaced rather than accepted, because a zero MinWindows would disable the
// evidence floor entirely -- the most dangerous possible default.
func NewEngine(t Thresholds) *Engine {
	d := DefaultThresholds()
	if t.MinWindows <= 0 {
		t.MinWindows = d.MinWindows
	}
	if t.MinObservation <= 0 {
		t.MinObservation = d.MinObservation
	}
	if t.HeadroomFactor < 1 {
		// Below 1 would propose a request BELOW the observed p95, which is the dangerous direction.
		t.HeadroomFactor = d.HeadroomFactor
	}
	if t.RightSizeMaxUtilisation <= 0 || t.RightSizeMaxUtilisation >= 1 {
		t.RightSizeMaxUtilisation = d.RightSizeMaxUtilisation
	}
	if t.MinSavingRatio < 0 {
		t.MinSavingRatio = d.MinSavingRatio
	}
	if t.IdleCPUMillicores <= 0 {
		t.IdleCPUMillicores = d.IdleCPUMillicores
	}
	if t.IdleMemoryBytes <= 0 {
		t.IdleMemoryBytes = d.IdleMemoryBytes
	}
	if t.IdleMinObservation <= 0 {
		t.IdleMinObservation = d.IdleMinObservation
	}
	if t.MinReplicasToKeep < 1 {
		t.MinReplicasToKeep = d.MinReplicasToKeep
	}
	if t.OverReplicationMaxUtilisation <= 0 || t.OverReplicationMaxUtilisation >= 1 {
		t.OverReplicationMaxUtilisation = d.OverReplicationMaxUtilisation
	}
	return &Engine{thresholds: t}
}

// Analyse produces recommendations for every container in stats.
//
// Sorted by severity then by saving, so the dangerous findings come first and the largest savings
// lead within each severity. Sorting purely by money would bury an imminent OOM kill beneath a list
// of modest efficiencies.
func (e *Engine) Analyse(stats []postgres.ContainerStats) []Recommendation {
	out := []Recommendation{}
	for _, s := range stats {
		out = append(out, e.analyseContainer(s)...)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if severityRank[out[i].Severity] != severityRank[out[j].Severity] {
			return severityRank[out[i].Severity] < severityRank[out[j].Severity]
		}
		// Larger saving first within a severity. Comparison via decimal, never float.
		return out[i].EstimatedMonthlySaving.GreaterThan(out[j].EstimatedMonthlySaving)
	})
	return out
}

// analyseContainer applies every rule to one container.
func (e *Engine) analyseContainer(s postgres.ContainerStats) []Recommendation {
	// THE EVIDENCE GATE, before any rule runs.
	//
	// Silence is the correct output for thin data, and it must be an early return rather than a
	// per-rule check -- a rule added later would otherwise quietly bypass it.
	if s.WindowCount < e.thresholds.MinWindows || s.Duration() < e.thresholds.MinObservation {
		return nil
	}

	var out []Recommendation

	// Order matters, because the rules are not independent.
	//
	// A container declaring no requests cannot be right-sized (there is nothing to reduce) and is not
	// idle in any actionable sense (there is no reservation to reclaim), so setRequests runs first and
	// the others skip that case. An idle container likewise should be reported as idle rather than as
	// merely over-provisioned, since the ACTIONS differ: delete versus resize.
	if r, ok := e.setRequests(s); ok {
		out = append(out, r)
		return out
	}
	if r, ok := e.idle(s); ok {
		out = append(out, r)
		// Deliberately NOT returning: an idle container is also under-requested if it somehow uses
		// more memory than it asked for, and that is still worth reporting.
	} else if r, ok := e.rightSize(s); ok {
		out = append(out, r)
	}
	if r, ok := e.underRequested(s); ok {
		out = append(out, r)
	}
	if r, ok := e.overReplicated(s); ok {
		out = append(out, r)
	}

	return out
}

// evidenceFor builds the evidence block.
func evidenceFor(s postgres.ContainerStats) Evidence {
	return Evidence{
		ObservedFrom:  s.ObservedFrom,
		ObservedTo:    s.ObservedTo,
		ObservedHours: s.Duration().Hours(),
		Windows:       s.WindowCount,
		Replicas:      s.Replicas,

		CPURequestedMillicores: s.CPURequestedMillicores,
		CPUAvgMillicores:       s.CPUAvgMillicores,
		CPUP95Millicores:       s.CPUP95Millicores,
		CPUMaxMillicores:       s.CPUMaxMillicores,

		MemRequestedBytes: s.MemRequestedBytes,
		MemAvgBytes:       s.MemAvgBytes,
		MemP95Bytes:       s.MemP95Bytes,
		MemMaxBytes:       s.MemMaxBytes,
	}
}

// confidenceFor grades the evidence.
//
// Based on observation SPAN rather than window count, because a hundred windows over an hour still
// only tells you about that hour -- and the thing a recommendation needs to survive is a daily cycle.
func (e *Engine) confidenceFor(s postgres.ContainerStats) Confidence {
	switch d := s.Duration(); {
	case d >= 7*24*time.Hour:
		// A week covers weekday and weekend patterns, which is what makes a right-size safe to apply
		// without further thought.
		return ConfidenceHigh
	case d >= 24*time.Hour:
		// A day covers the daily peak, which is the pattern that matters most.
		return ConfidenceMedium
	default:
		return ConfidenceLow
	}
}

func (e *Engine) base(s postgres.ContainerStats, kind Kind, sev Severity) Recommendation {
	return Recommendation{
		Kind:           kind,
		Severity:       sev,
		Confidence:     e.confidenceFor(s),
		Namespace:      s.Namespace,
		WorkloadKind:   s.WorkloadKind,
		WorkloadName:   s.WorkloadName,
		Container:      s.Container,
		Evidence:       evidenceFor(s),
		EstimatedRates: s.EstimatedRates,
		// Zero rather than nil, so a caller never has to guard against an unset decimal.
		EstimatedMonthlySaving: decimal.Zero,
	}
}
