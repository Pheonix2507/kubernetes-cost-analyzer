package postgres

import (
	"context"
	"errors"
	"testing"
	"time"
)

// These tests use the POOL directly rather than withTx, and that is not an oversight.
//
// An advisory lock is scoped to a SESSION, so testing it inside a single rolled-back transaction would
// prove nothing: there would be one session, and mutual exclusion between one thing and itself is
// trivially true. Two independent pool connections are the whole point.
//
// Nothing here writes a row, so there is nothing to roll back. The locks are released on every path,
// and any that were not would die with the pool when the test binary exits.

// testLockKey is deliberately NOT LockRollup.
//
// Using the production key would make these tests contend with a real rollup running in another terminal
// -- so the suite would fail depending on what else the developer happened to be doing, which is the
// least debuggable kind of flake. A key in its own range keeps the test isolated from the system it
// tests.
const testLockKey AdvisoryLockKey = 999_000_001

func requirePool(t *testing.T) {
	t.Helper()
	requireDB(t)
}

// TestAdvisoryLock_ExcludesASecondHolder is the property the rollup depends on.
//
// Without it, two concurrent rollups both DELETE a day and both INSERT it, and depending on the
// interleaving the day ends up duplicated or the transactions deadlock.
func TestAdvisoryLock_ExcludesASecondHolder(t *testing.T) {
	requirePool(t)
	ctx := context.Background()

	first, err := TryAdvisoryLock(ctx, testPool, testLockKey)
	if err != nil {
		t.Fatalf("first TryAdvisoryLock: %v", err)
	}
	defer first.Release()

	// A second attempt must be REFUSED, not queued. Non-blocking is what stops a five-minute cron
	// accumulating waiting processes behind a six-minute job until the pool is exhausted.
	second, err := TryAdvisoryLock(ctx, testPool, testLockKey)
	if err == nil {
		second.Release()
		t.Fatal("a second holder acquired the same lock; two concurrent rollups would both rewrite " +
			"the same day")
	}
	if !errors.Is(err, ErrLockHeld) {
		t.Errorf("error = %v, want ErrLockHeld. The caller branches on this to exit ZERO rather than "+
			"alerting -- a contended run is the system working, not failing", err)
	}
}

// TestAdvisoryLock_ReleaseActuallyReleases is the test that would have caught the pooling bug the
// implementation exists to avoid.
//
// pg_advisory_unlock only releases a lock held by the SAME connection. Acquiring on one pooled connection
// and unlocking on another returns false and emits a warning rather than raising, so nothing appears to
// fail -- the lock simply stays held until that connection is recycled, and every later run exits
// "another process holds the lock" while no process does.
//
// A job that stops working by succeeding is the hardest kind of failure to notice, so this asserts the
// lock is genuinely re-acquirable rather than that Release returned without complaint.
func TestAdvisoryLock_ReleaseActuallyReleases(t *testing.T) {
	requirePool(t)
	ctx := context.Background()

	first, err := TryAdvisoryLock(ctx, testPool, testLockKey)
	if err != nil {
		t.Fatalf("TryAdvisoryLock: %v", err)
	}
	first.Release()

	// Re-acquirable immediately -- necessary but NOT sufficient. Advisory locks are re-entrant within a
	// session, and this pool holds one connection, so a re-acquire can succeed on a lock the same
	// session still holds. The pg_locks assertion below is the one that actually witnesses release; it
	// is what caught the mutation where Release skipped the unlock entirely, and this check did not.
	second, err := TryAdvisoryLock(ctx, testPool, testLockKey)
	if err != nil {
		t.Fatalf("could not re-acquire after Release: %v\n"+
			"pg_advisory_unlock releases only for the connection that took the lock, so this is what a "+
			"lock acquired and released on DIFFERENT pooled connections looks like", err)
	}
	second.Release()

	// And confirm at the source: Postgres itself must report no lock held on this key.
	var held bool
	if err := testPool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND objid = $1)`,
		int64(testLockKey)).Scan(&held); err != nil {
		t.Fatalf("query pg_locks: %v", err)
	}
	if held {
		t.Error("pg_locks still reports the advisory lock as held after Release. Asserting against " +
			"pg_locks rather than only against re-acquisition, because a leaked lock on a recycled " +
			"connection can look released to the next caller and still be held")
	}
}

// TestAdvisoryLock_ReleaseIsSafeToCallTwiceAndOnNil covers the defer path.
//
// Release is called from a defer and returns nothing, so it must never panic -- a panic in a deferred
// cleanup replaces whatever error the job was actually reporting with a stack trace about the cleanup.
func TestAdvisoryLock_ReleaseIsSafeToCallTwiceAndOnNil(t *testing.T) {
	requirePool(t)
	ctx := context.Background()

	lock, err := TryAdvisoryLock(ctx, testPool, testLockKey)
	if err != nil {
		t.Fatalf("TryAdvisoryLock: %v", err)
	}
	lock.Release()
	// Idempotent. A second Release must not double-release the pooled connection, which would corrupt
	// the pool's accounting and hand the same connection to two goroutines.
	lock.Release()

	// And a nil lock, which is what a caller holds when TryAdvisoryLock returned ErrLockHeld. The
	// obvious `defer lock.Release()` before checking the error would reach exactly this path.
	var nilLock *AdvisoryLock
	nilLock.Release()
}

// TestAdvisoryLock_ReleasesEvenWhenTheJobContextIsCancelled pins the reason Release uses
// context.Background(), and it asserts against pg_locks because RE-ACQUISITION CANNOT DETECT THIS.
//
// THE BUG. Release runs in a defer, so by the time it runs the job's context is usually already
// cancelled -- which is exactly when releasing matters most, because the run was interrupted and the
// next one needs the lock. Measured with l.ctx instead of context.Background():
//
//	unlock returns: "timeout: context already done: context canceled"
//	pool connection: NOT destroyed -- total=1 idle=1, it goes back into the pool healthy
//	pg_locks:        the advisory lock is STILL HELD
//
// So the lock leaks onto a reusable pooled connection and stays there until that connection is
// recycled. Every later run then exits "another process holds the lock" while no process does, and the
// rollup silently stops happening while reporting success.
//
// WHY THE OBVIOUS TEST CANNOT SEE IT. The first version of this test released, re-acquired, and passed
// when the mutation was applied. Advisory locks are RE-ENTRANT WITHIN A SESSION: the test pool holds one
// connection, so re-acquiring got the same session back and pg_try_advisory_lock succeeded on a lock that
// session already held. The lock was then held twice and the test called it released.
//
// pg_locks is the only witness that does not depend on which connection the pool happens to hand back.
func TestAdvisoryLock_ReleasesEvenWhenTheJobContextIsCancelled(t *testing.T) {
	requirePool(t)

	jobCtx, cancel := context.WithCancel(context.Background())
	lock, err := TryAdvisoryLock(jobCtx, testPool, testLockKey)
	if err != nil {
		t.Fatalf("TryAdvisoryLock: %v", err)
	}

	// The job is interrupted -- SIGTERM, an evicted pod, a CronJob deadline -- and only then does the
	// deferred Release run.
	cancel()
	lock.Release()

	// Asked on a FRESH context, as the next run would. Counting the locks rather than testing existence,
	// so a doubly-held re-entrant lock is visible as 2 rather than looking the same as 1.
	var held int
	if queryErr := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objid = $1`,
		int64(testLockKey)).Scan(&held); queryErr != nil {
		t.Fatalf("query pg_locks: %v", queryErr)
	}
	if held != 0 {
		t.Errorf("pg_locks reports %d advisory lock(s) still held after a cancelled job released.\n"+
			"Release must use context.Background(): the unlock fails on a cancelled context, the "+
			"connection returns to the pool healthy with the lock still on it, and every later run "+
			"then skips its work while exiting 0", held)
	}

	// And it really is re-acquirable -- necessary, but NOT sufficient on its own, per the note above.
	ctx, cancelNext := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelNext()
	next, err := TryAdvisoryLock(ctx, testPool, testLockKey)
	if err != nil {
		t.Fatalf("the lock was not re-acquirable after a cancelled job: %v", err)
	}
	next.Release()
}

// TestAdvisoryLock_DifferentKeysDoNotContend checks the keys are a namespace rather than one global
// mutex.
//
// Distinct jobs must not serialise against each other for no reason. This matters as soon as there is a
// second batch job -- a retention sweep, a partition creator -- and getting it wrong would make them
// silently take turns.
func TestAdvisoryLock_DifferentKeysDoNotContend(t *testing.T) {
	requirePool(t)
	ctx := context.Background()

	a, err := TryAdvisoryLock(ctx, testPool, testLockKey)
	if err != nil {
		t.Fatalf("lock A: %v", err)
	}
	defer a.Release()

	b, err := TryAdvisoryLock(ctx, testPool, testLockKey+1)
	if err != nil {
		t.Fatalf("lock B on a different key was refused: %v; the keys must be a namespace, not one "+
			"global mutex", err)
	}
	b.Release()
}
