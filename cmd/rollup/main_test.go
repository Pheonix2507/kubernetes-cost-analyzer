package main

import (
	"testing"
	"time"
)

// WHY THIS FILE EXISTS -- AN AUDIT FINDING
// ---------------------------------------
// resolveRange is the entry point for every invocation of this binary. It has five branches, decides
// which days a scheduled run processes, and had NO TESTS. Two of its branches called time.Now directly,
// so testing "the default is yesterday" would have meant changing the system clock.
//
// Meanwhile internal/rollup carried a RollupYesterday method with an injectable clock and two thorough
// tests, and nothing called it -- main computed yesterday itself. The tested code was not the code that
// ran, and the code that ran was untested. A green suite that describes a path production never takes
// is worse than no suite, because it is evidence about the wrong thing.
//
// The clock is now a parameter, RollupYesterday is deleted, and its tests live here.

// at parses a test timestamp.
func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return ts.UTC()
}

// TestResolveRange_DefaultsToYesterday pins the scheduling rule, on the path that actually runs.
//
// Today is INCOMPLETE. Rolling it up writes a figure correct for the hours so far and wrong for the day,
// and because the rollup is a projection rather than an accumulation the next run replaces it -- so the
// value flickers instead of converging. A day is rolled up once it can no longer change.
func TestResolveRange_DefaultsToYesterday(t *testing.T) {
	t.Parallel()

	from, to, err := resolveRange(options{}, at(t, "2026-08-05T03:15:00Z"))
	if err != nil {
		t.Fatalf("resolveRange: %v", err)
	}
	if from.String() != "2026-08-04" || to.String() != "2026-08-04" {
		t.Errorf("range = [%s, %s], want exactly 2026-08-04 on both ends: today is incomplete", from, to)
	}
}

// TestResolveRange_YesterdayCrossesBoundaries covers the date arithmetic where it is easiest to be wrong.
//
// Ported verbatim in intent from the deleted RollupYesterday tests, because the property they described
// is real -- it was only ever attached to the wrong function.
func TestResolveRange_YesterdayCrossesBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct{ now, want, why string }{
		{"2026-08-01T02:00:00Z", "2026-07-31", "across a month"},
		{"2026-03-01T02:00:00Z", "2026-02-28", "February in a non-leap year"},
		{"2028-03-01T02:00:00Z", "2028-02-29", "February in a leap year"},
		{"2027-01-01T02:00:00Z", "2026-12-31", "across a year"},
		{"2026-08-05T00:00:00Z", "2026-08-04", "exactly midnight, the instant a nightly cron fires"},
	}

	for _, tt := range tests {
		t.Run(tt.why, func(t *testing.T) {
			t.Parallel()
			from, to, err := resolveRange(options{}, at(t, tt.now))
			if err != nil {
				t.Fatalf("resolveRange: %v", err)
			}
			if from.String() != tt.want || to.String() != tt.want {
				t.Errorf("at %s got [%s, %s], want %s on both ends (%s)",
					tt.now, from, to, tt.want, tt.why)
			}
		})
	}
}

// TestResolveRange_NonUTCClockStillYieldsAUTCDay guards the grain.
//
// The rollup is keyed on a UTC calendar day. A clock in +05:30 at 01:00 on the 5th is 19:30 on the 4th
// in UTC, so "yesterday" is the 3rd -- not the 4th, which is what reading the local date would give. Main
// passes time.Now().UTC(), and this asserts the function is correct even if a caller forgets.
func TestResolveRange_NonUTCClockStillYieldsAUTCDay(t *testing.T) {
	t.Parallel()

	ist := time.FixedZone("IST", 5*3600+1800)
	// 01:00 on 5 August IST == 19:30 on 4 August UTC. Yesterday in UTC is therefore 3 August.
	now := time.Date(2026, 8, 5, 1, 0, 0, 0, ist)

	from, to, err := resolveRange(options{}, now)
	if err != nil {
		t.Fatalf("resolveRange: %v", err)
	}
	if from.String() != "2026-08-03" {
		t.Errorf("got %s, want 2026-08-03: 01:00 IST on the 5th is 19:30 UTC on the 4th, so the "+
			"previous complete UTC day is the 3rd. Reading the LOCAL date would give the 4th and "+
			"roll up a day that is still in progress", from)
	}
	if to.String() != from.String() {
		t.Errorf("range = [%s, %s], want both ends equal", from, to)
	}
}

// TestResolveRange_MonthCoversExactlyItsDays checks the -month branch, including the calendar cases.
//
// Computed as Next() minus one day, so February and leap years need no special case -- which is exactly
// the kind of claim worth testing rather than trusting.
func TestResolveRange_MonthCoversExactlyItsDays(t *testing.T) {
	t.Parallel()

	tests := []struct{ month, wantFrom, wantTo string }{
		{"2026-07", "2026-07-01", "2026-07-31"},
		{"2026-02", "2026-02-01", "2026-02-28"},
		{"2028-02", "2028-02-01", "2028-02-29"},
		{"2026-12", "2026-12-01", "2026-12-31"},
		{"2026-04", "2026-04-01", "2026-04-30"},
	}

	for _, tt := range tests {
		t.Run(tt.month, func(t *testing.T) {
			t.Parallel()
			from, to, err := resolveRange(options{month: tt.month}, at(t, "2026-08-05T03:00:00Z"))
			if err != nil {
				t.Fatalf("resolveRange: %v", err)
			}
			if from.String() != tt.wantFrom || to.String() != tt.wantTo {
				t.Errorf("month %s -> [%s, %s], want [%s, %s]",
					tt.month, from, to, tt.wantFrom, tt.wantTo)
			}
		})
	}
}

// TestResolveRange_Precedence pins the flag ordering.
//
// Widest to narrowest, so a combination cannot silently do LESS than the operator asked for. `-all -from
// X` doing one day would be the dangerous direction: the operator asked for a full catch-up and would be
// told it succeeded.
func TestResolveRange_Precedence(t *testing.T) {
	t.Parallel()

	now := at(t, "2026-08-05T03:00:00Z")

	t.Run("-all beats everything", func(t *testing.T) {
		t.Parallel()
		from, to, err := resolveRange(options{all: true, month: "2026-07", from: "2026-08-01"}, now)
		if err != nil {
			t.Fatalf("resolveRange: %v", err)
		}
		if from.String() != "2020-01-01" {
			t.Errorf("from = %s, want the earliest backfill bound: -all must not be narrowed by "+
				"another flag", from)
		}
		// Today, not yesterday: -all is an explicit catch-up, so a partial today is what was asked for.
		// Safe because the rollup is a projection and tomorrow's run replaces it.
		if to.String() != "2026-08-05" {
			t.Errorf("to = %s, want today (2026-08-05) for an explicit catch-up", to)
		}
	})

	t.Run("-month beats -from", func(t *testing.T) {
		t.Parallel()
		from, _, err := resolveRange(options{month: "2026-07", from: "2026-08-01"}, now)
		if err != nil {
			t.Fatalf("resolveRange: %v", err)
		}
		if from.String() != "2026-07-01" {
			t.Errorf("from = %s, want 2026-07-01", from)
		}
	})

	t.Run("-from alone is a single day", func(t *testing.T) {
		t.Parallel()
		from, to, err := resolveRange(options{from: "2026-08-02"}, now)
		if err != nil {
			t.Fatalf("resolveRange: %v", err)
		}
		if from.String() != "2026-08-02" || to.String() != "2026-08-02" {
			t.Errorf("range = [%s, %s], want a single day", from, to)
		}
	})

	t.Run("-from with -to is the range", func(t *testing.T) {
		t.Parallel()
		from, to, err := resolveRange(options{from: "2026-08-01", to: "2026-08-04"}, now)
		if err != nil {
			t.Fatalf("resolveRange: %v", err)
		}
		if from.String() != "2026-08-01" || to.String() != "2026-08-04" {
			t.Errorf("range = [%s, %s], want [2026-08-01, 2026-08-04]", from, to)
		}
	})
}

// TestResolveRange_RejectsBadInput checks the operator gets told rather than getting silence.
//
// Each of these would otherwise do something plausible and wrong: -to alone would fall through to the
// default and roll up yesterday while the operator believed they had asked for a range.
func TestResolveRange_RejectsBadInput(t *testing.T) {
	t.Parallel()

	now := at(t, "2026-08-05T03:00:00Z")

	tests := []struct {
		name string
		opts options
		why  string
	}{
		{"-to without -from", options{to: "2026-08-04"},
			"it would otherwise fall through to the default and silently roll up yesterday"},
		{"unparseable -from", options{from: "yesterday"}, "only YYYY-MM-DD is accepted"},
		{"a timestamp in -from", options{from: "2026-08-04T00:00:00Z"},
			"a day is a day; accepting a timestamp invites questions about its time component"},
		{"unparseable -to", options{from: "2026-08-01", to: "the 4th"}, "same for the upper bound"},
		{"unparseable -month", options{month: "August"}, "only YYYY-MM"},
		{"a day in -month", options{month: "2026-08-04"}, "a month is not a date"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := resolveRange(tt.opts, now); err == nil {
				t.Errorf("%+v was accepted; want an error\nwhy: %s", tt.opts, tt.why)
			}
		})
	}
}

// TestParseDay_IsUTC pins the parse, because the whole rollup grain depends on it.
func TestParseDay_IsUTC(t *testing.T) {
	t.Parallel()

	d, err := parseDay("2026-08-04", "from")
	if err != nil {
		t.Fatalf("parseDay: %v", err)
	}
	got := d.Time()
	if got.Location() != time.UTC {
		t.Errorf("location = %v, want UTC", got.Location())
	}
	if !got.Equal(time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("parsed to %s, want midnight UTC on 2026-08-04", got)
	}
}
