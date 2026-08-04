package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func mustDec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("bad decimal literal %q: %v", s, err)
	}
	return d
}

// baseAllocation returns a valid allocation for the seeded fixture, so each test varies
// only the field it is actually about.
func baseAllocation(f fixture, start time.Time) ContainerAllocation {
	return ContainerAllocation{
		WindowStart:   start,
		WindowEnd:     start.Add(5 * time.Minute),
		PodID:         f.podID,
		ContainerName: "app",

		ClusterName:   "kca-dev",
		NamespaceName: "team-payments",
		PodName:       "api-abc",
		WorkloadKind:  "Deployment",
		WorkloadName:  "api",
		NodeName:      "worker-1",
		Team:          "payments",
		CostCentre:    "cc-1001",
		Environment:   "production",
		InstanceType:  "m5.large",
		CapacityType:  "on-demand",
		Zone:          "ap-south-1a",
		QoSClass:      "Burstable",

		CPUMillicoresRequested: 500,
		MemoryBytesRequested:   536870912,
		CPUMillicoresUsed:      2,
		MemoryBytesUsed:        5242880,

		CPUCostPerCoreHour:   decimal.RequireFromString("0.0480000000"),
		MemoryCostPerGiBHour: decimal.RequireFromString("0.0064000000"),
		CPUCost:              decimal.RequireFromString("0.0020000000"),
		MemoryCost:           decimal.RequireFromString("0.0002666667"),
	}
}

// -----------------------------------------------------------------------------
// Idempotency
// -----------------------------------------------------------------------------

// TestInsert_IsIdempotent is what makes the collector safe to retry. It runs on a timer; if
// it crashes after a partial write and re-runs the window, a bare INSERT would either fail
// or, with an incomplete key, duplicate the row and inflate the bill on every retry.
func TestInsert_IsIdempotent(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewAllocationRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	a := baseAllocation(f, start)

	if err := repo.Insert(ctx, a); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// A corrected second observation of the same window -- exactly what a backfill or a
	// retry after a Prometheus hiccup produces.
	a.CPUMillicoresUsed = 400
	a.CPUCost = mustDec(t, "0.0099999999")
	if err := repo.Insert(ctx, a); err != nil {
		t.Fatalf("second insert (same window): %v", err)
	}

	var rows int
	var cpuUsed int64
	var cpuCost decimal.Decimal
	err := tx.QueryRow(ctx, `
		SELECT count(*), max(cpu_millicores_used), max(cpu_cost)
		FROM container_allocations WHERE pod_id = $1`, f.podID).Scan(&rows, &cpuUsed, &cpuCost)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if rows != 1 {
		t.Errorf("got %d rows after inserting the same window twice, want 1 (the bill would be doubled)", rows)
	}
	// It must UPDATE, not merely ignore: a corrected sample has to win, or a transient
	// Prometheus failure would freeze a wrong number in place permanently.
	if cpuUsed != 400 {
		t.Errorf("cpu_millicores_used = %d, want the corrected 400", cpuUsed)
	}
	if !cpuCost.Equal(mustDec(t, "0.0099999999")) {
		t.Errorf("cpu_cost = %s, want the corrected value", cpuCost)
	}
}

// -----------------------------------------------------------------------------
// Money precision -- the reason for decimal and numeric
// -----------------------------------------------------------------------------

// TestInsert_MoneyPrecisionIsExact is the test that justifies decimal.Decimal and
// numeric(20,10) over float64 and double precision.
//
// If either end of the pipeline used binary floating point, these values would come back
// subtly different, and the error would compound under SUM across millions of rows -- in a
// number someone reconciles against a cloud invoice.
func TestInsert_MoneyPrecisionIsExact(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewAllocationRepository(tx)

	cases := []struct {
		name string
		val  string
	}{
		{"ten decimal places", "0.0000000001"},
		{"classic float trap", "0.1000000000"},
		{"repeating third", "0.3333333333"},
		{"large with fraction", "1234567890.1234567890"},
		{"exact zero", "0.0000000000"},
	}

	for i, c := range cases {
		start := time.Date(2026, 8, 4, 9, i*5, 0, 0, time.UTC)
		a := baseAllocation(f, start)
		a.CPUCost = mustDec(t, c.val)
		// Zero out the other money field so total_cost equals cpu_cost exactly and the
		// generated column can be checked too.
		a.MemoryCost = decimal.Zero

		if err := repo.Insert(ctx, a); err != nil {
			t.Fatalf("%s: insert %s: %v", c.name, c.val, err)
		}

		var got, total decimal.Decimal
		err := tx.QueryRow(ctx,
			`SELECT cpu_cost, total_cost FROM container_allocations WHERE pod_id=$1 AND window_start=$2`,
			f.podID, start).Scan(&got, &total)
		if err != nil {
			t.Fatalf("%s: read back: %v", c.name, err)
		}

		want := mustDec(t, c.val)
		// Equal compares numeric VALUE, so 0.10 and 0.1000000000 are equal -- which is what
		// we want, since Postgres pads to the column scale.
		if !got.Equal(want) {
			t.Errorf("%s: cpu_cost round-tripped as %s, want %s (precision was lost)", c.name, got, want)
		}
		// The GENERATED column must agree with its inputs. It cannot be written directly,
		// so this proves Postgres computed it.
		if !total.Equal(want) {
			t.Errorf("%s: total_cost = %s, want %s (generated column disagrees with cpu_cost + memory_cost)",
				c.name, total, want)
		}
	}
}

// TestGeneratedTotalCost_Sums proves total_cost really is cpu + memory, so no caller can
// make the total disagree with its parts.
func TestGeneratedTotalCost_Sums(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewAllocationRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	a := baseAllocation(f, start)
	a.CPUCost = mustDec(t, "0.0000000007")
	a.MemoryCost = mustDec(t, "0.0000000003")

	if err := repo.Insert(ctx, a); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var total decimal.Decimal
	if err := tx.QueryRow(ctx,
		`SELECT total_cost FROM container_allocations WHERE pod_id=$1`, f.podID).Scan(&total); err != nil {
		t.Fatalf("read total: %v", err)
	}
	if !total.Equal(mustDec(t, "0.0000000010")) {
		t.Errorf("total_cost = %s, want 0.0000000010", total)
	}
}

// -----------------------------------------------------------------------------
// The max(request, usage) billing rule
// -----------------------------------------------------------------------------

// TestBillable covers the rule the whole product rests on, including the BestEffort case
// that a request-only formula prices at zero.
func TestBillable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		reqCPU, usedCPU  int64
		reqMem, usedMem  int64
		wantCPU, wantMem int64
		why              string
	}{
		{
			name: "over-provisioned: request wins", reqCPU: 500, usedCPU: 2,
			reqMem: 536870912, usedMem: 5242880, wantCPU: 500, wantMem: 536870912,
			why: "the scheduler reserved 500m whether or not it was touched, so that is the bill",
		},
		{
			name: "under-requested: usage wins", reqCPU: 50, usedCPU: 400,
			reqMem: 67108864, usedMem: 210763776, wantCPU: 400, wantMem: 210763776,
			why: "the memory-hoarder fixture. It really consumed this, so it must be charged for it",
		},
		{
			name: "BestEffort: no requests at all", reqCPU: 0, usedCPU: 2,
			reqMem: 0, usedMem: 5242880, wantCPU: 2, wantMem: 5242880,
			why: "THE critical case. Billing on request alone prices this at ZERO and smears " +
				"its real cost silently across every other team's share",
		},
		{
			name: "idle: neither", reqCPU: 200, usedCPU: 0, reqMem: 268435456, usedMem: 0,
			wantCPU: 200, wantMem: 268435456,
			why: "the idle-service fixture: reserving capacity costs money even at zero usage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := ContainerAllocation{
				CPUMillicoresRequested: tt.reqCPU, CPUMillicoresUsed: tt.usedCPU,
				MemoryBytesRequested: tt.reqMem, MemoryBytesUsed: tt.usedMem,
			}
			gotCPU, gotMem := a.Billable()
			if gotCPU != tt.wantCPU || gotMem != tt.wantMem {
				t.Errorf("Billable() = (%d, %d), want (%d, %d)\nwhy this matters: %s",
					gotCPU, gotMem, tt.wantCPU, tt.wantMem, tt.why)
			}
		})
	}
}

// TestInsert_StoredBillableSatisfiesConstraint proves the database backstop works: the CHECK
// constraint independently enforces the max rule, so SQL written outside this package cannot
// break it either.
func TestInsert_StoredBillableSatisfiesConstraint(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)

	// Bypass the repository and write a deliberately wrong billable value directly.
	_, err := tx.Exec(ctx, `
		INSERT INTO container_allocations
			(window_start, window_end, pod_id, container_name, cluster_name, namespace_name, pod_name,
			 cpu_millicores_requested, cpu_millicores_used, cpu_millicores_billable)
		VALUES ('2026-08-04 09:00:00+00', '2026-08-04 09:05:00+00', $1, 'app', 'c', 'n', 'p',
		        500, 900, 500)`, f.podID)
	if err == nil {
		t.Error("the database accepted billable < max(requested, used); the CHECK constraint is missing or wrong")
	}
}

// -----------------------------------------------------------------------------
// Validation
// -----------------------------------------------------------------------------

func TestValidate(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	valid := func() ContainerAllocation {
		return ContainerAllocation{
			WindowStart: start, WindowEnd: start.Add(5 * time.Minute),
			PodID: 1, ContainerName: "app",
			ClusterName: "c", NamespaceName: "n",
		}
	}

	tests := []struct {
		name    string
		mutate  func(*ContainerAllocation)
		wantErr bool
	}{
		{"valid", func(*ContainerAllocation) {}, false},
		{"zero window start", func(a *ContainerAllocation) { a.WindowStart = time.Time{} }, true},
		{"reversed window", func(a *ContainerAllocation) { a.WindowEnd = start.Add(-time.Minute) }, true},
		{"zero-length window", func(a *ContainerAllocation) { a.WindowEnd = a.WindowStart }, true},
		{"missing pod id", func(a *ContainerAllocation) { a.PodID = 0 }, true},
		// Part of the primary key: an empty container name would silently collide with any
		// other unnamed container in the same pod and window.
		{"empty container name", func(a *ContainerAllocation) { a.ContainerName = "" }, true},
		{"missing namespace", func(a *ContainerAllocation) { a.NamespaceName = "" }, true},
		{"negative cpu", func(a *ContainerAllocation) { a.CPUMillicoresRequested = -1 }, true},
		{"negative cost", func(a *ContainerAllocation) { a.CPUCost = decimal.NewFromInt(-1) }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := valid()
			tt.mutate(&a)
			err := a.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("Validate() = nil, want an error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Batching
// -----------------------------------------------------------------------------

func TestInsertBatch(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewAllocationRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	var batch []ContainerAllocation
	for i := 0; i < 50; i++ {
		a := baseAllocation(f, start.Add(time.Duration(i)*5*time.Minute))
		batch = append(batch, a)
	}

	if err := repo.InsertBatch(ctx, batch); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	var rows int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM container_allocations WHERE pod_id = $1`, f.podID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 50 {
		t.Errorf("got %d rows, want 50", rows)
	}

	// An empty batch is a legitimate outcome -- an empty cluster, or a window where every
	// pod was Pending -- and must not be an error the caller has to special-case.
	if err := repo.InsertBatch(ctx, nil); err != nil {
		t.Errorf("InsertBatch(nil) = %v, want nil", err)
	}
}

// TestInsertBatch_ValidatesBeforeSending proves nothing is sent when any row is invalid.
//
// One bad row inside a batch aborts the enclosing transaction and takes every valid row with
// it. Failing before the first byte leaves the transaction usable and names the offending
// index.
func TestInsertBatch_ValidatesBeforeSending(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewAllocationRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	good := baseAllocation(f, start)
	bad := baseAllocation(f, start.Add(5*time.Minute))
	bad.ContainerName = "" // invalid

	err := repo.InsertBatch(ctx, []ContainerAllocation{good, bad})
	if err == nil {
		t.Fatal("InsertBatch accepted a batch containing an invalid allocation")
	}

	// Nothing at all should have been written, including the valid row that preceded it.
	var rows int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM container_allocations WHERE pod_id = $1`, f.podID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Errorf("got %d rows written, want 0 (validation must happen before any send)", rows)
	}
	// The transaction must still be usable, which is the practical benefit of failing early.
	if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
		t.Errorf("transaction is unusable after a rejected batch: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Reporting: the half-open interval
// -----------------------------------------------------------------------------

// TestCostByNamespace_HalfOpenRange is the boundary test, and it is the one most likely to
// catch a real reporting bug.
//
// Windows are [start, end). A range query must therefore be >= from AND < to. Using BETWEEN
// (closed on both ends) would include the sample exactly at `to`, which is also the first
// sample of the NEXT period -- so August and September would each count the 1 September
// 00:00 window, and the two reports would sum to more than the year.
func TestCostByNamespace_HalfOpenRange(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewAllocationRepository(tx)

	// Three windows: before the range, inside it, and exactly ON the upper bound.
	times := []time.Time{
		time.Date(2026, 8, 4, 8, 55, 0, 0, time.UTC), // before `from`
		time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),  // inside
		time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC), // exactly `to`
	}
	for _, ts := range times {
		a := baseAllocation(f, ts)
		a.NamespaceName = "team-payments"
		a.CPUCost = mustDec(t, "1.0000000000")
		a.MemoryCost = decimal.Zero
		if err := repo.Insert(ctx, a); err != nil {
			t.Fatalf("insert at %s: %v", ts, err)
		}
	}

	from := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC)

	costs, err := repo.CostByNamespace(ctx, from, to)
	if err != nil {
		t.Fatalf("CostByNamespace: %v", err)
	}
	if len(costs) != 1 {
		t.Fatalf("got %d namespaces, want 1: %+v", len(costs), costs)
	}

	// Exactly ONE window is in range: 09:00. The 08:55 sample is before `from`, and the
	// 09:30 sample sits ON `to`, which is excluded.
	if !costs[0].TotalCost.Equal(mustDec(t, "1.0000000000")) {
		t.Errorf("TotalCost = %s, want exactly 1.0 (one window in [09:00, 09:30)). "+
			"2.0 means the upper bound was included and adjacent reports will double-count",
			costs[0].TotalCost)
	}
	if costs[0].Team != "payments" {
		t.Errorf("Team = %q, want %q from the denormalised attribution", costs[0].Team, "payments")
	}
}

// TestCostByNamespace_CoreHourMaths checks the unit conversion in SQL: a 500 millicore
// reservation held for one hour is 0.5 core-hours.
func TestCostByNamespace_CoreHourMaths(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewAllocationRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	a := baseAllocation(f, start)
	a.WindowEnd = start.Add(time.Hour) // a full hour, to make the arithmetic obvious
	a.CPUMillicoresRequested = 500
	a.CPUMillicoresUsed = 0
	a.MemoryBytesRequested = 2 * 1024 * 1024 * 1024 // 2 GiB
	a.MemoryBytesUsed = 0

	if err := repo.Insert(ctx, a); err != nil {
		t.Fatalf("insert: %v", err)
	}

	costs, err := repo.CostByNamespace(ctx, start, start.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("CostByNamespace: %v", err)
	}
	if len(costs) != 1 {
		t.Fatalf("got %d rows, want 1", len(costs))
	}

	// 500 millicores for 1 hour = 0.5 core-hours.
	if !costs[0].CPUCoreHours.Equal(mustDec(t, "0.5")) {
		t.Errorf("CPUCoreHours = %s, want 0.5", costs[0].CPUCoreHours)
	}
	// 2 GiB for 1 hour = 2 GiB-hours.
	if !costs[0].MemoryGiBHours.Equal(mustDec(t, "2")) {
		t.Errorf("MemoryGiBHours = %s, want 2", costs[0].MemoryGiBHours)
	}
}

// TestCostByNamespace_EmptyRangeIsEmptySlice guards the JSON contract: a nil slice marshals
// to null, and null.length throws in the frontend.
func TestCostByNamespace_EmptyRangeIsEmptySlice(t *testing.T) {
	ctx, tx := withTx(t)
	repo := NewAllocationRepository(tx)

	costs, err := repo.CostByNamespace(ctx,
		time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("CostByNamespace: %v", err)
	}
	if costs == nil {
		t.Error("returned a nil slice; want empty so it marshals as [] not null")
	}
	if len(costs) != 0 {
		t.Errorf("got %d rows for an empty range, want 0", len(costs))
	}
}

// -----------------------------------------------------------------------------
// Partitioning
// -----------------------------------------------------------------------------

// TestInsert_PartitionRouting proves rows land in the month partition matching their window,
// and that a window outside the seeded range falls to the DEFAULT partition instead of
// failing.
//
// Without a default partition the insert would ERROR and the collector would lose data --
// see the trade-off documented in the migration.
func TestInsert_PartitionRouting(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewAllocationRepository(tx)

	cases := []struct {
		name          string
		start         time.Time
		wantPartition string
	}{
		{"august 2026", time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC), "container_allocations_2026_08"},
		{"january 2026 boundary", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "container_allocations_2026_01"},
		{"december 2027 upper edge", time.Date(2027, 12, 31, 23, 55, 0, 0, time.UTC), "container_allocations_2027_12"},
		{"beyond the seeded range", time.Date(2035, 6, 1, 0, 0, 0, 0, time.UTC), "container_allocations_default"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := baseAllocation(f, c.start)
			a.ContainerName = "c-" + c.name
			if err := repo.Insert(ctx, a); err != nil {
				t.Fatalf("insert: %v", err)
			}

			var partition string
			err := tx.QueryRow(ctx,
				`SELECT tableoid::regclass::text FROM container_allocations
				 WHERE pod_id=$1 AND window_start=$2 AND container_name=$3`,
				f.podID, c.start, a.ContainerName).Scan(&partition)
			if err != nil {
				t.Fatalf("read partition: %v", err)
			}
			if partition != c.wantPartition {
				t.Errorf("row landed in %s, want %s", partition, c.wantPartition)
			}
		})
	}
}

// TestEnsureAllocationPartition_IsIdempotent covers the maintenance function Phase 7's
// scheduler will call. Creating a partition that already exists must not error, or a
// scheduled job would fail every run after the first.
func TestEnsureAllocationPartition_IsIdempotent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		var name string
		// Committed rather than rolled back, because DDL inside a rolled-back transaction
		// would prove nothing about repeat calls. A far-future month keeps it out of the way
		// of every other test.
		if err := testPool.QueryRow(ctx,
			`SELECT ensure_allocation_partition('2029-03-01'::date)`).Scan(&name); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		if name != "container_allocations_2029_03" {
			t.Errorf("returned %q, want container_allocations_2029_03", name)
		}
	}

	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DROP TABLE IF EXISTS container_allocations_2029_03`)
	})
}
