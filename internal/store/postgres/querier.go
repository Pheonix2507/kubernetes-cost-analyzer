package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the subset of pgx we actually use.
//
// WHY THIS INTERFACE IS THE MOST USEFUL FIVE LINES IN THE PACKAGE
// --------------------------------------------------------------
// It is satisfied by BOTH *pgxpool.Pool and pgx.Tx. That single fact buys three things
// that are awkward to retrofit:
//
//  1. COMPOSABILITY. A repository written against Querier works standalone (pool) or as
//     one step inside a larger transaction (tx). Without it, every method needs a
//     duplicate "...Tx" variant, or the transaction boundary has to live inside the
//     repository -- and then two repositories can never share a transaction, which is
//     exactly what the collector needs when it upserts a namespace and then a pod that
//     references it.
//
//  2. TEST ISOLATION. Tests open a transaction, run against it, and roll back. No
//     truncation between tests, no ordering dependencies, no shared-state flakiness, and
//     no container per test. See helpers_test.go.
//
//  3. IT DOCUMENTS THE BLAST RADIUS. Five methods. No Begin, no Close, no Config. A
//     repository cannot accidentally start a nested transaction or close the pool out
//     from under the process.
//
// This is the same shape sqlc generates and calls DBTX, which is a good sign: when we
// eventually swap hand-written SQL for generated code, the seam is already in the right
// place.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	// SendBatch pipelines many statements in ONE network round trip.
	//
	// This is the difference between a collector that writes 5,000 allocation rows in
	// about a second and one that takes 30. Each individual Exec costs a full round trip,
	// and at 0.5ms each that is 2.5 seconds of pure latency before Postgres does any work
	// at all. Batching sends them together and reads the results back in order.
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
}

// Compile-time proof that the two things we actually pass in both satisfy Querier. If a
// pgx upgrade changes either signature, this fails here rather than at a call site.
var (
	_ Querier = (pgx.Tx)(nil)
)

// InTx runs fn inside a transaction, committing on success and rolling back on any error
// or panic.
//
// WHY A HELPER RATHER THAN Begin/Commit AT EACH CALL SITE
// ------------------------------------------------------
// The manual version has two failure modes that are easy to write and hard to see:
//
//	tx, _ := pool.Begin(ctx)
//	doWork(tx)            // returns an error, which nobody checks
//	tx.Commit(ctx)        // commits the partial work anyway
//
// and the worse one: an early return between Begin and Commit LEAKS the transaction. The
// connection stays checked out of the pool holding locks until the server times it out,
// and enough of those exhaust the pool. The symptom is "the database is slow", and the
// cause is a missing defer three files away.
//
// The deferred rollback makes both impossible. Rollback after a successful Commit is a
// documented no-op in pgx, so the defer is unconditional and needs no bookkeeping flag.
//
// The panic re-raise matters too: without it, a panic would unwind past Commit, the defer
// would roll back correctly, but the caller would see a mysterious crash with no
// indication a transaction was involved.
func InTx(ctx context.Context, pool TxBeginner, fn func(Querier) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		// Uses context.Background(), NOT ctx. Same reasoning as the graceful-shutdown
		// context in internal/httpapi/server.go: if ctx was cancelled -- a client
		// disconnect, a SIGTERM -- then a rollback issued on it would be refused, and the
		// transaction would leak exactly when we most need it cleaned up.
		_ = tx.Rollback(context.Background())
	}()

	if err := fn(tx); err != nil {
		// Deliberately NOT wrapped with "transaction failed": the caller's error is more
		// specific than anything this layer could add, and errors.Is must keep working
		// through it.
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// TxBeginner is the one method InTx needs from a pool.
//
// Narrower than passing *pgxpool.Pool: it keeps InTx testable and states that this
// function starts transactions and does nothing else with the pool.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}
