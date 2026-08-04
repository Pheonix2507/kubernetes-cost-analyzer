package health

import (
	"context"
	"errors"
	"testing"
	"time"
)

// stubChecker is a hand-written test double.
//
// WHY NO MOCKING LIBRARY: Checker has two methods. A stub is eight lines, reads
// plainly, and needs no code generation or DSL. Mocking frameworks earn their keep
// against wide interfaces with call-order expectations; reaching for one here would
// add a dependency and make the test harder to read than the code it tests.
//
// This is also the payoff for defining Checker as a small interface in the consuming
// package: these tests need no database, no network and no Docker.
type stubChecker struct {
	name  string
	err   error
	delay time.Duration
}

func (s stubChecker) Name() string { return s.name }

func (s stubChecker) Check(ctx context.Context) error {
	if s.delay > 0 {
		// Honour cancellation while "working". A real check must do the same, which
		// is why the stub models it: select on both the timer and ctx.Done().
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.err
}

func TestAggregator_AllUp(t *testing.T) {
	t.Parallel()

	agg := NewAggregator(time.Second,
		stubChecker{name: "postgres"},
		stubChecker{name: "prometheus"},
	)

	report := agg.Run(context.Background())

	if report.Status != StatusUp {
		t.Errorf("Status = %q, want %q", report.Status, StatusUp)
	}
	if len(report.Checks) != 2 {
		t.Fatalf("got %d checks, want 2", len(report.Checks))
	}
	for _, c := range report.Checks {
		if c.Status != StatusUp {
			t.Errorf("check %q status = %q, want %q", c.Name, c.Status, StatusUp)
		}
		if c.Error != "" {
			t.Errorf("check %q carried an error %q, want empty", c.Name, c.Error)
		}
	}
}

// TestAggregator_OneDownFailsOverall pins the "readiness is not a democracy" rule:
// a single failing dependency must take the whole replica out of service.
func TestAggregator_OneDownFailsOverall(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("connection refused")
	agg := NewAggregator(time.Second,
		stubChecker{name: "postgres", err: wantErr},
		stubChecker{name: "prometheus"},
	)

	report := agg.Run(context.Background())

	if report.Status != StatusDown {
		t.Errorf("Status = %q, want %q when one dependency is down", report.Status, StatusDown)
	}

	// The healthy dependency must still be reported as healthy. A readiness endpoint
	// that says only "something is broken" makes an on-call engineer go hunting;
	// one that names the failing dependency ends the investigation immediately.
	byName := map[string]CheckResult{}
	for _, c := range report.Checks {
		byName[c.Name] = c
	}
	if got := byName["postgres"]; got.Status != StatusDown || got.Error != wantErr.Error() {
		t.Errorf("postgres = %+v, want status down with the error message", got)
	}
	if got := byName["prometheus"]; got.Status != StatusUp {
		t.Errorf("prometheus = %+v, want status up", got)
	}
}

// TestAggregator_SlowCheckTimesOut proves the per-check deadline is enforced, so one
// hanging dependency cannot stall the probe indefinitely.
func TestAggregator_SlowCheckTimesOut(t *testing.T) {
	t.Parallel()

	agg := NewAggregator(50*time.Millisecond,
		stubChecker{name: "slow", delay: 5 * time.Second},
	)

	start := time.Now()
	report := agg.Run(context.Background())
	elapsed := time.Since(start)

	if report.Status != StatusDown {
		t.Errorf("Status = %q, want %q for a timed-out check", report.Status, StatusDown)
	}
	// Generous upper bound: we are asserting "the timeout was enforced", not
	// benchmarking the scheduler. A tight assertion here would flake on a loaded CI
	// machine, and a flaky test is worse than no test.
	if elapsed > 2*time.Second {
		t.Errorf("Run took %v, want it bounded by the 50ms per-check timeout", elapsed)
	}
}

// TestAggregator_ChecksRunConcurrently is the test that would fail if someone
// "simplified" Run into a serial loop.
//
// Three checks, each sleeping 200ms. Serially that is 600ms; concurrently it is
// ~200ms. Asserting under 450ms distinguishes the two with a wide margin either
// side, so it is decisive without being flaky.
func TestAggregator_ChecksRunConcurrently(t *testing.T) {
	t.Parallel()

	const delay = 200 * time.Millisecond
	agg := NewAggregator(2*time.Second,
		stubChecker{name: "a", delay: delay},
		stubChecker{name: "b", delay: delay},
		stubChecker{name: "c", delay: delay},
	)

	start := time.Now()
	report := agg.Run(context.Background())
	elapsed := time.Since(start)

	if report.Status != StatusUp {
		t.Fatalf("Status = %q, want %q", report.Status, StatusUp)
	}
	if elapsed > 450*time.Millisecond {
		t.Errorf("Run took %v for 3x%v checks; they appear to be running serially", elapsed, delay)
	}
}

// TestAggregator_ResultsMatchCheckerOrder guards the index-write concurrency design.
//
// Each goroutine writes results[i] for its own i, so output order must match input
// order deterministically. If someone replaces that with `append`, this test starts
// flaking AND `go test -race` reports a data race -- which is exactly the pair of
// symptoms that teaches why the index-write pattern was chosen.
func TestAggregator_ResultsMatchCheckerOrder(t *testing.T) {
	t.Parallel()

	// Descending delays: if results were appended in completion order, "slowest"
	// would come last rather than first.
	agg := NewAggregator(time.Second,
		stubChecker{name: "slowest", delay: 60 * time.Millisecond},
		stubChecker{name: "middle", delay: 30 * time.Millisecond},
		stubChecker{name: "fastest"},
	)

	report := agg.Run(context.Background())

	want := []string{"slowest", "middle", "fastest"}
	if len(report.Checks) != len(want) {
		t.Fatalf("got %d checks, want %d", len(report.Checks), len(want))
	}
	for i, name := range want {
		if report.Checks[i].Name != name {
			t.Errorf("Checks[%d].Name = %q, want %q (order must follow the checker list)",
				i, report.Checks[i].Name, name)
		}
	}
}

// TestAggregator_NoCheckers covers the collector binary's case: nothing to check yet.
// It must report up, and must return an empty slice rather than nil so the JSON
// response carries `[]` instead of `null`.
func TestAggregator_NoCheckers(t *testing.T) {
	t.Parallel()

	report := NewAggregator(time.Second).Run(context.Background())

	if report.Status != StatusUp {
		t.Errorf("Status = %q, want %q with no checkers", report.Status, StatusUp)
	}
	if report.Checks == nil {
		t.Error("Checks is nil; want an empty slice so it marshals as [] not null")
	}
	if len(report.Checks) != 0 {
		t.Errorf("got %d checks, want 0", len(report.Checks))
	}
}
