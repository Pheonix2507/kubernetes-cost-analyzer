package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// FieldError names one thing wrong with a request.
//
// WHY FIELD-LEVEL DETAIL AND NOT JUST A MESSAGE
// ---------------------------------------------
// "invalid request" tells a caller nothing. A frontend cannot highlight the offending input, and
// a human cannot tell which of six query parameters was wrong. Naming the field and the reason
// makes the response actionable, and the Field value is what a form can key an error message to.
type FieldError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// validationError accumulates every problem with a request.
//
// EVERY problem, not the first -- the same argument as config.Validate. A caller who sends three
// bad parameters should learn about all three from one response rather than fixing them one
// round trip at a time.
type validationError struct {
	fields []FieldError
}

func (v *validationError) add(field, reason string, args ...any) {
	if len(args) > 0 {
		reason = fmt.Sprintf(reason, args...)
	}
	v.fields = append(v.fields, FieldError{Field: field, Reason: reason})
}

func (v *validationError) empty() bool { return len(v.fields) == 0 }

// maxRangeDays bounds how much history one request may span.
//
// Not arbitrary: at container grain with a five-minute window, a 5,000-container cluster produces
// roughly 43 million rows a month. A request for five years would ask Postgres to aggregate
// billions of rows and hold the result in memory, which is a denial of service a single curl can
// trigger. 400 is refused rather than silently truncated, because silently narrowing someone's
// requested range means giving them a number that is not the number they asked for.
const maxRangeDays = 400

// defaultRangeHours is used when neither bound is given.
const defaultRangeHours = 24

// timeRange parses the from/to parameters.
//
// RFC3339 ONLY, deliberately. Accepting several formats sounds friendly and produces ambiguity:
// "01/02/2026" is January 2nd or February 1st depending on where the caller lives, and guessing
// wrong shifts a whole cost report by a month. One unambiguous format, required to include a
// timezone, removes the question.
func timeRange(q url.Values, v *validationError) (from, to time.Time) {
	return timeRangeDefault(q, v, defaultRangeHours*time.Hour)
}

// timeRangeDefault is timeRange with a caller-chosen default span, so the recommendations endpoint can
// default wider than the cost endpoints without duplicating the parsing and validation.
func timeRangeDefault(q url.Values, v *validationError, defaultSpan time.Duration) (from, to time.Time) {
	now := time.Now().UTC()

	parse := func(key string) (time.Time, bool) {
		raw := strings.TrimSpace(q.Get(key))
		if raw == "" {
			return time.Time{}, false
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			v.add(key, "must be an RFC3339 timestamp including a timezone, for example 2026-08-05T09:00:00Z")
			return time.Time{}, false
		}
		return t.UTC(), true
	}

	from, hasFrom := parse("from")
	to, hasTo := parse("to")

	switch {
	case !hasFrom && !hasTo:
		to = now
		from = now.Add(-defaultSpan)
	case hasFrom && !hasTo:
		to = now
	case !hasFrom && hasTo:
		from = to.Add(-defaultSpan)
	}

	if hasFrom || hasTo {
		if !to.After(from) {
			// A zero-length or reversed range would return nothing, which looks identical to
			// "there is no data" -- so it is refused rather than answered with an empty list.
			v.add("to", "must be after from (the range is half-open: [from, to))")
		} else if to.Sub(from) > maxRangeDays*24*time.Hour {
			v.add("from", "range spans %.0f days, which exceeds the %d-day maximum; narrow the range or use a coarser grouping",
				to.Sub(from).Hours()/24, maxRangeDays)
		}
	}

	return from, to
}

// filters parses the filter parameters.
//
// Unknown query parameters are IGNORED rather than rejected, which is the opposite of the
// decision made for the pricing catalogue -- and deliberately so. A config file is authored once
// and a typo there silently changes behaviour, so strictness is worth the friction. An HTTP
// query string is assembled by clients, proxies and analytics tools that routinely append their
// own parameters, and rejecting anything unrecognised would break a caller for reasons that have
// nothing to do with them.
func filters(q url.Values, v *validationError) postgres.Filters {
	get := func(key string) string {
		raw := strings.TrimSpace(q.Get(key))
		if raw == "" {
			return ""
		}
		// Filter values are compared against Kubernetes object names, so anything that cannot be
		// one cannot match. Rejecting is more useful than returning an empty result, which is
		// indistinguishable from "nothing costs anything here".
		//
		// This is defence in depth rather than the injection control -- values are bound as SQL
		// PLACEHOLDERS, so they are never parsed as SQL. The primary control is that the column
		// NAME comes from an allow-list.
		if len(raw) > 253 {
			v.add(key, "must be at most 253 characters")
			return ""
		}
		return raw
	}

	f := postgres.Filters{
		Namespace:    get("namespace"),
		Team:         get("team"),
		Environment:  get("environment"),
		CostCentre:   get("cost_centre"),
		WorkloadKind: get("workload_kind"),
		WorkloadName: get("workload_name"),
		Node:         get("node"),
		InstanceType: get("instance_type"),
		CapacityType: get("capacity_type"),
	}

	if raw := strings.TrimSpace(q.Get("estimated_only")); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			v.add("estimated_only", "must be true or false")
		} else {
			f.EstimatedOnly = b
		}
	}

	// capacity_type has only two meaningful values, so a typo is worth catching -- "Spot" would
	// otherwise match nothing and look like an absence of spot capacity.
	if f.CapacityType != "" && f.CapacityType != "spot" && f.CapacityType != "on-demand" {
		v.add("capacity_type", "must be spot or on-demand")
	}

	return f
}

// limitParam parses a positive bounded integer.
func limitParam(q url.Values, key string, def, max int, v *validationError) int {
	raw := strings.TrimSpace(q.Get(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		v.add(key, "must be an integer")
		return def
	}
	if n < 1 {
		v.add(key, "must be at least 1")
		return def
	}
	if n > max {
		// REFUSED, not clamped. Silently returning 500 rows to someone who asked for 10,000 makes
		// them believe they have the whole result set, and a client paginating on "did I get
		// fewer rows than I asked for?" would stop early and lose data.
		v.add(key, "must be at most %d", max)
		return def
	}
	return n
}

// summaryParams parses a cost-summary request.
func summaryParams(r *http.Request) (postgres.CostSummaryParams, *validationError) {
	q := r.URL.Query()
	v := &validationError{}

	from, to := timeRange(q, v)

	// group_by defaults to namespace: the grouping almost every caller wants, and the one the
	// dashboard leads with.
	groupBy := postgres.GroupBy(strings.TrimSpace(q.Get("group_by")))
	if groupBy == "" {
		groupBy = postgres.GroupByNamespace
	}
	if !postgres.ValidGroupBy(groupBy) {
		// The valid values come from the same map the query uses, so this message cannot drift
		// out of date when a grouping is added.
		v.add("group_by", "must be one of: %s", strings.Join(postgres.GroupByOptions(), ", "))
	}

	sortBy := postgres.SortField(strings.TrimSpace(q.Get("sort")))
	if sortBy == "" {
		// Descending total cost by default. A cost report is read to find what is expensive.
		sortBy = postgres.SortByTotalCost
	}
	if !postgres.ValidSortField(sortBy) {
		v.add("sort", "must be one of: %s", strings.Join(postgres.SortFieldOptions(), ", "))
	}

	descending := true
	if raw := strings.TrimSpace(q.Get("order")); raw != "" {
		switch strings.ToLower(raw) {
		case "asc":
			descending = false
		case "desc":
			descending = true
		default:
			v.add("order", "must be asc or desc")
		}
	}

	limit := limitParam(q, "limit", 100, 1000, v)
	f := filters(q, v)

	if !v.empty() {
		return postgres.CostSummaryParams{}, v
	}
	return postgres.CostSummaryParams{
		From: from, To: to,
		GroupBy: groupBy, Filters: f,
		SortBy: sortBy, Descending: descending, Limit: limit,
	}, nil
}

// statsParams parses a recommendations request.
//
// The default range is SEVEN DAYS rather than the 24 hours the cost endpoints use, and that asymmetry
// is deliberate. A cost figure for the last day is a useful answer on its own; a RECOMMENDATION from
// one day of data cannot see a weekly pattern, so a batch job that runs on Sundays looks abandoned.
// Defaulting wider means the advice is more trustworthy without the caller having to know to ask.
func statsParams(r *http.Request) (postgres.ContainerStatsParams, *validationError) {
	q := r.URL.Query()
	v := &validationError{}

	from, to := timeRangeDefault(q, v, 7*24*time.Hour)
	limit := limitParam(q, "limit", 200, 1000, v)
	f := filters(q, v)

	if !v.empty() {
		return postgres.ContainerStatsParams{}, v
	}
	return postgres.ContainerStatsParams{From: from, To: to, Filters: f, Limit: limit}, nil
}

// allocationsParams parses a raw-allocations page request.
func allocationsParams(r *http.Request) (postgres.AllocationsParams, *validationError) {
	q := r.URL.Query()
	v := &validationError{}

	from, to := timeRange(q, v)
	limit := limitParam(q, "limit", 100, 500, v)
	f := filters(q, v)

	var cursor *postgres.Cursor
	if raw := strings.TrimSpace(q.Get("cursor")); raw != "" {
		c, err := postgres.DecodeCursor(raw)
		if err != nil {
			// A 400 rather than a silent restart from the first page. A client looping on a stale
			// cursor would otherwise re-read page one forever with nothing to explain why.
			//
			// The parse error is NOT echoed: it describes our internal cursor format, and a client
			// must not learn to depend on that shape.
			v.add("cursor", "is not a valid cursor; omit it to start from the first page")
		} else {
			cursor = &c
		}
	}

	if !v.empty() {
		return postgres.AllocationsParams{}, v
	}
	return postgres.AllocationsParams{
		From: from, To: to, Filters: f, Limit: limit, Cursor: cursor,
	}, nil
}
