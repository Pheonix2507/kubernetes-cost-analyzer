package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

func testNode(name string) domain.Node {
	return domain.Node{
		Name:                     name,
		InstanceType:             "m5.large",
		Region:                   "ap-south-1",
		Zone:                     "ap-south-1a",
		CapacityType:             "on-demand",
		CapacityCPUMillicores:    2000,
		CapacityMemoryBytes:      8 * 1024 * 1024 * 1024,
		AllocatableCPUMillicores: 1900,
		AllocatableMemoryBytes:   7 * 1024 * 1024 * 1024,
		Ready:                    true,
	}
}

func testNamespace(name string) domain.Namespace {
	return domain.Namespace{
		Name:        name,
		Team:        "payments",
		CostCentre:  "cc-1001",
		Environment: "production",
		Labels:      map[string]string{"team": "payments", "extra": "kept"},
	}
}

func testWorkload() domain.Workload {
	return domain.Workload{Kind: "Deployment", Name: "api"}
}

func testPod(name, uid string) domain.Pod {
	started := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	return domain.Pod{
		UID: uid, Name: name, QoSClass: "Burstable", StartedAt: &started,
	}
}

// -----------------------------------------------------------------------------
// Idempotency: the property the whole collector depends on
// -----------------------------------------------------------------------------

// TestUpsert_IsIdempotent is the most important test in this file. The collector sees the
// same objects every cycle, so an upsert that created a second row would multiply the
// dimension tables by the number of collection cycles -- and every cost report would then
// join against duplicates and multiply the bill.
func TestUpsert_IsIdempotent(t *testing.T) {
	ctx, tx := withTx(t)
	inv := NewInventoryRepository(tx)

	clusterID1, err := inv.UpsertCluster(ctx, ClusterAttributes{Name: "c1", Provider: "kind", Region: "ap-south-1", Currency: "USD"})
	if err != nil {
		t.Fatalf("first UpsertCluster: %v", err)
	}
	clusterID2, err := inv.UpsertCluster(ctx, ClusterAttributes{Name: "c1", Provider: "kind", Region: "ap-south-1", Currency: "USD"})
	if err != nil {
		t.Fatalf("second UpsertCluster: %v", err)
	}
	if clusterID1 != clusterID2 {
		t.Errorf("cluster id changed on re-upsert: %d then %d", clusterID1, clusterID2)
	}

	// Same for each dimension, and each is checked because they have DIFFERENT conflict
	// targets -- clusters on (name), nodes on (cluster_id, name), pods on (cluster_id, uid).
	// A typo in any one of those unique constraints breaks only that table.
	nodeID1, _ := inv.UpsertNode(ctx, clusterID1, testNode("n1"))
	nodeID2, err := inv.UpsertNode(ctx, clusterID1, testNode("n1"))
	if err != nil || nodeID1 != nodeID2 {
		t.Errorf("node id changed on re-upsert: %d then %d (err=%v)", nodeID1, nodeID2, err)
	}

	nsID1, _ := inv.UpsertNamespace(ctx, clusterID1, testNamespace("ns1"))
	nsID2, err := inv.UpsertNamespace(ctx, clusterID1, testNamespace("ns1"))
	if err != nil || nsID1 != nsID2 {
		t.Errorf("namespace id changed on re-upsert: %d then %d (err=%v)", nsID1, nsID2, err)
	}

	wID1, _ := inv.UpsertWorkload(ctx, clusterID1, nsID1, testWorkload())
	wID2, err := inv.UpsertWorkload(ctx, clusterID1, nsID1, testWorkload())
	if err != nil || wID1 != wID2 {
		t.Errorf("workload id changed on re-upsert: %d then %d (err=%v)", wID1, wID2, err)
	}

	p := UpsertPodParams{ClusterID: clusterID1, NamespaceID: nsID1, WorkloadID: &wID1, NodeID: &nodeID1,
		Pod: testPod("p1", "uid-1")}
	podID1, _ := inv.UpsertPod(ctx, p)
	podID2, err := inv.UpsertPod(ctx, p)
	if err != nil || podID1 != podID2 {
		t.Errorf("pod id changed on re-upsert: %d then %d (err=%v)", podID1, podID2, err)
	}

	// And exactly one row of each -- COUNTED WITHIN THIS TEST'S OWN CLUSTER.
	//
	// An earlier version counted the WHOLE table, via a since-removed CountRows helper. This test runs inside a
	// rolled-back transaction so its own writes never persist, but it can still SEE rows
	// committed by anything else -- another process, or the collector having been run against
	// this database. So the global assertion was only ever true on an empty database, and it
	// broke the moment real data existed.
	//
	// That is a test that passes by luck. Scoping every count to the cluster_id this test
	// created makes the assertion about THIS TEST'S rows, which is what it was always meant to
	// check.
	counts := []struct {
		table string
		query string
	}{
		{"clusters", `SELECT count(*) FROM clusters WHERE id = $1`},
		{"nodes", `SELECT count(*) FROM nodes WHERE cluster_id = $1`},
		{"namespaces", `SELECT count(*) FROM namespaces WHERE cluster_id = $1`},
		{"workloads", `SELECT count(*) FROM workloads WHERE cluster_id = $1`},
		{"pods", `SELECT count(*) FROM pods WHERE cluster_id = $1`},
	}
	for _, c := range counts {
		var got int64
		if err := tx.QueryRow(ctx, c.query, clusterID1).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", c.table, err)
		}
		if got != 1 {
			t.Errorf("%s has %d rows for this cluster after two upserts, want 1", c.table, got)
		}
	}
}

// TestUpsertNode_PreservesFirstSeen pins a subtle detail of the ON CONFLICT clause.
//
// first_seen_at is deliberately absent from the SET list. If it were included, every
// collection cycle would overwrite it with now(), and the answer to "how long have we been
// paying for this node?" -- which is the whole point of recording it -- would always be
// "since five minutes ago".
func TestUpsertNode_PreservesFirstSeen(t *testing.T) {
	ctx, tx := withTx(t)
	inv := NewInventoryRepository(tx)

	clusterID, err := inv.UpsertCluster(ctx, ClusterAttributes{Name: "c1", Provider: "kind", Region: "", Currency: "USD"})
	if err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
	if _, upsertErr := inv.UpsertNode(ctx, clusterID, testNode("n1")); upsertErr != nil {
		t.Fatalf("first upsert: %v", upsertErr)
	}

	var firstSeen1, lastSeen1 time.Time
	err = tx.QueryRow(ctx, `SELECT first_seen_at, last_seen_at FROM nodes WHERE name = 'n1'`).
		Scan(&firstSeen1, &lastSeen1)
	if err != nil {
		t.Fatalf("read timestamps: %v", err)
	}

	// A distinguishable second observation: the node has been resized (a real scenario on
	// GKE, and it must update) while first_seen_at must not move.
	grown := testNode("n1")
	grown.CapacityCPUMillicores = 4000
	grown.InstanceType = "m5.xlarge"
	if _, upsertErr := inv.UpsertNode(ctx, clusterID, grown); upsertErr != nil {
		t.Fatalf("second upsert: %v", upsertErr)
	}

	var firstSeen2 time.Time
	var capacity int64
	var instanceType string
	err = tx.QueryRow(ctx,
		`SELECT first_seen_at, capacity_cpu_millicores, instance_type FROM nodes WHERE name = 'n1'`).
		Scan(&firstSeen2, &capacity, &instanceType)
	if err != nil {
		t.Fatalf("read after update: %v", err)
	}

	if !firstSeen1.Equal(firstSeen2) {
		t.Errorf("first_seen_at moved from %v to %v; node lifetime is now unknowable",
			firstSeen1, firstSeen2)
	}
	// Mutable attributes MUST have been updated, or a resized node would be priced wrongly
	// forever.
	if capacity != 4000 || instanceType != "m5.xlarge" {
		t.Errorf("mutable attributes not updated: capacity=%d instance_type=%q", capacity, instanceType)
	}
}

// -----------------------------------------------------------------------------
// NULL handling
// -----------------------------------------------------------------------------

// TestUpsertPod_NullableForeignKeys covers the two legitimate NULLs, which mean different
// things and are both real states a collector will observe.
func TestUpsertPod_NullableForeignKeys(t *testing.T) {
	ctx, tx := withTx(t)
	inv := NewInventoryRepository(tx)

	clusterID, _ := inv.UpsertCluster(ctx, ClusterAttributes{Name: "c1", Provider: "kind", Region: "", Currency: "USD"})
	nsID, _ := inv.UpsertNamespace(ctx, clusterID, testNamespace("ns1"))

	// A Pending pod with no controller: unscheduled AND bare. Both FKs nil.
	podID, err := inv.UpsertPod(ctx, UpsertPodParams{
		ClusterID: clusterID, NamespaceID: nsID,
		WorkloadID: nil, NodeID: nil,
		Pod: domain.Pod{UID: "uid-pending", Name: "pending", QoSClass: "BestEffort"},
	})
	if err != nil {
		t.Fatalf("upsert unscheduled bare pod: %v (nil FKs must be allowed)", err)
	}

	var workloadID, nodeID *int64
	if scanErr := tx.QueryRow(ctx, `SELECT workload_id, node_id FROM pods WHERE id = $1`, podID).
		Scan(&workloadID, &nodeID); scanErr != nil {
		t.Fatalf("read pod: %v", scanErr)
	}
	if workloadID != nil || nodeID != nil {
		t.Errorf("expected NULL foreign keys, got workload=%v node=%v", workloadID, nodeID)
	}

	// Now it gets scheduled. The upsert MUST fill in the node, because a Pending pod
	// acquiring a node is the normal lifecycle and its cost starts at that moment.
	realNodeID, _ := inv.UpsertNode(ctx, clusterID, testNode("n1"))
	_, err = inv.UpsertPod(ctx, UpsertPodParams{
		ClusterID: clusterID, NamespaceID: nsID,
		WorkloadID: nil, NodeID: &realNodeID,
		Pod: domain.Pod{UID: "uid-pending", Name: "pending", QoSClass: "BestEffort"},
	})
	if err != nil {
		t.Fatalf("re-upsert after scheduling: %v", err)
	}

	if err := tx.QueryRow(ctx, `SELECT node_id FROM pods WHERE id = $1`, podID).Scan(&nodeID); err != nil {
		t.Fatalf("read after scheduling: %v", err)
	}
	if nodeID == nil || *nodeID != realNodeID {
		t.Errorf("node_id = %v after scheduling, want %d", nodeID, realNodeID)
	}
}

// TestUpsertNamespace_NilLabelsBecomeEmptyObject covers the jsonb CHECK constraint. A nil Go
// map encoded naively becomes SQL NULL, which violates jsonb_typeof(labels) = 'object' and
// would fail every namespace with no labels.
func TestUpsertNamespace_NilLabelsBecomeEmptyObject(t *testing.T) {
	ctx, tx := withTx(t)
	inv := NewInventoryRepository(tx)

	clusterID, _ := inv.UpsertCluster(ctx, ClusterAttributes{Name: "c1", Provider: "kind", Region: "", Currency: "USD"})
	nsID, err := inv.UpsertNamespace(ctx, clusterID, domain.Namespace{Name: "bare", Labels: nil})
	if err != nil {
		t.Fatalf("upsert namespace with nil labels: %v", err)
	}

	var labels string
	if err := tx.QueryRow(ctx, `SELECT labels::text FROM namespaces WHERE id = $1`, nsID).Scan(&labels); err != nil {
		t.Fatalf("read labels: %v", err)
	}
	if labels != "{}" {
		t.Errorf("labels = %q, want %q for a namespace with no labels", labels, "{}")
	}
}

// TestUpsertNamespace_LabelsRoundTrip proves the jsonb column preserves arbitrary labels, so
// grouping by an unanticipated dimension stays possible without a schema change.
func TestUpsertNamespace_LabelsRoundTrip(t *testing.T) {
	ctx, tx := withTx(t)
	inv := NewInventoryRepository(tx)

	clusterID, _ := inv.UpsertCluster(ctx, ClusterAttributes{Name: "c1", Provider: "kind", Region: "", Currency: "USD"})
	nsID, _ := inv.UpsertNamespace(ctx, clusterID, testNamespace("ns1"))

	var team, extra string
	err := tx.QueryRow(ctx,
		`SELECT labels->>'team', labels->>'extra' FROM namespaces WHERE id = $1`, nsID).
		Scan(&team, &extra)
	if err != nil {
		t.Fatalf("query jsonb keys: %v", err)
	}
	if team != "payments" || extra != "kept" {
		t.Errorf("labels round-trip lost data: team=%q extra=%q", team, extra)
	}
}

// -----------------------------------------------------------------------------
// Constraints and safety
// -----------------------------------------------------------------------------

func TestUpsertPod_RejectsInvalidQoS(t *testing.T) {
	ctx, tx := withTx(t)
	inv := NewInventoryRepository(tx)

	clusterID, _ := inv.UpsertCluster(ctx, ClusterAttributes{Name: "c1", Provider: "kind", Region: "", Currency: "USD"})
	nsID, _ := inv.UpsertNamespace(ctx, clusterID, testNamespace("ns1"))

	_, err := inv.UpsertPod(ctx, UpsertPodParams{
		ClusterID: clusterID, NamespaceID: nsID,
		Pod: domain.Pod{UID: "u", Name: "p", QoSClass: "Excellent"},
	})
	if err == nil {
		t.Error("upsert accepted an invalid QoS class; the CHECK constraint is not doing its job")
	}
}

// TestInTx_RollsBackOnError proves the transaction helper does not commit partial work,
// which is the failure that leaves the dimension tables referencing rows that do not exist.
func TestInTx_RollsBackOnError(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	wantErr := context.Canceled
	err := InTx(ctx, testPool, func(db Querier) error {
		inv := NewInventoryRepository(db)
		if _, err := inv.UpsertCluster(ctx, ClusterAttributes{Name: "rollback-me", Provider: "kind", Region: "", Currency: "USD"}); err != nil {
			return err
		}
		// Fail AFTER a successful write. This is the case that matters: the naive
		// Begin/work/Commit shape would commit this row anyway.
		return wantErr
	})
	if err == nil {
		t.Fatal("InTx returned nil, want the callback's error")
	}
	// The callback's error must pass through unwrapped enough for errors.Is to work.
	if !errors.Is(err, wantErr) {
		t.Errorf("InTx error = %v, want it to wrap %v", err, wantErr)
	}

	var exists bool
	if err := testPool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM clusters WHERE name = 'rollback-me')`).Scan(&exists); err != nil {
		t.Fatalf("check for rolled-back row: %v", err)
	}
	if exists {
		t.Error("the row survived a failed transaction; InTx committed partial work")
	}
}

// TestUpsertCluster_RefreshesDerivedAttributes covers the reason Phase 11 changed this method at
// all: provider and region are DERIVED from the cluster's nodes on every cycle, so they have to be
// able to change. Before Phase 11 the collector passed the literals "kubernetes" and "", and a
// cluster that moved region would have reported its original location forever.
func TestUpsertCluster_RefreshesDerivedAttributes(t *testing.T) {
	ctx, tx := withTx(t)
	inv := NewInventoryRepository(tx)

	id1, err := inv.UpsertCluster(ctx, ClusterAttributes{
		Name: "migrating", Provider: "kind", Region: "ap-south-1", Account: "", Currency: "USD",
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// The same cluster, now genuinely somewhere else and paid for by a named account.
	id2, err := inv.UpsertCluster(ctx, ClusterAttributes{
		Name: "migrating", Provider: "aws", Region: "us-east-1", Account: "1234567890", Currency: "USD",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("identity changed on re-upsert: %d then %d; name must remain the identity", id1, id2)
	}

	var provider, region, account, currency string
	if err := tx.QueryRow(ctx,
		`SELECT provider, region, account, currency FROM clusters WHERE id = $1`, id1,
	).Scan(&provider, &region, &account, &currency); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if provider != "aws" || region != "us-east-1" || account != "1234567890" {
		t.Errorf("attributes not refreshed: provider=%q region=%q account=%q", provider, region, account)
	}
	if currency != "USD" {
		t.Errorf("currency = %q, want USD", currency)
	}
}

// TestUpsertCluster_RejectsNonISO4217Currency asserts the CHECK constraint, not our own code.
//
// It is worth a test because the value's ORIGIN is a YAML file a human edits: `usd` or `US$` in a
// catalogue would otherwise travel all the way to a rendered dashboard. The constraint is the last
// place that can refuse it, so the constraint is what gets tested.
func TestUpsertCluster_RejectsNonISO4217Currency(t *testing.T) {
	ctx, tx := withTx(t)
	inv := NewInventoryRepository(tx)

	for _, bad := range []string{"usd", "US$", "US", "USDD", "", "usd "} {
		t.Run("currency="+bad, func(t *testing.T) {
			// A SAVEPOINT per case, because a constraint violation aborts the enclosing
			// transaction: without it the first rejection would poison the shared test
			// transaction and every later case would fail for the wrong reason.
			if _, err := tx.Exec(ctx, "SAVEPOINT c"); err != nil {
				t.Fatalf("savepoint: %v", err)
			}
			_, err := inv.UpsertCluster(ctx, ClusterAttributes{
				Name: "bad-currency-" + bad, Provider: "kind", Region: "", Currency: bad,
			})
			if err == nil {
				t.Errorf("currency %q was accepted; the ISO 4217 CHECK did not fire", bad)
			}
			if _, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT c"); rbErr != nil {
				t.Fatalf("rollback to savepoint: %v", rbErr)
			}
		})
	}
}
