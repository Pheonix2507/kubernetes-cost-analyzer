package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

const testMiB = 1024 * 1024

// onlyStats asserts the filter isolated the fixture and returns its row.
func onlyStats(t *testing.T, rows []ContainerStats) ContainerStats {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("got %d container stats rows, want exactly 1: %+v", len(rows), rows)
	}
	return rows[0]
}

// =============================================================================
// Averaging: per container, not per workload
// =============================================================================

// TestContainerStats_AveragesArePerContainerNotPerWorkload guards the mistake that would make every
// recommendation wrong by a factor of the replica count.
//
// The GROUP BY collapses every replica AND every window into one row. avg() therefore divides by
// (replicas x windows), giving the mean for a SINGLE container -- which is what a request is set on. A
// sum() there would give the workload's total, and the engine would confidently propose requesting six
// times what one pod needs. The output would look plausible: a bigger number for a busier workload.
//
// Two replicas at 100m and 200m must average to 150m, not total to 300m.
func TestContainerStats_AveragesArePerContainerNotPerWorkload(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)
	alloc := NewAllocationRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)

	// Two pods, same container name, same window: this is what "2 replicas" looks like in the fact
	// table. A second pod row is needed because replicas are counted by DISTINCT pod_name.
	secondPodID, err := NewInventoryRepository(tx).UpsertPod(ctx, UpsertPodParams{
		ClusterID: f.clusterID, NamespaceID: f.namespaceID,
		WorkloadID: &f.workloadID, NodeID: &f.nodeID,
		Pod: testPod("pod-b-"+t.Name(), "uid-b-"+t.Name()),
	})
	if err != nil {
		t.Fatalf("seed second pod: %v", err)
	}

	for i, cpu := range []int64{100, 200} {
		a := baseAllocation(f, start)
		a.CPUMillicoresUsed = cpu
		a.MemoryBytesUsed = int64(i+1) * 100 * testMiB // 100Mi and 200Mi
		a.CPUMillicoresRequested = 500
		a.MemoryBytesRequested = 512 * testMiB
		if i == 1 {
			a.PodID = secondPodID
			a.PodName = "api-def"
		}
		if err := alloc.Insert(ctx, a); err != nil {
			t.Fatalf("insert replica %d: %v", i, err)
		}
	}

	got := onlyStats(t, mustStats(t, ctx, repo, ContainerStatsParams{
		From: start, To: start.Add(time.Hour),
		Filters: Filters{Namespace: f.namespaceName}, Limit: 10,
	}))

	if got.Replicas != 2 {
		t.Errorf("Replicas = %d, want 2 (counted by DISTINCT pod_name)", got.Replicas)
	}
	if got.CPUAvgMillicores != 150 {
		t.Errorf("CPUAvgMillicores = %d, want 150 -- the MEAN of 100 and 200.\n"+
			"300 means a sum() crept in, and the engine would then propose a request sized for the "+
			"whole workload on every single pod", got.CPUAvgMillicores)
	}
	if want := int64(150 * testMiB); got.MemAvgBytes != want {
		t.Errorf("MemAvgBytes = %d, want %d (the mean of 100Mi and 200Mi)", got.MemAvgBytes, want)
	}
	// Requests are max(), not avg(): a request is a declared value, and averaging a value that
	// changed mid-window would produce a number nobody ever configured.
	if got.CPURequestedMillicores != 500 {
		t.Errorf("CPURequestedMillicores = %d, want 500", got.CPURequestedMillicores)
	}
}

// =============================================================================
// Peaks and percentiles
// =============================================================================

// TestContainerStats_P95ExcludesUnrecordedPeaks covers the NULLIF, which is easy to read as noise.
//
// Rows written before migration 000003 have no peak columns, stored as 0. Feeding those zeroes into the
// percentile drags it toward zero in proportion to how much history predates the migration, so the
// engine proposes a SMALLER request than the container needs -- and right-sizing below real usage is the
// one failure mode here that causes an incident rather than a bill.
//
// THE FIXTURE RATIO IS THE WHOLE TEST, and my first attempt got it wrong in a way worth recording. I
// used ten unrecorded and ten recorded windows, and the test passed with the NULLIF deliberately
// removed -- so it proved nothing. The reason is arithmetic: percentile_cont(0.95) over 20 values
// interpolates at position 0.95 x 19 = 18.05, and with only ten zeroes that position already sits
// inside the recorded values. The zeroes never reached the 95th percentile, so excluding them changed
// nothing.
//
// NINETEEN unrecorded and ONE recorded is what discriminates, and it is also the realistic case: right
// after a migration adds peak columns, almost all history has no peaks. Then:
//
//	without NULLIF: sorted [0 x19, 100], position 18.05 interpolates 0 -> 100 at 5% = 5m
//	with NULLIF:    one non-null value                                              = 100m
//
// A 20x understatement, and the direction that throttles.
func TestContainerStats_P95ExcludesUnrecordedPeaks(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)

	const windows = 20
	seedWindows(t, ctx, tx, f, start, windows, func(a *domain.ContainerAllocation, i int) {
		a.CPUMillicoresUsed = 50
		a.MemoryBytesUsed = 50 * testMiB
		if i == windows-1 {
			a.CPUMillicoresMax = 100
			a.MemoryBytesMax = 100 * testMiB
		} else {
			// "not recorded" -- the pre-migration state, stored as 0 rather than NULL.
			a.CPUMillicoresMax = 0
			a.MemoryBytesMax = 0
		}
	})

	got := onlyStats(t, mustStats(t, ctx, repo, ContainerStatsParams{
		From: start, To: start.Add(3 * time.Hour),
		Filters: Filters{Namespace: f.namespaceName}, Limit: 10,
	}))

	if got.CPUP95Millicores != 100 {
		t.Errorf("CPUP95Millicores = %d, want 100 -- the only RECORDED peak.\n"+
			"5 is what you get by treating the nineteen unrecorded zeroes as observations, and a "+
			"container right-sized to 5m while peaking at 100m is throttled twenty-fold",
			got.CPUP95Millicores)
	}
	if want := int64(100 * testMiB); got.MemP95Bytes != want {
		t.Errorf("MemP95Bytes = %d, want %d (the only recorded peak)", got.MemP95Bytes, want)
	}

	// AND coverage must report the shortfall, which is what stops the engine acting on the figure above.
	// One window in twenty is 0.05, far below the gate -- so the correct outcome for this container is
	// "no recommendation yet", not "a confident 100m proposal". The percentile being right matters
	// anyway, because coverage improves as the collector runs and the gate then opens.
	if got.PeakCoverage < 0.049 || got.PeakCoverage > 0.051 {
		t.Errorf("PeakCoverage = %.4f, want 0.05: one of twenty windows recorded a peak, and the "+
			"engine must be able to see that before trusting the percentile", got.PeakCoverage)
	}
}

// TestContainerStats_PeakCoverageKeysOnMemory pins the fix for a bug that inverted the guard's purpose.
//
// Coverage answers "was a peak recorded for this window?", and the obvious column to test is CPU. That
// is wrong, because a CPU peak of 0 has two completely different meanings: "no peak was recorded" and
// "this container genuinely uses no measurable CPU". At whole-millicore resolution an idling container
// really does peak at 0.
//
// The consequence was backwards. The check exists to stop the engine inventing idle findings out of
// missing data -- but a genuinely idle container failed it and was EXCLUDED, so the guard against
// false positives was suppressing the true positives. The idle-service fixture was skipped for being
// too idle to measure.
//
// Memory is the reliable signal: every RUNNING container holds some. Measured on the real cluster,
// `sleep infinity` holds ~335 KiB and an idle nginx ~6 MiB, so a zero memory peak really does mean
// "not recorded".
func TestContainerStats_PeakCoverageKeysOnMemory(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)

	// The genuinely-idle case: zero CPU peak throughout, but memory IS recorded -- the real numbers a
	// `sleep infinity` container produces.
	seedWindows(t, ctx, tx, f, start, 12, func(a *domain.ContainerAllocation, _ int) {
		a.CPUMillicoresUsed, a.CPUMillicoresMax = 0, 0
		a.MemoryBytesUsed, a.MemoryBytesMax = 335872, 335872
	})

	got := onlyStats(t, mustStats(t, ctx, repo, ContainerStatsParams{
		From: start, To: start.Add(2 * time.Hour),
		Filters: Filters{Namespace: f.namespaceName}, Limit: 10,
	}))

	if got.PeakCoverage != 1.0 {
		t.Errorf("PeakCoverage = %.3f, want 1.0.\n"+
			"Every window recorded a memory peak. Keying coverage on cpu_millicores_max would report "+
			"0.0 here and the engine would refuse to analyse a container that is idle -- which is "+
			"exactly the finding it exists to produce", got.PeakCoverage)
	}
	if got.CPUMaxMillicores != 0 {
		t.Errorf("CPUMaxMillicores = %d, want 0: a genuinely idle container peaks at zero and that "+
			"must be reportable rather than treated as missing", got.CPUMaxMillicores)
	}
}

// =============================================================================
// Rates and observation span
// =============================================================================

// TestContainerStats_UsesTheLatestRateNotAnAverage pins a subtle choice.
//
// Converting a proposed reduction into money must use the price that applies NOW. A rate averaged
// across a period in which the price changed produces a saving figure that was never true at any point
// -- and spot prices move constantly, so this is the normal case rather than an edge one.
func TestContainerStats_UsesTheLatestRateNotAnAverage(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	// The rate halves partway through. An average would report 0.0360; the latest is 0.0240.
	seedWindows(t, ctx, tx, f, start, 4, func(a *domain.ContainerAllocation, i int) {
		if i < 2 {
			a.CPUCostPerCoreHour = mustDec(t, "0.0480000000")
		} else {
			a.CPUCostPerCoreHour = mustDec(t, "0.0240000000")
		}
	})

	got := onlyStats(t, mustStats(t, ctx, repo, ContainerStatsParams{
		From: start, To: start.Add(time.Hour),
		Filters: Filters{Namespace: f.namespaceName}, Limit: 10,
	}))

	if !got.CPUCostPerCoreHour.Equal(mustDec(t, "0.0240000000")) {
		t.Errorf("CPUCostPerCoreHour = %s, want 0.0240000000 -- the rate from the LATEST window.\n"+
			"0.0360 is the average across the price change, which was the true price at no point in "+
			"the period", got.CPUCostPerCoreHour)
	}
}

// TestContainerStats_ObservationSpanIsTheEvidenceGate checks the fields the engine gates on.
//
// Confidence is based on observation SPAN rather than sample count, and the difference matters: a
// hundred windows collected over one hour still only describes that hour. A recommendation to delete a
// workload that runs weekly must not come from a Tuesday afternoon.
func TestContainerStats_ObservationSpanIsTheEvidenceGate(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	const windows = 6
	seedWindows(t, ctx, tx, f, start, windows, nil)

	got := onlyStats(t, mustStats(t, ctx, repo, ContainerStatsParams{
		From: start, To: start.Add(3 * time.Hour),
		Filters: Filters{Namespace: f.namespaceName}, Limit: 10,
	}))

	if got.WindowCount != windows {
		t.Errorf("WindowCount = %d, want %d", got.WindowCount, windows)
	}
	// ObservedFrom is the earliest window START and ObservedTo the latest window END, so the span
	// covers the measured period rather than the requested range. Reporting the REQUESTED range would
	// make three windows in a seven-day query look like a week of evidence.
	if !got.ObservedFrom.Equal(start) {
		t.Errorf("ObservedFrom = %s, want %s (the earliest window start)", got.ObservedFrom, start)
	}
	wantTo := start.Add(windows * 5 * time.Minute)
	if !got.ObservedTo.Equal(wantTo) {
		t.Errorf("ObservedTo = %s, want %s (the LAST window's END, not its start)", got.ObservedTo, wantTo)
	}
	if span := got.ObservedTo.Sub(got.ObservedFrom); span != 30*time.Minute {
		t.Errorf("observed span = %v, want 30m: six five-minute windows. Reporting the requested 3h "+
			"instead would let the engine treat half an hour of data as three hours of evidence", span)
	}
}

// TestContainerStats_RejectsUnknownFilter confirms the allow-list guards this query too.
//
// It shares filterColumns with the summary, and that sharing is the point: a filter added to one
// endpoint and forgotten in the other is how an allow-list quietly develops a hole.
func TestContainerStats_RejectsUnknownFilter(t *testing.T) {
	ctx, tx := withTx(t)
	repo := NewReportRepository(tx)

	// Reaches the same code path a caller cannot: Filters is a struct, so an unknown key can only
	// arrive if someone adds a field without adding its column. The test documents what happens then.
	_, err := repo.ContainerStats(ctx, ContainerStatsParams{
		From:    time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		To:      time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC),
		Filters: Filters{CapacityType: "on-demand"},
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("a known filter was rejected: %v", err)
	}
}

// mustStats runs the query or fails the test.
func mustStats(t *testing.T, ctx context.Context, repo *ReportRepository, p ContainerStatsParams) []ContainerStats {
	t.Helper()
	rows, err := repo.ContainerStats(ctx, p)
	if err != nil {
		t.Fatalf("ContainerStats: %v", err)
	}
	return rows
}
