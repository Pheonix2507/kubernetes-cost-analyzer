package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	pgxdecimal "github.com/jackc/pgx-shopspring-decimal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests run against a REAL PostgreSQL instance, and that is a deliberate choice.
//
// WHY NOT A MOCK OR AN IN-MEMORY FAKE
// -----------------------------------
// Everything worth testing here is behaviour Postgres provides, not behaviour our code
// provides: ON CONFLICT resolution, CHECK constraint enforcement, partition routing,
// numeric precision, generated columns. A mock would be us asserting that our own
// assumptions match our own assumptions, and it would pass happily while the real schema
// rejected every insert. sqlmock in particular verifies the SQL STRING, so it would pass a
// query with a typo in a column name.
//
// WHY NOT testcontainers-go
// It is the other reasonable answer and genuinely more hermetic: it starts a throwaway
// Postgres per run, so CI needs no database service. The costs are a large dependency tree
// and ~10 seconds of container startup on every `go test`. For a project that already runs
// a local Postgres via `make db-up`, reusing it keeps the loop fast. Worth revisiting when
// CI arrives in Phase 10.
//
// ISOLATION COMES FROM TRANSACTIONS, NOT TRUNCATION
// Each test runs inside a transaction that is ALWAYS rolled back. That gives:
//   - perfect isolation with no cleanup code and no TRUNCATE between tests
//   - no ordering dependencies, so tests can be reordered or run individually
//   - safety against running on the dev database, since nothing is ever committed
//
// This is only possible because the repositories accept a Querier rather than a concrete
// pool: a pgx.Tx satisfies it, so the code under test cannot tell the difference. That is
// the payoff for the interface, and it is worth understanding as the main reason to define
// it at all.

var (
	testPool *pgxpool.Pool

	// skipReason is non-empty when no database is reachable. Tests needing one then call
	// t.Skip with it.
	//
	// WHY NOT os.Exit(0) FROM TestMain, WHICH IS THE OBVIOUS SHORTCUT
	// --------------------------------------------------------------
	// An earlier version did exactly that, and it was worse than useless: `go test` reported
	// `ok` for this package having run ZERO tests, and the explanatory message written to
	// stderr never appeared. Every green result was meaningless, and nothing distinguished
	// "42 tests passed" from "no tests ran at all".
	//
	// That is the precise failure this file's own comments warn about, and it is worth
	// keeping the note: a silent skip is how a suite quietly stops testing anything.
	//
	// Calling t.Skip per test instead means `go test -v` prints an explicit SKIP line for
	// each one, and `-run` still works. The skip is visible, countable, and impossible to
	// mistake for a pass.
	skipReason string
)

// requireDB skips the calling test when no database is available.
func requireDB(t *testing.T) {
	t.Helper()
	if skipReason != "" {
		t.Skip(skipReason)
	}
}

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		skipReason = "no TEST_DATABASE_URL or DATABASE_URL set; run `make test` (loads .env) " +
			"or `make db-up && make migrate-up`"
		os.Exit(m.Run())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad database URL: %v\n", err)
		os.Exit(1)
	}
	// The same registration production uses. Registering it here too is not duplication for
	// its own sake -- it means the tests exercise the real numeric codec, so a broken
	// registration fails a test instead of surfacing in production under pool growth.
	cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		pgxdecimal.Register(conn.TypeMap())
		return nil
	}

	testPool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot create pool: %v\n", err)
		os.Exit(1)
	}
	defer testPool.Close()

	// A configured-but-unreachable database is a SKIP, not a failure: it usually means the
	// container is simply not running. A misconfigured URL, by contrast, failed above.
	if pingErr := testPool.Ping(ctx); pingErr != nil {
		skipReason = fmt.Sprintf("postgres unreachable at the configured URL (%v); run `make db-up`", pingErr)
		os.Exit(m.Run())
	}

	// Fail with an ACTIONABLE message if the schema is missing, rather than letting every
	// test fail with "relation container_allocations does not exist".
	var exists bool
	err = testPool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'container_allocations')`,
	).Scan(&exists)
	if err != nil || !exists {
		skipReason = "schema is not migrated; run `make migrate-up`"
		os.Exit(m.Run())
	}

	os.Exit(m.Run())
}

// withTx gives a test a Querier bound to a transaction that is rolled back on cleanup.
//
// Nothing a test does is ever committed, so tests may run against the development database
// without polluting it.
func withTx(t *testing.T) (context.Context, Querier) {
	t.Helper()
	requireDB(t)

	// A per-test timeout, so a test that deadlocks on a lock fails in seconds with a clear
	// error rather than hanging the suite until CI kills it.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	t.Cleanup(func() {
		// context.Background(), not ctx: the per-test context may already be cancelled by
		// the cleanup above, and a rollback on a cancelled context is refused -- leaking
		// the transaction and holding its locks until the server times it out.
		_ = tx.Rollback(context.Background())
	})

	return ctx, tx
}

// seedFixture creates one full dimension chain and returns the ids, so each test does not
// repeat six upserts to reach the interesting assertion.
type fixture struct {
	clusterID   int64
	nodeID      int64
	namespaceID int64
	workloadID  int64
	podID       int64
}

func seedFixture(t *testing.T, ctx context.Context, db Querier) fixture {
	t.Helper()
	inv := NewInventoryRepository(db)

	// Names are suffixed with the test's name so that two tests running against the same
	// database cannot collide on a unique constraint. Rollback makes this unnecessary in
	// principle, but a test that is accidentally changed to commit would otherwise fail
	// mysteriously and only when run alongside another.
	suffix := t.Name()

	clusterID, err := inv.UpsertCluster(ctx, "cluster-"+suffix, "kind", "ap-south-1")
	if err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
	nodeID, err := inv.UpsertNode(ctx, clusterID, testNode("node-"+suffix))
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}
	namespaceID, err := inv.UpsertNamespace(ctx, clusterID, testNamespace("ns-"+suffix))
	if err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	workloadID, err := inv.UpsertWorkload(ctx, clusterID, namespaceID, testWorkload())
	if err != nil {
		t.Fatalf("seed workload: %v", err)
	}
	podID, err := inv.UpsertPod(ctx, UpsertPodParams{
		ClusterID:   clusterID,
		NamespaceID: namespaceID,
		WorkloadID:  &workloadID,
		NodeID:      &nodeID,
		Pod:         testPod("pod-"+suffix, "uid-"+suffix),
	})
	if err != nil {
		t.Fatalf("seed pod: %v", err)
	}

	return fixture{clusterID, nodeID, namespaceID, workloadID, podID}
}
