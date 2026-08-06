// Command rollup pre-aggregates the fact table into daily rows and monthly statements.
//
// WHY A THIRD BINARY, AND NOT A TICKER INSIDE cmd/collector
// --------------------------------------------------------
// The collector already has a database connection and a periodic loop, so adding a rollup to it is
// three lines. It is still the wrong shape, for reasons that are about lifecycle rather than tidiness:
//
//   - A BATCH JOB COMPLETES. It has an exit code, and "did last night's rollup succeed" is answerable
//     by looking at it. A goroutine inside a long-running service has no exit code, so its failures
//     are visible only if somebody reads the logs, and its success is not visible at all.
//   - BACKFILL IS A CLI INVOCATION. "Roll up July" is a thing an operator types once, with arguments.
//     A daemon cannot be asked that, so a daemon-hosted rollup grows a second code path for backfill
//     -- and two code paths for the same computation eventually disagree, at which point the backfill
//     produces different numbers from the nightly run.
//   - FAILURE DOMAINS SHOULD NOT BE SHARED. A rollup that exhausts memory aggregating a huge month
//     must not stop collection, because collection is the thing that cannot be caught up: Prometheus
//     retention expires and that data is gone forever, whereas a rollup can be recomputed from facts
//     at any time. The recoverable job must not be able to kill the unrecoverable one.
//   - SCHEDULE, NOT INTERVAL. This should run shortly after midnight UTC, which is a cron expression.
//     A ticker inside a service runs every N hours from whenever the pod happened to start, so the
//     rollup drifts across the day and eventually straddles the midnight boundary it exists to
//     respect.
//
// Phase 10 runs this as a Kubernetes CronJob. Until then it is a Makefile target.
//
// USAGE
//
//	rollup                       # yesterday (the scheduled default)
//	rollup -from 2026-07-01 -to 2026-08-04
//	rollup -all                  # every day that has fact rows
//	rollup -month 2026-07        # roll up July's days, then write July's statements
//	rollup -month 2026-07 -close # ... and freeze them
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/config"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/logging"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/rollup"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// earliestBackfill bounds -all.
//
// Not unbounded, because -all with an empty fact table and a bug in the date arithmetic would iterate
// from the zero time and issue two million no-op statements before anybody noticed. The real lower
// bound comes from DaysWithFacts, so this only ever caps a pathological case.
const earliestBackfill = "2020-01-01"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	from  string
	to    string
	all   bool
	month string
	close bool
	// closePreviousMonth is the scheduled monthly close: work out the previous month, roll up its days,
	// generate its statements and freeze them.
	//
	// WHY THE BINARY COMPUTES THE MONTH RATHER THAN THE CALLER
	// ------------------------------------------------------
	// The obvious CronJob arg is `-month $(date -d 'last month' +%Y-%m)`, and it fails twice.
	//
	// These images are distroless-static: no shell, no coreutils, no `date`. A subshell in the args would
	// need a shell in the image, which is exactly the attack surface distroless removes -- so the
	// convenience of `$( )` would cost the property that a compromised container cannot run anything.
	//
	// And `date -d 'last month'` is WRONG on the 31st. On 31 March it yields 3 March, because "one month
	// before 31 March" is not a date and GNU date normalises the overflow forwards. So a monthly close
	// scheduled for the 31st would silently close the wrong month in several months of the year -- and a
	// finalised statement is immutable by database trigger, so that mistake needs a deliberate
	// un-finalise to correct.
	//
	// Computing it here means the arg is a constant, the image needs no shell, and the calendar
	// arithmetic is the same tested code the CLI uses.
	closePreviousMonth bool

	// version short-circuits everything else. Registered as a real flag here, rather than
	// sniffed out of os.Args as it would have to be in a binary with no flag set, so that it
	// appears in the usage text `flag` generates for -h.
	version bool
}

func parseFlags() options {
	var o options
	flag.BoolVar(&o.version, "version", false, "print build information and exit")
	flag.StringVar(&o.from, "from", "", "first day to roll up, YYYY-MM-DD (default: yesterday)")
	flag.StringVar(&o.to, "to", "", "last day to roll up, inclusive, YYYY-MM-DD (default: same as -from)")
	flag.BoolVar(&o.all, "all", false, "roll up every day that has fact rows")
	flag.StringVar(&o.month, "month", "", "also write monthly statements for this month, YYYY-MM")
	// Named -close rather than -finalise so a mistyped flag cannot silently be interpreted as
	// something else, and because the verb describes what happens to the statement.
	flag.BoolVar(&o.close, "close", false,
		"freeze the -month statements. Requires the month to have ENDED. This is irreversible without a deliberate un-finalise")
	flag.BoolVar(&o.closePreviousMonth, "close-previous-month", false,
		"roll up the PREVIOUS calendar month, write its statements and freeze them. What the monthly CronJob runs; needs no date argument, so the image needs no shell")
	flag.Parse()
	return o
}

func run() error {
	opts := parseFlags()

	// Checked here, immediately after parsing and BEFORE config.Load on the next line. That one
	// line of ordering is the whole fix: this binary runs as a CronJob, so the pod that needs
	// identifying is often one that failed before it could reach a database.
	if opts.version {
		buildinfo.PrintVersionAndExit()
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	log := logging.New(logging.Options{
		Level:     cfg.LogLevel,
		JSON:      cfg.IsProduction(),
		AddSource: !cfg.IsProduction(),
		Output:    os.Stderr,
		Attrs:     append([]any{"service", "rollup"}, buildinfo.LogAttrs()...),
	})
	log.Info("starting", "build", buildinfo.String(), "env", cfg.Env)

	// SIGTERM is handled even though this is a batch job, because Kubernetes sends it: a CronJob's
	// activeDeadlineSeconds expiring, a node draining, or `kubectl delete job` all terminate a
	// running rollup. Stopping at a committed day boundary rather than mid-transaction is what makes
	// the interrupted run safe to simply re-run.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := postgres.New(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("create database pool: %w", err)
	}
	defer db.Close()

	// Fail fast on an unreachable database, unlike cmd/api.
	//
	// The opposite choice from the API, deliberately. The API must start without a database so a blip
	// during a rollout does not CrashLoopBackOff every new pod -- it has readiness probes to express
	// "up but not ready". A batch job has no such state: it either does the work or it does not, and
	// a job that exits 0 having silently skipped everything is indistinguishable from success. Exiting
	// non-zero immediately is the only honest outcome, and it is what makes a CronJob's failure count
	// mean something.
	pingCtx, cancelPing := context.WithTimeout(ctx, 10*time.Second)
	defer cancelPing()
	if pingErr := db.Check(pingCtx); pingErr != nil {
		return fmt.Errorf("database unreachable: %w", pingErr)
	}

	// THE LOCK, taken once for the whole run and held across every day's transaction.
	//
	// A contended run exits ZERO. Another process is already doing this work, so there is nothing
	// wrong and nothing to alert on -- and a CronJob whose failure count increments every time it
	// overlaps with a long backfill is a CronJob whose failure count means nothing.
	lock, err := postgres.TryAdvisoryLock(ctx, db.Pool(), postgres.LockRollup)
	if err != nil {
		if errors.Is(err, postgres.ErrLockHeld) {
			log.Info("another rollup holds the lock, exiting successfully — the holder is doing this work")
			return nil
		}
		return fmt.Errorf("take rollup lock: %w", err)
	}
	defer lock.Release()

	store := postgres.NewTxRollupStore(db.Pool())
	job := rollup.NewJob(store, log)

	// The clock is passed in rather than read inside, so the branch that decides WHICH DAY a scheduled
	// run processes is testable. UTC, because the rollup grain is a UTC calendar day.
	// -close-previous-month resolves to the equivalent explicit flags before anything else runs.
	//
	// Rewritten into `-month <prev> -close` rather than handled as a separate code path, so the scheduled
	// job and a human typing the flags by hand take the SAME path through resolveRange, RollupRange and
	// RunMonth. Two code paths for one operation is the failure cmd/rollup's own doc comment argues
	// against, and it is how a backfill ends up disagreeing with a nightly run.
	if opts.closePreviousMonth {
		prev := postgres.MonthOf(time.Now().UTC()).Previous()
		opts.month = prev.String()
		opts.close = true
		log.Info("closing the previous month", slog.String("month", opts.month))
	}

	from, to, err := resolveRange(opts, time.Now().UTC())
	if err != nil {
		return err
	}

	started := time.Now()
	res, err := job.RollupRange(ctx, from, to)
	// Reported even on error, because a cancelled or partly-failed run still committed whatever it
	// finished, and knowing how far it got is the difference between re-running the remainder and
	// re-running everything.
	report(log, res, time.Since(started))
	if err != nil {
		return err
	}

	if opts.month != "" {
		month, perr := postgres.ParseMonth(opts.month)
		if perr != nil {
			return perr
		}
		mres, merr := job.RunMonth(ctx, month, opts.close)
		if merr != nil {
			return merr
		}
		log.Info("month complete",
			slog.String("month", mres.Month.String()),
			slog.Int64("statements_written", mres.Generated.Written),
			slog.Int64("left_finalised", mres.Generated.SkippedFinalised),
			slog.Int64("newly_frozen", mres.Finalised))
	}

	// A NON-ZERO EXIT FOR A PARTIAL RUN, after doing all the work that could be done.
	//
	// The two obvious alternatives are both wrong. Exiting 0 makes a silently incomplete rollup look
	// like a success, and the missing days are then never noticed until a chart has a hole in it.
	// Exiting at the first failure abandons days that would have succeeded, so a single bad day
	// blocks every later one indefinitely. Do everything possible, then report failure.
	if res.Failed() {
		return fmt.Errorf("%d of %d days failed: %v",
			len(res.Failures), len(res.Failures)+len(res.Days), res.Failures)
	}
	return nil
}

// resolveRange turns the flags into a day range.
//
// The precedence is -all, then -month, then -from/-to, then yesterday. Ordered from widest to
// narrowest so a combination cannot silently do less than the operator asked for: -all -from X does
// everything rather than one day.
//
// TAKES `now` RATHER THAN CALLING time.Now, and an audit is the reason.
//
// This function is the entry point for every invocation of this binary and has five branches, and it
// had no tests -- because two branches called time.Now directly, so testing "the default is yesterday"
// meant changing the system clock. Meanwhile internal/rollup had a RollupYesterday method with an
// injectable clock and two thorough tests, and NOTHING CALLED IT: main computed yesterday itself, here.
//
// So the tested code was not the code that ran, and the code that ran was untested. That is worse than
// having neither, because the green test suite was evidence about a path production never takes. It was
// also precisely the failure this binary's own doc comment argues against -- two code paths for one
// computation, which eventually disagree.
//
// RollupYesterday is deleted and its tests moved here, onto the path that actually executes.
func resolveRange(opts options, now time.Time) (from, to postgres.Date, err error) {
	switch {
	case opts.all:
		earliest, perr := time.Parse("2006-01-02", earliestBackfill)
		if perr != nil {
			return from, to, fmt.Errorf("parse earliestBackfill: %w", perr)
		}
		// Today as the upper bound rather than yesterday: -all is an explicit catch-up, so including a
		// partial today is what the operator asked for. The nightly run will replace it tomorrow,
		// which is safe precisely because the rollup is a projection rather than an accumulation.
		return postgres.DayOf(earliest), postgres.DayOf(now), nil

	case opts.month != "":
		month, perr := postgres.ParseMonth(opts.month)
		if perr != nil {
			return from, to, perr
		}
		// The month's days, ending on its last day -- Next() minus one, so February needs no special
		// case and neither does a leap year.
		return postgres.DayOf(month.Time()), postgres.DayOf(month.Next().Time()).AddDays(-1), nil

	case opts.from != "":
		f, perr := parseDay(opts.from, "from")
		if perr != nil {
			return from, to, perr
		}
		if opts.to == "" {
			return f, f, nil
		}
		t, perr := parseDay(opts.to, "to")
		if perr != nil {
			return from, to, perr
		}
		return f, t, nil

	case opts.to != "":
		return from, to, errors.New("-to requires -from")

	default:
		// YESTERDAY, NEVER TODAY. The scheduled default, and the only complete day when a nightly run
		// fires. Rolling up a day still in progress writes a figure correct for the hours so far and
		// wrong for the day -- and because the rollup is a projection the next run replaces it, so the
		// value flickers instead of converging.
		y := postgres.DayOf(now).AddDays(-1)
		return y, y, nil
	}
}

// parseDay reads a YYYY-MM-DD flag.
func parseDay(s, flagName string) (postgres.Date, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return postgres.Date{}, fmt.Errorf("-%s must be YYYY-MM-DD, got %q", flagName, s)
	}
	// Parsed as UTC by time.Parse when no zone is given, which is exactly the grain the rollup uses.
	return postgres.DayOf(t), nil
}

// report logs the run's headline numbers.
//
// The compression ratio is stated every run rather than left to be rediscovered. It is the number that
// justifies this entire phase, and it is also the fastest possible smoke test: a ratio near 1 means the
// grain is wrong or the fact table is nearly empty, and either is worth noticing immediately.
func report(log *slog.Logger, res rollup.RangeResult, took time.Duration) {
	if len(res.Days) == 0 && !res.Failed() {
		return
	}
	log.Info("rollup finished",
		slog.Int("days", len(res.Days)),
		slog.Int("failed_days", len(res.Failures)),
		slog.Int64("fact_rows_read", res.FactRows),
		slog.Int64("rollup_rows_written", res.RollupRows),
		slog.String("compression", fmt.Sprintf("%.1fx", res.Compression())),
		slog.Duration("took", took))
}
