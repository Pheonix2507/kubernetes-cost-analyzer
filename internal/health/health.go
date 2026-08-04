// Package health defines dependency health checking and aggregates the results.
//
// WHY THIS PACKAGE EXISTS
// -----------------------
// Kubernetes needs to ask our process two DIFFERENT questions, and conflating them
// is one of the most damaging mistakes you can make in a production deployment:
//
//	LIVENESS  ("are you alive?")     -> /healthz
//	  Failing it makes the kubelet KILL AND RESTART the container.
//	  It must therefore check NOTHING except that the process is running and its
//	  event loop is responsive. No database, no cache, no downstream service.
//
//	READINESS ("can you serve traffic?") -> /readyz
//	  Failing it REMOVES THE POD FROM SERVICE ENDPOINTS. Traffic stops arriving;
//	  the container keeps running and is given the chance to recover.
//	  This is where dependency checks belong.
//
// WHAT GOES WRONG IF YOU CONFLATE THEM
// ------------------------------------
// Suppose liveness pings the database, and the database has a 30-second blip. Every
// replica fails liveness simultaneously, so the kubelet restarts every replica at
// once. Now you have a cold start stampede: empty caches, a thundering herd of new
// connections against the database that was already struggling, and the restarts
// keep failing because the dependency is still unhealthy. A brief database blip has
// become a total, self-sustaining outage -- caused entirely by the health check.
//
// With the split, the same blip merely removes pods from the load balancer until the
// database recovers, then traffic resumes. Nothing restarts. Nothing stampedes.
//
// This package therefore serves ONLY readiness. Liveness needs no abstraction at
// all: it returns 200 unconditionally, and that is correct.
package health

import (
	"context"
	"sync"
	"time"
)

// Checker is a dependency that can report whether it is currently usable.
//
// WHY AN INTERFACE HERE AND NOT ELSEWHERE IN PHASE 0
// --------------------------------------------------
// An interface earns its place when there are multiple real implementations and a
// caller that must not know which one it has. Both are true here: Postgres
// implements it now, and Prometheus (Phase 4) and the Kubernetes API (Phase 1) will
// implement it later. The /readyz handler consumes Checker and will never change as
// those arrive.
//
// It is deliberately tiny. A single-method-plus-name interface is trivial to
// implement, trivial to fake in tests, and impossible to misuse. Interfaces in Go
// should be discovered from real duplication, not designed up front -- and they
// belong in the package that CONSUMES them (here), not the package that implements
// them. That is what lets store/postgres depend on nothing of ours.
type Checker interface {
	// Name identifies the dependency in the readiness response, e.g. "postgres".
	Name() string
	// Check returns nil when the dependency is usable. It MUST honour ctx
	// cancellation: a check that ignores its deadline can hang the whole
	// readiness probe and get the pod killed for the wrong reason.
	Check(ctx context.Context) error
}

// Status is the health verdict for one dependency or for the service overall.
type Status string

// The only two health verdicts. Deliberately binary: a "degraded" state invites
// endless argument about whether to route traffic to it, and Kubernetes endpoints are
// binary regardless.
const (
	StatusUp   Status = "up"
	StatusDown Status = "down"
)

// CheckResult is the outcome of one dependency check.
type CheckResult struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	// Error carries the failure reason. omitempty keeps successful responses clean.
	//
	// NOTE: this leaks internal detail (hostnames, driver messages) to whoever can
	// reach /readyz. That is acceptable and useful for a probe reachable only from
	// inside the cluster, but /readyz must NEVER be exposed through a public
	// ingress. Phase 5 puts it behind an internal-only route.
	Error string `json:"error,omitempty"`
	// LatencyMS is how long the check took. Rising latency here is an early
	// warning signal well before a check starts outright failing.
	LatencyMS int64 `json:"latency_ms"`
}

// Report is the aggregate readiness of every dependency.
type Report struct {
	Status Status        `json:"status"`
	Checks []CheckResult `json:"checks"`
}

// Aggregator runs a set of Checkers and combines their results.
type Aggregator struct {
	checkers []Checker
	// timeout bounds EACH INDIVIDUAL check.
	//
	// Per-check rather than overall, deliberately: with an overall budget, one slow
	// dependency consumes the whole allowance and healthy dependencies get reported
	// as failed because they were never given time to answer. Per-check means a slow
	// dependency is reported as slow and the others still report honestly.
	timeout time.Duration
}

// NewAggregator returns an Aggregator over checkers.
//
// Variadic checkers make the wiring in main read as a list of dependencies, and make
// the zero-dependency case (no checkers at all) legal -- which is genuinely useful
// for a binary like the collector that has nothing to expose yet.
func NewAggregator(timeout time.Duration, checkers ...Checker) *Aggregator {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Aggregator{checkers: checkers, timeout: timeout}
}

// Run executes every check CONCURRENTLY and returns the combined report.
//
// The overall status is StatusDown if ANY check fails. Readiness is not a democracy:
// if the database is unreachable, this replica cannot serve, no matter how healthy
// everything else is.
//
// WHY CONCURRENT: checks are I/O-bound network round trips. Run serially, total
// latency is the SUM of every check; run concurrently, it is the MAXIMUM. With four
// dependencies at 100ms each that is 400ms versus 100ms -- and readiness probes run
// every few seconds for the life of the pod, on every replica.
func (a *Aggregator) Run(ctx context.Context) Report {
	// The zero-checker case must not allocate goroutines or return a nil slice
	// (which would marshal as JSON `null` rather than `[]` and irritate clients).
	if len(a.checkers) == 0 {
		return Report{Status: StatusUp, Checks: []CheckResult{}}
	}

	// Pre-allocated to exactly len(checkers), and each goroutine writes to ONE
	// distinct index.
	//
	// THIS IS WHY THERE IS NO MUTEX HERE. Distinct elements of a slice are distinct
	// memory locations, so concurrent writes to results[0] and results[1] do not
	// race -- and the slice header itself is never modified, only read. Had we used
	// `results = append(results, r)` instead, every goroutine would be mutating the
	// shared slice header (length, and possibly the backing array pointer on
	// growth), which IS a data race and would corrupt or lose results.
	//
	// `go test -race` catches the append version immediately. It is worth writing
	// it wrong once just to see the detector fire.
	results := make([]CheckResult, len(a.checkers))

	// WaitGroup rather than a channel: we are not streaming results, we simply need
	// to know when all goroutines have finished. A channel would work but adds
	// buffering decisions and a collection loop for no benefit. Reach for a channel
	// when you need to COMMUNICATE values, and a WaitGroup when you only need to
	// wait.
	var wg sync.WaitGroup

	for i, c := range a.checkers {
		wg.Add(1)
		// Go 1.22 onwards, `i` and `c` are fresh variables per iteration, so
		// capturing them directly is safe. Before 1.22 every goroutine shared one
		// variable and this loop would have checked the LAST dependency N times --
		// the single most notorious bug in Go's history, which is why the language
		// changed the semantics.
		go func() {
			defer wg.Done()
			results[i] = a.runOne(ctx, c)
		}()
	}
	wg.Wait()

	overall := StatusUp
	for _, r := range results {
		if r.Status == StatusDown {
			overall = StatusDown
			break
		}
	}
	return Report{Status: overall, Checks: results}
}

// runOne executes a single check under its own timeout.
func (a *Aggregator) runOne(ctx context.Context, c Checker) CheckResult {
	// A child context per check. Cancelling it does not affect siblings, and the
	// deferred cancel releases the timer immediately when the check finishes early
	// -- omitting that defer leaks a timer per check, per probe, forever. `go vet`
	// flags the omission, which is why `make vet` is in the default check target.
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	// Monotonic elapsed time. time.Since uses the monotonic clock embedded in the
	// time.Time, so it stays correct across wall-clock adjustments (NTP steps, DST).
	// Subtracting two wall-clock readings can produce negative durations.
	start := time.Now()
	err := c.Check(ctx)
	result := CheckResult{
		Name:      c.Name(),
		Status:    StatusUp,
		LatencyMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		result.Status = StatusDown
		result.Error = err.Error()
	}
	return result
}
