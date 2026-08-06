package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AdvisoryLockKey identifies a lock. Distinct jobs must use distinct keys or they serialise against
// each other for no reason.
type AdvisoryLockKey int64

// The keys in use. Hand-picked constants rather than a hash of a name, because hashtext() is not
// documented as stable across Postgres versions -- a key that changes on an upgrade would let two
// instances of the same job run concurrently exactly once, during the upgrade, which is the worst
// possible time for it.
const (
	// LockRollup guards the daily rollup and monthly report writer.
	LockRollup AdvisoryLockKey = 8_314_001
)

// ErrLockHeld means another process holds the lock. It is not a failure.
//
// A cron job that cannot get the lock has nothing useful to do: the holder is already doing the work.
// Exiting successfully is correct, and treating it as an error would fill an alerting channel with
// notifications about the system working as designed.
var ErrLockHeld = errors.New("advisory lock is held by another session")

// AdvisoryLock is a held lock. Release must be called, normally by defer.
type AdvisoryLock struct {
	conn *pgxpool.Conn
	key  AdvisoryLockKey
}

// TryAdvisoryLock takes a session-scoped advisory lock without blocking.
//
// WHY SESSION-SCOPED AND NOT pg_advisory_xact_lock
// -----------------------------------------------
// The transaction-scoped variant is simpler and releases itself on commit, which is usually what you
// want. It is wrong here: a backfill over 60 days runs SIXTY transactions, deliberately, so a failure
// on day 40 does not roll back the 39 days already written. A lock released at the first commit would
// protect day 1 and nothing else.
//
// WHY A DEDICATED CONNECTION -- THE BUG THIS FUNCTION EXISTS TO PREVENT
// -------------------------------------------------------------------
// "Session-scoped" means scoped to a CONNECTION, and with a pool you do not control which connection
// a query runs on. The obvious-looking version:
//
//	pool.Exec(ctx, "SELECT pg_try_advisory_lock($1)")   // acquires on connection A
//	... work ...
//	pool.Exec(ctx, "SELECT pg_advisory_unlock($1)")     // may run on connection B
//
// takes the lock on one connection and releases it on another. pg_advisory_unlock returns FALSE and
// emits a warning rather than raising, so nothing fails: the lock stays held on connection A until
// that connection is recycled, which can be minutes or hours. Every subsequent run then exits
// "another process holds the lock" while no process does, and the rollup silently stops happening.
// A job that stops working by succeeding is the hardest kind of failure to notice.
//
// pool.Acquire pins one connection for the lock's whole lifetime, which is the only way to make the
// session identity real. It costs one connection from the pool for the duration of the job -- which
// is why the lock is taken by short-lived batch jobs and not by the API.
func TryAdvisoryLock(ctx context.Context, pool *pgxpool.Pool, key AdvisoryLockKey) (*AdvisoryLock, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection for advisory lock: %w", err)
	}

	var acquired bool
	// pg_try_advisory_lock, not pg_advisory_lock: TRY returns immediately.
	//
	// The blocking form would queue this run behind the current one, and a five-minute cron with a
	// six-minute job would then accumulate waiting processes until the pool is exhausted -- a slow
	// job turning into an outage. Non-blocking means a contended run is simply skipped, and the next
	// scheduled invocation picks the work up.
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(key)).Scan(&acquired); err != nil {
		conn.Release()
		return nil, fmt.Errorf("try advisory lock %d: %w", key, err)
	}
	if !acquired {
		conn.Release()
		return nil, ErrLockHeld
	}
	return &AdvisoryLock{conn: conn, key: key}, nil
}

// Release unlocks and returns the connection to the pool.
//
// Deliberately returns no error, so it composes with defer and cannot be forgotten in favour of
// error handling nobody would act on. The failure modes are both benign: if the unlock fails the
// connection is still released, and the lock then dies with that connection whenever the pool closes
// or recycles it -- which is precisely the safety property that makes advisory locks preferable to a
// lease with a TTL.
func (l *AdvisoryLock) Release() {
	if l == nil || l.conn == nil {
		return
	}
	// context.Background(), not a caller context: Release runs in a defer, and by then the job's
	// context is usually already cancelled -- which is exactly when releasing matters most. Using a
	// cancelled context would refuse the unlock and leak the lock until the connection recycles.
	//
	// Same reasoning as the shutdown context in httpapi.Server and the rollback context in the
	// repository tests. Cleanup must not depend on the thing being cleaned up after.
	_, _ = l.conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(l.key))
	l.conn.Release()
	l.conn = nil
}
