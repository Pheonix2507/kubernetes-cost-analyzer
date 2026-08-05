package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// A cost sample lives in the domain rather than in the persistence layer because three
// packages now speak it: internal/costing PRODUCES it, internal/store/postgres PERSISTS it,
// and internal/httpapi will SERVE it. Leaving it in the postgres package would mean the cost
// engine importing a SQL package in order to describe its own output -- the same inverted
// dependency that moved Node and Pod here in Phase 2.

// ContainerAllocation is one immutable cost sample: what a single container reserved and
// used over one time window, and what that cost.
//
// WHY MONEY IS decimal.Decimal AND NOT float64
// --------------------------------------------
// float64 cannot represent 0.1 exactly, and the error compounds under SUM. Adding
// 0.0000267 eleven million times -- one row per container per window per month -- drifts
// from the true total, and the drift is not reproducible because it depends on summation
// order, which the query planner is free to change between runs.
//
// A cost report that disagrees with itself between two runs, or with the cloud invoice it
// is reconciled against, is not a rounding curiosity. It is the end of anyone trusting the
// tool. decimal.Decimal is exact base-10 arithmetic and maps directly onto Postgres
// numeric, which is why the schema uses numeric(20,10) rather than double precision.
//
// The trade-off is real and worth stating: decimal arithmetic is roughly an order of
// magnitude slower than float64 and allocates. That is irrelevant here, because every one
// of these values crosses a network and a disk, and the I/O dominates by orders of
// magnitude.
type ContainerAllocation struct {
	// The window is HALF-OPEN: [WindowStart, WindowEnd).
	//
	// With closed intervals, consecutive windows share an endpoint and every range query
	// double-counts the boundary sample. Essentially every time-bucketing bug in cost
	// reporting traces back to this choice.
	WindowStart time.Time
	WindowEnd   time.Time

	PodID         int64
	ContainerName string

	// --- Attribution: an immutable snapshot of ownership at collection time ---------
	// Denormalised on purpose. If team were joined from the namespaces table, relabelling
	// a namespace would silently rewrite every historical report, so last month's
	// finalised figure would change after the fact.
	ClusterName   string
	NamespaceName string
	PodName       string
	WorkloadKind  string
	WorkloadName  string
	NodeName      string
	Team          string
	CostCentre    string
	Environment   string
	InstanceType  string
	CapacityType  string
	Zone          string
	QoSClass      string

	// RateSource records HOW the rate was derived: an exact catalogue match, published
	// per-resource rates, or a fallback guess for an unrecognised instance type.
	//
	// Persisted rather than discarded, because once a cost is a number in a database a real
	// price and a guess look identical -- and a report that cannot distinguish them invites
	// someone to present a fabricated figure as fact.
	RateSource string

	// --- Measurements, in the same integer units internal/kube uses ---------------
	CPUMillicoresRequested int64
	MemoryBytesRequested   int64
	CPUMillicoresUsed      int64
	MemoryBytesUsed        int64

	// Peaks within the window. Used by right-sizing, never by cost. See domain.Usage.
	CPUMillicoresMax int64
	MemoryBytesMax   int64

	// --- Rates in effect for THIS window -----------------------------------------
	CPUCostPerCoreHour   decimal.Decimal
	MemoryCostPerGiBHour decimal.Decimal

	CPUCost    decimal.Decimal
	MemoryCost decimal.Decimal
}

// Billable returns the quantities cost is actually charged on: max(requested, used).
//
// WHY max AND NOT requested
// -------------------------
// Requests alone are the obvious basis, and they miss an entire class of workload. A
// BestEffort container declares no requests at all, so a request-only formula prices it at
// exactly ZERO while it consumes real CPU and memory on a real node. That cost does not
// disappear -- it is silently smeared across every other team's share, so everyone else is
// over-billed to hide it.
//
// This is why the no-requests-at-all fixture exists in deploy/demo-workloads, and why the
// database has a CHECK constraint asserting the stored billable value really is the max:
// the rule is the product's definition and must not drift between the three layers that
// would otherwise each reimplement it.
//
// A method on the struct rather than a field the caller sets, so it cannot be filled in
// wrongly. The database constraint then catches anyone bypassing this by writing SQL
// directly.
func (a ContainerAllocation) Billable() (cpuMillicores, memoryBytes int64) {
	return max(a.CPUMillicoresRequested, a.CPUMillicoresUsed),
		max(a.MemoryBytesRequested, a.MemoryBytesUsed)
}

// Validate checks the invariants that the database also enforces.
//
// WHY CHECK IN BOTH PLACES
// ------------------------
// The CHECK constraints are the real guarantee -- they hold against psql, a migration, and
// any future service. But a constraint violation surfaces as an opaque Postgres error
// naming a constraint, arriving after a network round trip, and if it happens mid-batch it
// aborts the whole transaction including every valid row alongside it.
//
// Validating first turns that into a precise, local error naming the field and the pod, and
// keeps one malformed sample from discarding the other 4,999 in the batch. The database
// remains the backstop; this is the useful error message.
func (a ContainerAllocation) Validate() error {
	var errs []error

	if a.WindowStart.IsZero() || a.WindowEnd.IsZero() {
		errs = append(errs, errors.New("window_start and window_end must both be set"))
	} else if !a.WindowEnd.After(a.WindowStart) {
		errs = append(errs, fmt.Errorf("window_end (%s) must be after window_start (%s)",
			a.WindowEnd.Format(time.RFC3339), a.WindowStart.Format(time.RFC3339)))
	}
	if a.PodID == 0 {
		errs = append(errs, errors.New("pod_id must be set"))
	}
	if a.ContainerName == "" {
		// Part of the primary key, so an empty name would silently collide with any other
		// unnamed container in the same pod and window -- one would overwrite the other.
		errs = append(errs, errors.New("container_name must not be empty"))
	}
	if a.ClusterName == "" || a.NamespaceName == "" {
		errs = append(errs, errors.New("cluster_name and namespace_name must be set for attribution"))
	}
	if a.CPUMillicoresRequested < 0 || a.MemoryBytesRequested < 0 ||
		a.CPUMillicoresUsed < 0 || a.MemoryBytesUsed < 0 {
		errs = append(errs, errors.New("resource amounts must not be negative"))
	}
	if a.CPUCost.IsNegative() || a.MemoryCost.IsNegative() {
		errs = append(errs, errors.New("costs must not be negative"))
	}

	return errors.Join(errs...)
}
