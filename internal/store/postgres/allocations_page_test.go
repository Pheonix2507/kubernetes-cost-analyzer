package postgres

import (
	"testing"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

// =============================================================================
// The cursor itself
// =============================================================================

// TestCursor_RoundTrips is the cheapest test here and guards the most embarrassing failure: a cursor
// that does not survive its own encode/decode pair means every second page is wrong.
//
// The timestamp is the part that actually breaks. A cursor encoding a local time and decoding as UTC
// shifts the keyset predicate by the offset, and a client paginating across that boundary silently
// skips or repeats rows -- which looks like a data problem, not a serialisation one.
func TestCursor_RoundTrips(t *testing.T) {
	t.Parallel()

	// A non-UTC zone with a half-hour offset, because a bug that assumes whole hours survives testing
	// in most zones. India is UTC+05:30.
	ist := time.FixedZone("IST", 5*3600+1800)

	want := Cursor{
		WindowStart:   time.Date(2026, 8, 4, 9, 5, 0, 0, ist),
		PodID:         918273645,
		ContainerName: "app-with-a-hyphen",
	}

	encoded, err := want.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := DecodeCursor(encoded)
	if err != nil {
		t.Fatalf("DecodeCursor(%q): %v", encoded, err)
	}

	// Equal, not ==: the decoded time carries a different *Location, and comparing the structs
	// directly would fail on a difference that does not exist in the instant they represent.
	if !got.WindowStart.Equal(want.WindowStart) {
		t.Errorf("WindowStart = %s, want %s (the same instant)", got.WindowStart, want.WindowStart)
	}
	if got.PodID != want.PodID || got.ContainerName != want.ContainerName {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// TestCursor_RejectsGarbage checks a malformed cursor is an error rather than a silent restart.
//
// Returning the first page for an unreadable cursor is the tempting behaviour and the wrong one: a
// client looping "fetch page, follow cursor" would re-read page one forever with nothing in the
// response to explain why.
func TestCursor_RejectsGarbage(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"not-base64-at-all!!",
		"",
		"eyJ3Ijoibm90LWEtdGltZSJ9", // valid base64, valid JSON, invalid timestamp
		"e30",                      // {} -- decodes, but names no row
	} {
		if _, err := DecodeCursor(bad); err == nil {
			t.Errorf("DecodeCursor(%q) succeeded; a malformed cursor must be an error rather than a "+
				"silent restart from page one", bad)
		}
	}
}

// =============================================================================
// Keyset traversal
// =============================================================================

// TestAllocations_FullTraversalCoversEveryRowExactlyOnce is THE pagination test.
//
// WHY THIS PROPERTY AND NOT "PAGE 2 LOOKS RIGHT"
// ---------------------------------------------
// Both classic pagination bugs are invisible when you look at one page at a time. An off-by-one in
// the predicate (<= instead of <) repeats the boundary row on every page; a strict > on a
// non-unique key skips rows that share a timestamp. Each page looks entirely plausible on its own.
// The only assertion that catches either is walking the whole set and comparing it against what was
// inserted.
//
// The fixture deliberately puts SEVERAL CONTAINERS IN THE SAME WINDOW, so the boundary between pages
// falls in the middle of a group sharing a window_start. That is exactly where a keyset predicate
// comparing only the timestamp loses rows, and it is why the predicate is a row-value comparison over
// the full primary key.
func TestAllocations_FullTraversalCoversEveryRowExactlyOnce(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)
	alloc := NewAllocationRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)

	// 7 windows x 3 containers = 21 rows, paged 4 at a time. 21 is not a multiple of 4, so the last
	// page is partial -- the case where an off-by-one in the "is there a next page" check shows up.
	const (
		windows    = 7
		containers = 3
		pageSize   = 4
	)
	want := map[string]bool{}
	for w := range windows {
		ts := start.Add(time.Duration(w) * 5 * time.Minute)
		for c := range containers {
			a := baseAllocation(f, ts)
			a.ContainerName = []string{"app", "sidecar", "proxy"}[c]
			if err := alloc.Insert(ctx, a); err != nil {
				t.Fatalf("insert %s/%s: %v", ts, a.ContainerName, err)
			}
			want[ts.Format(time.RFC3339)+"/"+a.ContainerName] = true
		}
	}

	seen := map[string]int{}
	var cursor *Cursor
	pages := 0
	for {
		page, err := repo.Allocations(ctx, AllocationsParams{
			From: start, To: start.Add(time.Hour),
			Filters: Filters{Namespace: f.namespaceName},
			Limit:   pageSize, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if pages > 20 {
			t.Fatal("more than 20 pages for 21 rows: the cursor is not advancing, which means an " +
				"infinite loop for any real client")
		}

		if len(page.Rows) > pageSize {
			t.Fatalf("page %d returned %d rows, want at most %d", pages, len(page.Rows), pageSize)
		}
		for _, row := range page.Rows {
			seen[row.WindowStart.UTC().Format(time.RFC3339)+"/"+row.Container]++
		}

		if page.NextCursor == "" {
			break
		}
		c, err := DecodeCursor(page.NextCursor)
		if err != nil {
			t.Fatalf("page %d returned an undecodable cursor %q: %v", pages, page.NextCursor, err)
		}
		cursor = &c
	}

	// EXACTLY ONCE. Both directions matter and they fail differently: a duplicate inflates any total
	// computed client-side, and a gap hides cost that is really there.
	for key := range want {
		switch seen[key] {
		case 1:
		case 0:
			t.Errorf("row %s was never returned: a strict > on a non-unique key SKIPS rows sharing "+
				"a window_start", key)
		default:
			t.Errorf("row %s was returned %d times: a <= in the keyset predicate REPEATS the "+
				"boundary row on every page", key, seen[key])
		}
	}
	for key := range seen {
		if !want[key] {
			t.Errorf("row %s was returned but never inserted", key)
		}
	}
	if len(seen) != windows*containers {
		t.Errorf("traversal returned %d distinct rows over %d pages, want %d",
			len(seen), pages, windows*containers)
	}
}

// TestAllocations_OrderedByThePrimaryKey checks the sort matches the index the cursor relies on.
//
// Keyset pagination is only correct if the ORDER BY is TOTAL and matches the cursor's comparison in
// both key order and DIRECTION. If the query ordered by anything the cursor does not encode, two rows
// could tie and Postgres would be free to return them in either order between pages -- so the same
// row could appear on both sides of a boundary, non-deterministically.
//
// The direction is DESCENDING -- newest first -- and I first wrote this test asserting ascending,
// which is worth recording because the failure was mine rather than the query's. Descending is right
// for this endpoint: it is the audit view behind a cost figure, and the question is almost always
// "what happened recently", not "what happened first". It also matches the strict `<` in the keyset
// predicate, and those two must agree or the predicate excludes the wrong side of the boundary.
//
// Note the traversal test above passed while this one failed, which is the useful signal: the
// implementation was internally consistent, and only my assumption about the contract was wrong.
func TestAllocations_OrderedByThePrimaryKey(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)
	alloc := NewAllocationRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	// Inserted out of order, so a passing test cannot be an accident of insertion order.
	for _, w := range []int{2, 0, 1} {
		ts := start.Add(time.Duration(w) * 5 * time.Minute)
		for _, name := range []string{"sidecar", "app"} {
			a := baseAllocation(f, ts)
			a.ContainerName = name
			if err := alloc.Insert(ctx, a); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}

	page, err := repo.Allocations(ctx, AllocationsParams{
		From: start, To: start.Add(time.Hour),
		Filters: Filters{Namespace: f.namespaceName}, Limit: 100,
	})
	if err != nil {
		t.Fatalf("Allocations: %v", err)
	}
	if len(page.Rows) != 6 {
		t.Fatalf("got %d rows, want 6", len(page.Rows))
	}

	for i := 1; i < len(page.Rows); i++ {
		prev, cur := page.Rows[i-1], page.Rows[i]
		if cur.WindowStart.After(prev.WindowStart) {
			t.Fatalf("row %d (%s) is NEWER than row %d (%s): window_start must be descending to match "+
				"the `<` in the keyset predicate",
				i, cur.WindowStart, i-1, prev.WindowStart)
		}
		// Within a window, container_name breaks the tie -- the third column of the primary key, also
		// descending. Without a total order the boundary row is whichever one Postgres felt like.
		if cur.WindowStart.Equal(prev.WindowStart) && cur.Container > prev.Container {
			t.Errorf("within window %s, %q follows %q: the tie-break must be total AND descending, or "+
				"the cursor boundary is non-deterministic", cur.WindowStart, cur.Container, prev.Container)
		}
	}
}

// TestAllocations_LastPageHasNoCursor checks the termination condition.
//
// A non-empty cursor on the final page makes a client fetch one more page, get nothing, and either
// loop forever or render an empty tail. The cursor is the signal that more data EXISTS, so it must be
// derived from having seen an extra row rather than from the page being full -- a result whose size
// happens to equal the limit is not evidence of anything.
func TestAllocations_LastPageHasNoCursor(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	// EXACTLY as many rows as the page size: the trap case. A "full page means more data" check
	// returns a cursor here and the client fetches an empty page.
	seedWindows(t, ctx, tx, f, start, 3, nil)

	page, err := repo.Allocations(ctx, AllocationsParams{
		From: start, To: start.Add(time.Hour),
		Filters: Filters{Namespace: f.namespaceName}, Limit: 3,
	})
	if err != nil {
		t.Fatalf("Allocations: %v", err)
	}
	if len(page.Rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(page.Rows))
	}
	if page.NextCursor != "" {
		t.Errorf("a full final page returned a cursor (%q). The cursor must come from having read an "+
			"extra row, not from the page being full -- otherwise every exactly-full result makes the "+
			"client fetch an empty page", page.NextCursor)
	}
}

// TestAllocations_EmptyPageIsAnEmptySliceWithNoCursor covers the no-data case, which a dashboard hits
// on its first render before the collector has written anything.
func TestAllocations_EmptyPageIsAnEmptySliceWithNoCursor(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	page, err := repo.Allocations(ctx, AllocationsParams{
		From:    time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC),
		To:      time.Date(2001, 1, 2, 0, 0, 0, 0, time.UTC),
		Filters: Filters{Namespace: f.namespaceName}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Allocations: %v", err)
	}
	if page.Rows == nil {
		t.Error("Rows is nil; want an empty slice so it marshals to [] rather than null")
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q on an empty result", page.NextCursor)
	}
}

// TestAllocations_RateSourceIsSurfaced checks the provenance reaches the row a human inspects.
//
// This endpoint exists as the escape hatch for "that figure looks wrong, show me what it came from".
// If a fallback-priced row looked identical to a catalogue-priced one, the audit trail would answer
// the question with the same confidence whether or not the price was a guess.
func TestAllocations_RateSourceIsSurfaced(t *testing.T) {
	ctx, tx := withTx(t)
	f := seedFixture(t, ctx, tx)
	repo := NewReportRepository(tx)

	start := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	seedWindows(t, ctx, tx, f, start, 1, func(a *domain.ContainerAllocation, _ int) {
		a.RateSource = "fallback"
	})

	page, err := repo.Allocations(ctx, AllocationsParams{
		From: start, To: start.Add(time.Hour),
		Filters: Filters{Namespace: f.namespaceName, EstimatedOnly: true}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Allocations: %v", err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("estimated_only matched %d rows, want 1", len(page.Rows))
	}
	if page.Rows[0].RateSource != "fallback" {
		t.Errorf("RateSource = %q, want fallback", page.Rows[0].RateSource)
	}
}
