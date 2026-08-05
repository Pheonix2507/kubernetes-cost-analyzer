// Package postgres owns the PostgreSQL connection pool and, from Phase 2 onwards,
// the repository implementations that read and write cost data.
//
// WHY internal/store/postgres AND NOT internal/database
// -----------------------------------------------------
// The path names the TECHNOLOGY, one level below a generic concept. That leaves room
// for internal/store/<something-else> later without a rename, and it makes the
// dependency direction obvious at a glance: the rest of the application talks to
// interfaces, and this package is one concrete answer to them.
//
// WHY pgx AND NOT database/sql, GORM OR ent
// -----------------------------------------
//   - database/sql is a lowest-common-denominator abstraction across every SQL
//     driver, so it cannot express Postgres-specific features. It also has no native
//     support for COPY. Phase 4 uses pgx.Batch rather than COPY -- COPY cannot express
//     ON CONFLICT, and idempotent re-collection is not negotiable -- but COPY into a temp
//     table remains the escalation if batching ever becomes the bottleneck.
//   - GORM hides the SQL it generates. For a system whose whole job is aggregating
//     time-series cost data, the queries ARE the product, and being unable to read
//     or EXPLAIN them without instrumenting the ORM is disqualifying.
//   - pgx speaks the Postgres wire protocol directly: real prepared statements,
//     native type mapping (including arrays, JSONB and numeric), COPY, LISTEN/NOTIFY,
//     and better performance than database/sql because it skips that translation
//     layer entirely.
//
// We write SQL by hand and scan rows by hand at first. That is deliberate: doing it
// manually teaches nullable columns, scan destinations and error wrapping properly.
// Once the boilerplate is genuinely painful and understood, sqlc generates it for us
// from the same SQL -- an informed refactor rather than a framework chosen up front.
package postgres

import (
	"context"
	"fmt"

	pgxdecimal "github.com/jackc/pgx-shopspring-decimal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/config"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
)

// Compile-time assertion that *DB satisfies health.Checker.
//
// WHY THIS ONE LINE IS WORTH WRITING
// ----------------------------------
// Go interfaces are satisfied implicitly, so nothing normally tells you that a type
// was MEANT to implement one. If someone renames Check to Ping, the failure appears
// wherever *DB is passed to NewAggregator -- possibly in a different package, with a
// confusing message about an unsatisfied interface.
//
// This declares the intent where the type is defined. It costs no runtime memory
// ((*DB)(nil) is a typed nil pointer, never dereferenced) and turns a distant,
// confusing error into an immediate, local one.
var _ health.Checker = (*DB)(nil)

// DB wraps a pgx connection pool.
//
// The pool field is unexported: nothing outside this package gets to run arbitrary
// SQL against our database. Callers use the repository methods added in Phase 2. If
// that rule is not enforced by the type system, queries WILL end up scattered
// through HTTP handlers, and then there is no single place to add a timeout, a
// metric or an index.
type DB struct {
	pool *pgxpool.Pool
}

// New creates the connection pool.
//
// IMPORTANT -- IT DOES NOT CONNECT. pgxpool builds the pool lazily and opens
// connections on demand, so New succeeds even when Postgres is completely down.
//
// That sounds like a bug and is exactly the behaviour we want in Kubernetes:
//
//   - If startup required a live database, then a database blip during a rollout
//     would make every new pod crash on boot. The Deployment would stall, and the
//     ReplicaSet would back off exponentially -- so the service stays broken for
//     minutes after the database has recovered.
//   - Starting anyway and reporting NOT READY means the pods exist, are being
//     health-checked, and begin serving the instant the database returns. No
//     restarts, no backoff.
//
// This is the same principle as the liveness/readiness split in internal/health:
// fail the CHECK, not the PROCESS.
func New(ctx context.Context, cfg config.Database) (*DB, error) {
	// ParseConfig validates the DSN and populates defaults. It parses only -- no
	// network I/O -- so a malformed URL fails here, immediately at startup, with a
	// message naming the problem. That is the one database failure we DO want fatal,
	// because no amount of waiting will fix a typo.
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// No %q on the URL and no logging of it: a Postgres DSN contains the
		// password. Wrapping the parse error is enough to diagnose the problem
		// without writing a credential into the logs, where it would be shipped to
		// the log aggregator and retained for months.
		return nil, fmt.Errorf("parsing DATABASE_URL: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxOpenConns
	poolCfg.MinIdleConns = cfg.MinIdleConns
	poolCfg.MaxConnLifetime = cfg.ConnMaxLifetime

	// Jitter spreads connection recycling out over time.
	//
	// Without it, every connection created during startup shares a birthday and so
	// expires within the same instant, N minutes later. The pool then reconnects all
	// of them at once -- a self-inflicted thundering herd that repeats on a fixed
	// period, and one of those "mysterious latency spike every 30 minutes" problems
	// that is very hard to attribute after the fact.
	poolCfg.MaxConnLifetimeJitter = cfg.ConnMaxLifetime / 10

	// Teach every connection how to map Postgres `numeric` to decimal.Decimal.
	//
	// WHY AfterConnect AND NOT ONCE AT POOL LEVEL: pgx's type map is PER CONNECTION, so a
	// registration done once on the pool would apply to whichever connection happened to
	// exist at the time and silently not to any opened later. The bug that produces is
	// horrible to diagnose -- money columns scan correctly for a while and then start
	// failing under load, when the pool grows past its initial size.
	//
	// Without this, scanning numeric into a decimal.Decimal fails outright, and the
	// tempting fix is to change the Go field to float64. That would compile, pass tests,
	// and quietly corrupt every financial total under SUM. See the note on
	// ContainerAllocation.
	poolCfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		pgxdecimal.Register(conn.TypeMap())
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating pgx pool: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Pool exposes the underlying pool for the repository implementations added in
// Phase 2. It stays in this package's own directory tree; see the note on DB.pool.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Close releases every connection. It blocks until in-flight queries finish, which
// is why main defers it AFTER the HTTP server has shut down -- closing the pool
// while requests are still being served would fail those requests.
func (db *DB) Close() { db.pool.Close() }

// Name implements health.Checker.
func (db *DB) Name() string { return "postgres" }

// Check implements health.Checker by acquiring a connection and pinging it.
//
// WHY Ping AND NOT `SELECT 1`
// ---------------------------
// pool.Ping acquires a real connection from the pool and verifies it round-trips.
// That distinguishes the two failures that matter and look identical from outside:
// the database being unreachable, and the database being reachable but the POOL
// being exhausted. A hand-written `SELECT 1` on an already-held connection would
// pass in the second case, and readiness would report healthy while every real
// request queued for a connection and timed out.
func (db *DB) Check(ctx context.Context) error {
	if err := db.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres ping: %w", err)
	}
	return nil
}

// Stats exposes pool statistics for the /metrics endpoint in Phase 9.
//
// Pool saturation is one of the highest-value metrics a service can emit: when
// AcquireCount rises while TotalConns sits pinned at MaxConns, requests are queueing
// for a connection, and the resulting latency looks like a slow database when it is
// actually an undersized pool. Those two causes have opposite fixes, so telling them
// apart matters.
func (db *DB) Stats() *pgxpool.Stat { return db.pool.Stat() }
