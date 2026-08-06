package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SchemaChecker reports whether migrations have been applied.
//
// WHY THIS EXISTS -- FOUND BY DEPLOYING, NOT BY READING
// ====================================================
// Installing the Helm chart into kind produced a pod that was READY and BROKEN. /readyz returned 200
// because it pings the database and the ping succeeded; /api/v1/costs/summary returned 500 because the
// database had ZERO TABLES. The chart deliberately does not run migrations -- a schema change with a lock
// is a decision, not a deploy side effect -- so a fresh install genuinely has no schema.
//
// A pod that will 500 on every real request must not be in Service endpoints. Connectivity is necessary
// for readiness and it is not sufficient, and the gap between those two is exactly where this lives.
//
// WHY THE MIGRATION TABLE RATHER THAN THE APPLICATION TABLES
// --------------------------------------------------------
// Checking for `container_allocations` would work today and become a liability: an expand/contract
// migration that recreates a table would make every replica briefly unready, so a routine schema change
// would cause an outage. The migration bookkeeping table is stable across all of that.
//
// It also catches a state no table check would: `dirty = true`, which golang-migrate sets when a migration
// FAILED PART WAY. That database has some of a migration applied, which is worse than none -- and a pod
// that served traffic against it would produce wrong answers rather than errors.
//
// THE COST IS ONE ROW PER PROBE. A single-row read from a two-row table by primary key, which is a
// single-page index lookup. Compare the alternative: no signal at all until a user hits a 500.
type SchemaChecker struct {
	db Querier
}

// NewSchemaChecker wires the checker to a Querier.
func NewSchemaChecker(db Querier) *SchemaChecker { return &SchemaChecker{db: db} }

// Name identifies the check in the readiness report.
//
// "schema" rather than "postgres", because the pool's own connectivity check is already called that and a
// report with two identically-named entries tells an operator nothing about which one failed. Two distinct
// names make the report actionable: postgres down means the database is unreachable, schema down means it
// is reachable and unmigrated.
func (s *SchemaChecker) Name() string { return "schema" }

// Check verifies the schema has been migrated and is not mid-failure.
func (s *SchemaChecker) Check(ctx context.Context) error {
	var version int64
	var dirty bool

	// schema_migrations is golang-migrate's bookkeeping table: one row, the version and a dirty flag.
	err := s.db.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty)

	switch {
	case err == nil:
		// DIRTY IS THE DANGEROUS CASE, and it is why this checks more than existence. golang-migrate sets
		// it before applying a migration and clears it after; a set flag means one failed part way, so the
		// database holds some of a schema change. A pod serving traffic against that produces plausible
		// wrong answers rather than honest errors.
		if dirty {
			return fmt.Errorf("schema migration %d failed part way (dirty): resolve it before serving traffic", version)
		}
		if version == 0 {
			return errors.New("no migrations have been applied")
		}
		return nil

	case errors.Is(err, pgx.ErrNoRows):
		// The table exists and is empty, which golang-migrate leaves after a full `down`. Reachable and
		// unmigrated.
		return errors.New("schema_migrations is empty: no migrations have been applied")

	default:
		// The commonest real case on a fresh install: the table does not exist at all. The driver error
		// mentions the relation, which is exactly the useful detail here -- so unlike the API's handlers,
		// which mask driver text from users, this one keeps it. A readiness report is read by an operator,
		// not by the internet, and internal/health already documents that /readyz must not be publicly
		// exposed.
		return fmt.Errorf("cannot read schema_migrations (run `make migrate-up`): %w", err)
	}
}
