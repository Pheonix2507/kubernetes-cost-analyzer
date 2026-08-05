package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/httpapi/middleware"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/logging"
)

// discardLogger builds a real logger that writes nowhere.
//
// Tests exercise the SAME logging path production uses. Passing nil would mean the
// logging statements never execute, so a nil-pointer bug inside a log call -- in an
// error path, the code least likely to be covered -- would ship unnoticed.
// io.Discard gives the real code path with no output noise.
func discardLogger() *slog.Logger {
	return logging.New(logging.Options{Output: io.Discard})
}

type stubChecker struct {
	name string
	err  error
}

func (s stubChecker) Name() string                  { return s.name }
func (s stubChecker) Check(_ context.Context) error { return s.err }

// -----------------------------------------------------------------------------
// Liveness
// -----------------------------------------------------------------------------

func TestHealthz_AlwaysOK(t *testing.T) {
	t.Parallel()

	// Note the deliberately BROKEN dependency. Liveness must ignore it entirely --
	// this is the test that enforces the liveness/readiness separation, and it fails
	// if someone "helpfully" makes /healthz check the database.
	agg := health.NewAggregator(time.Second, stubChecker{name: "postgres", err: errors.New("down")})
	srv := NewRouter(RouterOptions{
		Log: discardLogger(), Readiness: agg,
		Inventory: &stubInventory{}, Pricer: defaultStubPricer(), Reports: &stubReports{},
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d even with a failing dependency", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
}

// -----------------------------------------------------------------------------
// Readiness
// -----------------------------------------------------------------------------

func TestReadyz_AllDependenciesUp(t *testing.T) {
	t.Parallel()

	agg := health.NewAggregator(time.Second, stubChecker{name: "postgres"})
	srv := NewRouter(RouterOptions{
		Log: discardLogger(), Readiness: agg,
		Inventory: &stubInventory{}, Pricer: defaultStubPricer(), Reports: &stubReports{},
	})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var report health.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("response is not valid JSON: %v (body: %s)", err, rec.Body.String())
	}
	if report.Status != health.StatusUp {
		t.Errorf("report status = %q, want %q", report.Status, health.StatusUp)
	}
}

// TestReadyz_DependencyDownReturns503 backs Phase 0's verification step 8: stopping
// Postgres must take this replica out of service without restarting it.
func TestReadyz_DependencyDownReturns503(t *testing.T) {
	t.Parallel()

	agg := health.NewAggregator(time.Second,
		stubChecker{name: "postgres", err: errors.New("connection refused")},
	)
	srv := NewRouter(RouterOptions{
		Log: discardLogger(), Readiness: agg,
		Inventory: &stubInventory{}, Pricer: defaultStubPricer(), Reports: &stubReports{},
	})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	// 503, not 500 -- see the reasoning in handleReady.
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var report health.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if report.Status != health.StatusDown {
		t.Errorf("report status = %q, want %q", report.Status, health.StatusDown)
	}
	// The body must NAME the failing dependency: the kubelet records it in pod
	// events, which is what makes "readiness probe failed" actionable.
	if len(report.Checks) != 1 || report.Checks[0].Name != "postgres" {
		t.Fatalf("checks = %+v, want one entry named postgres", report.Checks)
	}
	if report.Checks[0].Error == "" {
		t.Error("failing check carried no error message")
	}
}

// -----------------------------------------------------------------------------
// Routing
// -----------------------------------------------------------------------------

// TestRouting covers what the Go 1.22+ ServeMux gives us for free, and is the
// evidence that we do not yet need a third-party router.
func TestRouting(t *testing.T) {
	t.Parallel()

	srv := NewRouter(RouterOptions{
		Log: discardLogger(), Readiness: health.NewAggregator(time.Second),
		Inventory: &stubInventory{}, Pricer: defaultStubPricer(), Reports: &stubReports{},
	})

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"liveness", http.MethodGet, "/healthz", http.StatusOK},
		{"readiness", http.MethodGet, "/readyz", http.StatusOK},
		{"version", http.MethodGet, "/version", http.StatusOK},
		// Method-qualified patterns produce a 405 automatically. Without them this
		// would be a 200, silently treating a POST as a GET.
		{"wrong method", http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{"unknown path", http.MethodGet, "/does-not-exist", http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.wantStatus)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Middleware
// -----------------------------------------------------------------------------

func TestRequestID_GeneratedWhenAbsent(t *testing.T) {
	t.Parallel()

	srv := NewRouter(RouterOptions{
		Log: discardLogger(), Readiness: health.NewAggregator(time.Second),
		Inventory: &stubInventory{}, Pricer: defaultStubPricer(), Reports: &stubReports{},
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if got := rec.Header().Get(middleware.RequestIDHeader); got == "" {
		t.Error("no request ID in response headers; a client cannot quote one when reporting a fault")
	}
}

// TestRequestID_HonoursInboundValue covers cross-service correlation: an ID minted
// upstream must survive, or one user action cannot be traced across services.
func TestRequestID_HonoursInboundValue(t *testing.T) {
	t.Parallel()

	const upstream = "abcdef0123456789"
	srv := NewRouter(RouterOptions{
		Log: discardLogger(), Readiness: health.NewAggregator(time.Second),
		Inventory: &stubInventory{}, Pricer: defaultStubPricer(), Reports: &stubReports{},
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(middleware.RequestIDHeader, upstream)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if got := rec.Header().Get(middleware.RequestIDHeader); got != upstream {
		t.Errorf("request ID = %q, want the inbound %q preserved", got, upstream)
	}
}

// TestRecover_TurnsPanicIntoResponse is arguably the most important test here.
// Without the Recover middleware, a panic in ANY handler kills the whole process and
// every other in-flight request with it.
//
// It builds its own chain rather than using NewRouter, because no real route panics
// (and none should).
func TestRecover_TurnsPanicIntoResponse(t *testing.T) {
	t.Parallel()

	panicking := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		// A nil map write: a realistic accidental panic, not a contrived one.
		// govet's nilness check correctly spots it -- that is the whole point of
		// this handler, so the diagnostic is suppressed here and only here.
		var m map[string]string
		m["boom"] = "yes" //nolint:govet,staticcheck // deliberate panic; it IS the behaviour under test
	})

	log := discardLogger()
	handler := middleware.Chain(panicking,
		middleware.RequestID,
		middleware.RequestLogger(log),
		middleware.Recover(log),
	)

	rec := httptest.NewRecorder()
	// Without Recover this call would panic and take the test process down, rather
	// than returning -- which is exactly the production failure mode.
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("panic response is not our JSON error envelope: %v (body: %s)", err, rec.Body.String())
	}
	if body.Error.Code != "internal_error" {
		t.Errorf("error code = %q, want %q", body.Error.Code, "internal_error")
	}

	// The client must not receive the panic value or a stack trace: those leak SQL,
	// file paths and internal hostnames.
	got := rec.Body.String()
	for _, leak := range []string{"assignment to entry in nil map", ".go:", "goroutine"} {
		if strings.Contains(got, leak) {
			t.Errorf("response leaks internal detail %q: %s", leak, got)
		}
	}
}

// -----------------------------------------------------------------------------
// Log volume from probes
// -----------------------------------------------------------------------------

// TestProbeLogging is a REGRESSION TEST for an operational bug rather than a functional
// one, which makes it easy to miss in review and expensive in production.
//
// THE BUG: the access logger mapped any 5xx to ERROR, uniformly. /readyz correctly
// returns 503 while a dependency is down, and the kubelet polls it every few seconds on
// every replica. So a database blip produced hundreds of ERROR lines per minute for a
// condition the system was already handling correctly by draining traffic.
//
// That is how error-log alerting gets muted, and a muted alert cannot warn you about the
// next real problem. Probes are therefore DEBUG when passing and WARN when failing.
func TestProbeLogging(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		path        string
		failing     bool
		wantLevel   string // "" means nothing should be logged at info level
		description string
	}{
		{
			name: "passing probe is silent at info level", path: "/readyz", failing: false,
			wantLevel:   "",
			description: "polled every few seconds forever; at INFO it is pure noise",
		},
		{
			name: "failing probe warns, does not error", path: "/readyz", failing: true,
			wantLevel:   "WARN",
			description: "a real signal, but the system is handling it as designed",
		},
		{
			name: "passing liveness is silent too", path: "/healthz", failing: false,
			wantLevel: "",
		},
		{
			name: "real traffic is still logged at info", path: "/api/v1/nodes", failing: false,
			wantLevel:   "INFO",
			description: "the demotion must apply ONLY to probe paths",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			// Info level, as production runs. A DEBUG line must therefore not appear.
			log := logging.New(logging.Options{Level: "info", Output: &buf})

			var checkers []health.Checker
			if tt.failing {
				checkers = append(checkers, stubChecker{name: "postgres", err: errors.New("down")})
			}
			srv := NewRouter(RouterOptions{
				Log: log, Readiness: health.NewAggregator(time.Second, checkers...),
				Inventory: &stubInventory{}, Pricer: defaultStubPricer(), Reports: &stubReports{},
			})

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))

			out := buf.String()
			if tt.wantLevel == "" {
				if strings.Contains(out, "http request") {
					t.Errorf("probe %s produced an access log line at info level: %s\nwhy: %s",
						tt.path, out, tt.description)
				}
				return
			}
			if !strings.Contains(out, "level="+tt.wantLevel) {
				t.Errorf("%s logged at the wrong level; want %s, got: %s\nwhy: %s",
					tt.path, tt.wantLevel, out, tt.description)
			}
			// The specific regression: a failing probe must NOT be ERROR.
			if tt.wantLevel == "WARN" && strings.Contains(out, "level=ERROR") {
				t.Errorf("failing probe logged at ERROR; this is the log-spam bug: %s", out)
			}
		})
	}
}
