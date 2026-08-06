package rollup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/logging"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// discardLogger silences the job's output.
//
// Built through internal/logging rather than slog directly, matching every other test package here.
// That is not only consistency: going through our own constructor means these tests exercise the same
// handler production uses, so a change that breaks logger construction fails a test rather than
// surfacing at runtime.
func discardLogger() *slog.Logger {
	return logging.New(logging.Options{Output: io.Discard})
}

// fakeStore is the whole reason Store is an interface.
//
// Proving "a failing day does not abort the run" against a real database means arranging for exactly
// the fourth day of eight to fail, which needs either a trigger, a constraint violation engineered into
// the data, or a proxy that drops a connection at the right moment. All three test the arrangement more
// than the code. Four methods on a struct tests the actual decision.
type fakeStore struct {
	// days is every day this fake "has facts for". DaysWithFacts FILTERS it by the requested range,
	// which is not incidental fidelity -- it is what makes the range assertions mean anything.
	//
	// An earlier version returned this list verbatim and ignored from/to. Every test still passed with
	// RollupYesterday deliberately changed to roll up TODAY, because the fake returned the same day
	// whatever range it was asked for. The test asserted on the fake's configuration rather than on the
	// code's behaviour. A stub that ignores its arguments cannot test anything that depends on them.
	days []postgres.Date
	// gotFrom and gotTo record the range that was asked for, so a test can assert on the REQUEST as
	// well as on the result.
	gotFrom, gotTo postgres.Date
	// failOn maps a day to the error it should return.
	failOn map[string]error
	// rolled records what was attempted, in order, so a test can assert the run CONTINUED past a
	// failure rather than merely that it reported one.
	rolled []string

	daysErr error

	generated   []postgres.Month
	genResult   postgres.GenerateResult
	genErr      error
	finalised   []postgres.Month
	finaliseN   int64
	finaliseErr error
}

func (f *fakeStore) RollupDay(_ context.Context, day postgres.Date) (postgres.DayResult, error) {
	f.rolled = append(f.rolled, day.String())
	if err, bad := f.failOn[day.String()]; bad {
		return postgres.DayResult{}, err
	}
	// Plausible figures so Compression() has something to divide.
	return postgres.DayResult{Day: day, FactRows: 10000, RollupRows: 32}, nil
}

func (f *fakeStore) DaysWithFacts(_ context.Context, from, to postgres.Date) ([]postgres.Date, error) {
	f.gotFrom, f.gotTo = from, to
	if f.daysErr != nil {
		return nil, f.daysErr
	}
	// Filtered, inclusive at both ends, exactly as the real query is.
	out := []postgres.Date{}
	for _, d := range f.days {
		if !d.Before(from) && !d.After(to) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeStore) GenerateMonth(_ context.Context, m postgres.Month) (postgres.GenerateResult, error) {
	f.generated = append(f.generated, m)
	return f.genResult, f.genErr
}

func (f *fakeStore) FinaliseMonth(_ context.Context, m postgres.Month, _ time.Time) (int64, error) {
	f.finalised = append(f.finalised, m)
	return f.finaliseN, f.finaliseErr
}

func days(strs ...string) []postgres.Date {
	out := make([]postgres.Date, 0, len(strs))
	for _, s := range strs {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			panic(err)
		}
		out = append(out, postgres.DayOf(t))
	}
	return out
}

// =============================================================================
// Failure isolation
// =============================================================================

// TestRollupRange_OneBadDayDoesNotStopTheRest is the behaviour that makes this job runnable unattended.
//
// Aborting on the first error means one corrupt day blocks every LATER day from ever being rolled up --
// and the later days are the ones somebody is waiting for, because they are the recent ones. A backfill
// that stops on 3 August and refuses to process the 4th through the 31st has turned one bad day into a
// month of missing data.
//
// Same decision internal/costing makes for a failing namespace, for the same reason: partial coverage
// that reports its gaps beats a total blank.
func TestRollupRange_OneBadDayDoesNotStopTheRest(t *testing.T) {
	t.Parallel()

	boom := errors.New("constraint violation on 2026-08-02")
	store := &fakeStore{
		days:   days("2026-08-01", "2026-08-02", "2026-08-03", "2026-08-04"),
		failOn: map[string]error{"2026-08-02": boom},
	}
	job := NewJob(store, discardLogger())

	res, err := job.RollupRange(context.Background(), postgres.Day(2026, 8, 1), postgres.Day(2026, 8, 4))
	if err != nil {
		t.Fatalf("RollupRange returned an error for a per-day failure: %v\n"+
			"A bad day is reported in Failures, not returned -- returning it would make the caller "+
			"unable to distinguish 'one day failed' from 'nothing ran'", err)
	}

	// EVERY day was attempted, including the three after the failure. Asserting on the attempt order
	// rather than only on the results, because "it reported a failure" and "it carried on" are
	// different claims and only the second one is what this test is about.
	want := []string{"2026-08-01", "2026-08-02", "2026-08-03", "2026-08-04"}
	if len(store.rolled) != len(want) {
		t.Fatalf("attempted %v, want all of %v: the run stopped at the failure", store.rolled, want)
	}
	for i, day := range want {
		if store.rolled[i] != day {
			t.Errorf("attempt %d was %s, want %s", i, store.rolled[i], day)
		}
	}

	if len(res.Days) != 3 {
		t.Errorf("succeeded on %d days, want 3", len(res.Days))
	}
	if !res.Failed() {
		t.Error("Failed() is false after a day failed; the caller would exit 0 on an incomplete rollup")
	}
	// The specific day AND the specific error, so a partial run says exactly what to re-run.
	if got := res.Failures["2026-08-02"]; !errors.Is(got, boom) {
		t.Errorf("Failures[2026-08-02] = %v, want the underlying error", got)
	}
	if len(res.Failures) != 1 {
		t.Errorf("Failures has %d entries, want 1", len(res.Failures))
	}
}

// TestRollupRange_CancellationStopsRatherThanRecordingFailures separates the two ways a run can end
// badly, which look identical if you only check for errors.
//
// A cancelled context is not a per-day failure. Treating it as one would try every remaining day
// against a dead context, record each as broken, and report "28 days failed" -- burying the single real
// cause under 28 symptoms. Worse, a caller reading Failures would conclude the data is corrupt when the
// truth is that a pod was evicted.
func TestRollupRange_CancellationStopsRatherThanRecordingFailures(t *testing.T) {
	t.Parallel()

	store := &fakeStore{
		days: days("2026-08-01", "2026-08-02", "2026-08-03", "2026-08-04"),
		failOn: map[string]error{
			// Wrapped, exactly as a real driver would return it, so the test proves errors.Is unwrapping
			// works rather than that a bare sentinel is comparable.
			"2026-08-02": errors.Join(errors.New("write rollup"), context.Canceled),
		},
	}
	job := NewJob(store, discardLogger())

	res, err := job.RollupRange(context.Background(), postgres.Day(2026, 8, 1), postgres.Day(2026, 8, 4))
	if err == nil {
		t.Fatal("cancellation was swallowed; a cancelled run must return an error so the caller " +
			"does not treat an interrupted rollup as a complete one")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled so callers can distinguish an "+
			"interruption from a data problem", err)
	}
	// STOPPED, not continued: two attempts, not four.
	if len(store.rolled) != 2 {
		t.Errorf("attempted %v, want to stop after the cancelled day", store.rolled)
	}
	// And the one day that DID succeed is still reported, because it was committed.
	if len(res.Days) != 1 {
		t.Errorf("res.Days has %d entries, want the 1 day that completed before cancellation", len(res.Days))
	}
}

// TestRollupRange_AlreadyCancelledContextDoesNothing checks the pre-loop guard.
//
// A rollup that starts after SIGTERM should do nothing rather than begin a transaction it cannot finish.
func TestRollupRange_AlreadyCancelledContextDoesNothing(t *testing.T) {
	t.Parallel()

	store := &fakeStore{days: days("2026-08-01", "2026-08-02")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewJob(store, discardLogger()).
		RollupRange(ctx, postgres.Day(2026, 8, 1), postgres.Day(2026, 8, 2))
	if err == nil {
		t.Fatal("a cancelled context produced no error")
	}
	if len(store.rolled) != 0 {
		t.Errorf("attempted %v with an already-cancelled context; nothing should start", store.rolled)
	}
}

// =============================================================================
// Which days
// =============================================================================

// TestRollupRange_OnlyTouchesDaysWithFacts is the decision that keeps a collection gap visible.
//
// Iterating the calendar would be the obvious implementation and it would write a zero-cost row for
// every day the collector was down. A day that cost nothing and a day that was never measured then look
// identical -- and on a chart, the second one silently becomes the first. The rollup leaves such days
// ABSENT so the gap survives into every view built on top of it.
func TestRollupRange_OnlyTouchesDaysWithFacts(t *testing.T) {
	t.Parallel()

	// A gap in the middle (the 2nd and 3rd have no facts) AND a day outside the range (the 9th), so
	// this proves both that gaps are skipped and that the range is respected.
	store := &fakeStore{days: days("2026-08-01", "2026-08-04", "2026-08-09")}
	job := NewJob(store, discardLogger())

	res, err := job.RollupRange(context.Background(), postgres.Day(2026, 8, 1), postgres.Day(2026, 8, 4))
	if err != nil {
		t.Fatalf("RollupRange: %v", err)
	}

	if len(store.rolled) != 2 {
		t.Errorf("attempted %v, want only the two days that have facts. Writing zero rows for the "+
			"gap would make 'the collector was down' indistinguishable from 'nothing ran that day'",
			store.rolled)
	}
	if len(res.Days) != 2 {
		t.Errorf("res.Days = %d, want 2", len(res.Days))
	}
}

// TestRollupRange_RejectsAReversedRange catches an operator typo before it silently does nothing.
func TestRollupRange_RejectsAReversedRange(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	_, err := NewJob(store, discardLogger()).
		RollupRange(context.Background(), postgres.Day(2026, 8, 4), postgres.Day(2026, 8, 1))
	if err == nil {
		t.Fatal("a reversed range was accepted; it would return no days and look like success")
	}
	if len(store.rolled) != 0 {
		t.Errorf("attempted %v on a reversed range", store.rolled)
	}
}

// TestRollupYesterday_UsesYesterdayNotToday pins the scheduling rule.
//
// Today is INCOMPLETE. Rolling it up writes a row correct for the hours so far and wrong for the day,
// and because the rollup is a projection the next run replaces it -- so the value flickers instead of
// converging. A day is rolled up once it can no longer change.
func TestRollupYesterday_UsesYesterdayNotToday(t *testing.T) {
	t.Parallel()

	// BOTH days have facts, so only the requested RANGE can decide which is rolled up. With just the
	// 4th present, the fake would return it for any range and the test would prove nothing.
	store := &fakeStore{days: days("2026-08-04", "2026-08-05")}
	job := NewJob(store, discardLogger())
	// Injected clock, so the rule is provable without waiting for midnight.
	job.now = func() time.Time { return time.Date(2026, 8, 5, 3, 15, 0, 0, time.UTC) }

	if _, err := job.RollupYesterday(context.Background()); err != nil {
		t.Fatalf("RollupYesterday: %v", err)
	}
	if len(store.rolled) != 1 || store.rolled[0] != "2026-08-04" {
		t.Errorf("rolled %v, want exactly [2026-08-04]: today is incomplete and rolling it up makes "+
			"the figure flicker until the day ends", store.rolled)
	}
	// And the RANGE asked for, which is the actual contract -- the result above only follows from it.
	if store.gotFrom.String() != "2026-08-04" || store.gotTo.String() != "2026-08-04" {
		t.Errorf("asked for [%s, %s], want exactly yesterday on both ends",
			store.gotFrom, store.gotTo)
	}
}

// TestRollupYesterday_CrossesAMonthBoundary checks the date arithmetic where it is easiest to get wrong.
func TestRollupYesterday_CrossesAMonthBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct{ now, want string }{
		{"2026-08-01T02:00:00Z", "2026-07-31"},
		{"2026-03-01T02:00:00Z", "2026-02-28"}, // 2026 is not a leap year
		{"2028-03-01T02:00:00Z", "2028-02-29"}, // 2028 is
		{"2027-01-01T02:00:00Z", "2026-12-31"}, // across a year
	}

	for _, tt := range tests {
		t.Run(tt.now, func(t *testing.T) {
			t.Parallel()
			now, err := time.Parse(time.RFC3339, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			// The target day AND the day either side, so only correct arithmetic selects the right one.
			target, perr := time.Parse("2006-01-02", tt.want)
			if perr != nil {
				t.Fatal(perr)
			}
			store := &fakeStore{days: []postgres.Date{
				postgres.DayOf(target.AddDate(0, 0, -1)),
				postgres.DayOf(target),
				postgres.DayOf(target.AddDate(0, 0, 1)),
			}}
			job := NewJob(store, discardLogger())
			job.now = func() time.Time { return now }

			if _, err := job.RollupYesterday(context.Background()); err != nil {
				t.Fatalf("RollupYesterday: %v", err)
			}
			if len(store.rolled) != 1 || store.rolled[0] != tt.want {
				t.Errorf("at %s rolled %v, want exactly [%s]", tt.now, store.rolled, tt.want)
			}
		})
	}
}

// TestRollupRange_ReportsCompression covers the number that justifies the phase.
//
// Stated every run rather than left to be rediscovered, and it doubles as the cheapest smoke test: a
// ratio near 1 means the grain is wrong or the fact table is nearly empty.
func TestRollupRange_ReportsCompression(t *testing.T) {
	t.Parallel()

	store := &fakeStore{days: days("2026-08-01", "2026-08-02")}
	res, err := NewJob(store, discardLogger()).
		RollupRange(context.Background(), postgres.Day(2026, 8, 1), postgres.Day(2026, 8, 2))
	if err != nil {
		t.Fatalf("RollupRange: %v", err)
	}

	// 2 days x 10,000 fact rows -> 2 x 32 rollup rows.
	if res.FactRows != 20000 || res.RollupRows != 64 {
		t.Fatalf("FactRows=%d RollupRows=%d, want 20000 and 64", res.FactRows, res.RollupRows)
	}
	if got := res.Compression(); got < 312 || got > 313 {
		t.Errorf("Compression() = %.1f, want 312.5", got)
	}

	// Zero rather than a division by zero when nothing was written. +Inf in a log line is a crash
	// waiting for whatever parses it.
	if got := (RangeResult{}).Compression(); got != 0 {
		t.Errorf("Compression() on an empty result = %v, want 0", got)
	}
}

// =============================================================================
// Months
// =============================================================================

// TestRunMonth_DoesNotFinaliseUnlessAsked pins the default.
//
// Finalising is irreversible without a deliberate un-finalise, so it must never be a side effect of
// generating. Generation is idempotent and safe to run hourly; finalisation is a one-way decision.
func TestRunMonth_DoesNotFinaliseUnlessAsked(t *testing.T) {
	t.Parallel()

	store := &fakeStore{genResult: postgres.GenerateResult{Written: 10}}
	job := NewJob(store, discardLogger())

	if _, err := job.RunMonth(context.Background(), postgres.NewMonth(2026, 7), false); err != nil {
		t.Fatalf("RunMonth: %v", err)
	}
	if len(store.generated) != 1 {
		t.Errorf("generated %v, want one month", store.generated)
	}
	if len(store.finalised) != 0 {
		t.Errorf("finalised %v without being asked; freezing is irreversible and must be explicit",
			store.finalised)
	}
}

// TestRunMonth_FinalisesWhenAsked is the other half.
func TestRunMonth_FinalisesWhenAsked(t *testing.T) {
	t.Parallel()

	store := &fakeStore{genResult: postgres.GenerateResult{Written: 10}, finaliseN: 10}
	res, err := NewJob(store, discardLogger()).
		RunMonth(context.Background(), postgres.NewMonth(2026, 7), true)
	if err != nil {
		t.Fatalf("RunMonth: %v", err)
	}
	if len(store.finalised) != 1 {
		t.Fatalf("finalised %v, want one month", store.finalised)
	}
	if res.Finalised != 10 {
		t.Errorf("res.Finalised = %d, want 10", res.Finalised)
	}
}

// TestRunMonth_GenerationFailureDoesNotFinalise checks the ordering.
//
// Finalising statements that failed to generate would freeze whatever stale figures were already there,
// which is the one outcome worse than not generating at all: wrong AND immutable.
func TestRunMonth_GenerationFailureDoesNotFinalise(t *testing.T) {
	t.Parallel()

	store := &fakeStore{genErr: errors.New("rollup table is empty")}
	_, err := NewJob(store, discardLogger()).
		RunMonth(context.Background(), postgres.NewMonth(2026, 7), true)
	if err == nil {
		t.Fatal("a generation failure was swallowed")
	}
	if len(store.finalised) != 0 {
		t.Error("finalised after generation failed; that would freeze stale figures permanently")
	}
}

// TestRunMonth_ReportsSkippedFinalised covers the field that distinguishes two very different runs.
//
// "Wrote 12 statements" and "wrote 12 and refused to touch 28 that were already signed off" are not the
// same outcome, and a caller that cannot tell them apart cannot tell whether its regeneration did
// anything at all.
func TestRunMonth_ReportsSkippedFinalised(t *testing.T) {
	t.Parallel()

	store := &fakeStore{genResult: postgres.GenerateResult{Written: 0, SkippedFinalised: 28}}
	res, err := NewJob(store, discardLogger()).
		RunMonth(context.Background(), postgres.NewMonth(2026, 7), false)
	if err != nil {
		t.Fatalf("RunMonth: %v", err)
	}
	if res.Generated.SkippedFinalised != 28 {
		t.Errorf("SkippedFinalised = %d, want 28", res.Generated.SkippedFinalised)
	}
	if res.Generated.Written != 0 {
		t.Errorf("Written = %d, want 0: every statement was already frozen", res.Generated.Written)
	}
}
