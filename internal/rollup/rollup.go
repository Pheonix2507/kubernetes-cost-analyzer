// Package rollup orchestrates the batch job that pre-aggregates the fact table.
//
// WHY THIS PACKAGE EXISTS SEPARATELY FROM THE REPOSITORY
// -----------------------------------------------------
// internal/store/postgres holds the SQL: what a day's aggregate IS. This package holds the decisions
// around running it, which are a different kind of question entirely:
//
//   - what is the transaction boundary, and what does a partial failure leave behind
//   - which days should be processed when nobody said
//   - does one bad day abort the run or get reported and skipped
//   - who holds the lock, and for how long
//
// None of those are expressible in SQL, and all of them are the difference between a batch job you
// can run unattended and one that needs a human watching it. Separating them also means they are
// testable against a fake store, so the failure-isolation behaviour can be proven without arranging
// for a real database to fail on the fourth day of a range.
//
// HOW IT COMMUNICATES WITH THE REST OF THE APPLICATION
// It talks to Postgres only, through the Store interface below, and it is driven by cmd/rollup. It
// never touches Kubernetes or Prometheus: by the time a rollup runs, the facts are already recorded,
// and reaching back to a live cluster would make history depend on the present.
package rollup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// Store is the persistence this job needs.
//
// AN INTERFACE DEFINED HERE, BY THE CONSUMER, listing only the four operations this package calls --
// not the twenty the repository offers. That is the dependency-inversion direction that actually pays:
// the fake in the tests implements four methods rather than a whole repository, and adding a method to
// RollupRepository cannot break this package or its tests.
type Store interface {
	// RollupDay must be atomic: it deletes a day and reinserts it, and a reader between the two
	// would see the day as having no cost. The implementation supplies that transaction.
	RollupDay(ctx context.Context, day postgres.Date) (postgres.DayResult, error)
	// DaysWithFacts is what makes the job self-describing rather than needing to be told the range.
	DaysWithFacts(ctx context.Context, from, to postgres.Date) ([]postgres.Date, error)
	GenerateMonth(ctx context.Context, m postgres.Month) (postgres.GenerateResult, error)
	FinaliseMonth(ctx context.Context, m postgres.Month, now time.Time) (int64, error)
}

// Job runs the rollup.
type Job struct {
	store Store
	log   *slog.Logger
	// now is injectable so the month-completeness guard is testable without waiting for a month to
	// end. The alternative -- calling time.Now inside FinaliseMonth -- would make that rule provable
	// only by changing the system clock.
	now func() time.Time
}

// NewJob builds a Job.
func NewJob(store Store, log *slog.Logger) *Job {
	return &Job{store: store, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// RangeResult reports a whole run.
type RangeResult struct {
	Days       []postgres.DayResult
	FactRows   int64
	RollupRows int64

	// Failures maps a day to the error that stopped it, so a partial run reports exactly what is
	// missing rather than only that something went wrong.
	Failures map[string]error
}

// Compression is the ratio achieved, or zero when nothing was read.
//
// Returned as a float and only used for reporting. It is the number that justifies the whole phase,
// so the job states it every run rather than leaving it to be rediscovered.
func (r RangeResult) Compression() float64 {
	if r.RollupRows == 0 {
		return 0
	}
	return float64(r.FactRows) / float64(r.RollupRows)
}

// Failed reports whether any day failed.
func (r RangeResult) Failed() bool { return len(r.Failures) > 0 }

// RollupRange rolls up every day that has fact rows in [from, to], inclusive.
//
// ONE TRANSACTION PER DAY, NOT ONE FOR THE RANGE
// ---------------------------------------------
// A single transaction spanning a 60-day backfill has three problems, in increasing severity: it holds
// locks for minutes, it accumulates a rollback segment proportional to the whole range, and a failure
// on day 40 discards the 39 days that succeeded. The third is the one that matters -- it turns a
// partially-recoverable job into an all-or-nothing one, so a transient error means starting over.
//
// Per-day transactions make each day independently durable. The unit of work matches the unit of
// data, which is the property that lets this be re-run safely after any failure: the days that
// succeeded are done, and re-running them is a no-op by construction because RollupDay is a
// projection rather than an accumulation.
//
// A FAILING DAY IS RECORDED AND THE RUN CONTINUES, which is the same decision internal/costing makes
// for a failing namespace. Aborting on the first error means one corrupt day blocks every later day
// from ever being rolled up, and the later days are usually the ones somebody is waiting for. Partial
// coverage that reports its gaps beats a total blank -- but it must REPORT them, which is what
// Failures is for and why the caller is expected to exit non-zero on it.
func (j *Job) RollupRange(ctx context.Context, from, to postgres.Date) (RangeResult, error) {
	res := RangeResult{Failures: map[string]error{}}

	if to.Before(from) {
		return res, fmt.Errorf("range is reversed: from %s is after to %s", from, to)
	}

	// Ask which days actually have data rather than iterating the calendar.
	//
	// Two reasons. A calendar loop over a range that predates collection would run hundreds of
	// no-op rollups, each a DELETE and an INSERT that touch nothing. And more importantly, a day
	// with no facts is left ALONE rather than written as a zero -- so a genuine gap in collection
	// stays visible as a missing day instead of being recorded as a day that cost nothing. Those two
	// look identical on a chart and mean completely different things.
	days, err := j.store.DaysWithFacts(ctx, from, to)
	if err != nil {
		return res, fmt.Errorf("determine which days to roll up: %w", err)
	}
	if len(days) == 0 {
		j.log.Info("no fact rows in range, nothing to roll up",
			slog.String("from", from.String()), slog.String("to", to.String()))
		return res, nil
	}

	j.log.Info("rolling up",
		slog.String("from", from.String()), slog.String("to", to.String()),
		slog.Int("days_with_data", len(days)))

	for _, day := range days {
		// Checked before each day rather than relying on the query to notice. A SIGTERM mid-backfill
		// should stop between days -- at a committed boundary -- rather than have the next day's
		// transaction start and be cancelled halfway through its INSERT.
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("cancelled after %d of %d days: %w", len(res.Days), len(days), err)
		}

		dayRes, err := j.store.RollupDay(ctx, day)
		if err != nil {
			// Cancellation is not a per-day failure; it means stop. Recording it in Failures and
			// continuing would try every remaining day against a dead context and report each as
			// broken, burying the one real cause under a list of symptoms.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return res, fmt.Errorf("cancelled during %s: %w", day, err)
			}
			res.Failures[day.String()] = err
			j.log.Error("day failed, continuing",
				slog.String("day", day.String()), slog.Any("error", err))
			continue
		}

		res.Days = append(res.Days, dayRes)
		res.FactRows += dayRes.FactRows
		res.RollupRows += dayRes.RollupRows

		attrs := []any{
			slog.String("day", day.String()),
			slog.Int64("fact_rows", dayRes.FactRows),
			slog.Int64("rollup_rows", dayRes.RollupRows),
		}
		if dayRes.Deleted > 0 {
			// A re-run. Logged explicitly because a SCHEDULED job re-doing a day it already did is
			// usually a scheduling bug -- a cron firing twice, or a lookback window that overlaps --
			// whereas a backfill doing it is entirely expected. The log makes the two distinguishable
			// without reading the invocation.
			attrs = append(attrs, slog.Int64("replaced_rows", dayRes.Deleted))
		}
		j.log.Info("day rolled up", attrs...)
	}

	return res, nil
}

// RollupYesterday rolls up the previous complete UTC day. The default for a scheduled run.
//
// YESTERDAY, NOT TODAY, and the reason is the same one that made the collector's windows aligned:
// today is incomplete. Rolling up a day still in progress writes a row that is correct for the hours
// so far and wrong for the day, and since the rollup is a projection the next run would replace it --
// so the value flickers rather than converging. A day is rolled up once it can no longer change.
//
// This does mean cost is visible in the rollup only after the day closes. The trend endpoint covers
// the gap by routing hourly queries to the fact table, which is exactly what that routing is for.
func (j *Job) RollupYesterday(ctx context.Context) (RangeResult, error) {
	yesterday := postgres.DayOf(j.now()).AddDays(-1)
	return j.RollupRange(ctx, yesterday, yesterday)
}

// MonthResult reports monthly statement generation.
type MonthResult struct {
	Month     postgres.Month
	Generated postgres.GenerateResult
	// Finalised is how many statements were closed, and is zero unless finalisation was asked for.
	Finalised int64
}

// RunMonth generates a month's statements, optionally closing them.
//
// The rollup for the month's days must already exist -- this reads the daily rollup, not the facts.
// Callers that want both should roll up the range first; the CLI does exactly that, in that order,
// because generating a statement from days that have not been rolled up yet would produce a
// confidently incomplete number with a coverage figure that says so only if someone reads it.
func (j *Job) RunMonth(ctx context.Context, m postgres.Month, finalise bool) (MonthResult, error) {
	res := MonthResult{Month: m}

	gen, err := j.store.GenerateMonth(ctx, m)
	if err != nil {
		return res, fmt.Errorf("generate statements for %s: %w", m, err)
	}
	res.Generated = gen

	j.log.Info("monthly statements generated",
		slog.String("month", m.String()),
		slog.Int64("written", gen.Written),
		slog.Int64("skipped_finalised", gen.SkippedFinalised))

	if !finalise {
		return res, nil
	}

	// The completeness guard lives in the repository, not here, so it holds for every caller rather
	// than only for the one that goes through this package.
	closed, err := j.store.FinaliseMonth(ctx, m, j.now())
	if err != nil {
		return res, fmt.Errorf("finalise %s: %w", m, err)
	}
	res.Finalised = closed

	j.log.Info("monthly statements finalised — these are now immutable",
		slog.String("month", m.String()), slog.Int64("closed", closed))
	return res, nil
}
