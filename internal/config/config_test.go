package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// allKeys is every environment variable Load reads. Keeping this list here means a
// new variable added to Load without a corresponding test entry shows up as a
// hermeticity failure rather than as a mysteriously passing test.
var allKeys = []string{
	"APP_ENV", "LOG_LEVEL",
	"API_HTTP_ADDR", "API_READ_TIMEOUT", "API_WRITE_TIMEOUT",
	"API_IDLE_TIMEOUT", "API_SHUTDOWN_TIMEOUT",
	"DATABASE_URL", "DB_MAX_OPEN_CONNS", "DB_MIN_IDLE_CONNS", "DB_CONN_MAX_LIFETIME",
	"PROMETHEUS_URL", "PROMETHEUS_TIMEOUT",
	"COLLECTOR_INTERVAL", "COLLECTOR_WORKERS",
}

// setEnv makes the environment hermetic, then applies the test's own values.
//
// WHY THIS MATTERS: Load reads ambient process state. Without clearing first, a
// developer who happens to have DATABASE_URL exported in their shell gets different
// test results from CI -- the classic "passes on my machine" failure. We blank every
// known key so each case starts from a known baseline.
//
// t.Setenv cannot UNSET a variable, only set it. Blanking works because loader.str
// treats "" as absent (see the comment on that method) -- a small design decision
// that pays off directly in test ergonomics.
//
// NOTE: t.Setenv registers automatic cleanup, restoring the previous value when the
// test ends. It also forbids t.Parallel() in the same test: the environment is
// process-global, so parallel tests mutating it would race. That is why no test in
// this file calls t.Parallel().
func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, k := range allKeys {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

// validEnv is the minimum needed for Load to succeed: only DATABASE_URL is required.
func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL": "postgres://kca:pw@localhost:55432/kca_dev?sslmode=disable",
	}
}

func TestLoad_AppliesDefaults(t *testing.T) {
	setEnv(t, validEnv())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	// Table of expectations rather than a wall of ifs: adding a new default is one
	// line, and a failure names the field that drifted.
	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"Env", cfg.Env, EnvDevelopment},
		{"LogLevel", cfg.LogLevel, "info"},
		{"API.Addr", cfg.API.Addr, ":8080"},
		{"API.ReadTimeout", cfg.API.ReadTimeout, 10 * time.Second},
		{"API.WriteTimeout", cfg.API.WriteTimeout, 15 * time.Second},
		{"API.IdleTimeout", cfg.API.IdleTimeout, 60 * time.Second},
		{"API.ShutdownTimeout", cfg.API.ShutdownTimeout, 15 * time.Second},
		{"Database.MaxOpenConns", cfg.Database.MaxOpenConns, int32(20)},
		{"Database.MinIdleConns", cfg.Database.MinIdleConns, int32(5)},
		{"Database.ConnMaxLifetime", cfg.Database.ConnMaxLifetime, 30 * time.Minute},
		{"Prometheus.URL", cfg.Prometheus.URL, "http://localhost:19090"},
		{"Prometheus.Timeout", cfg.Prometheus.Timeout, 30 * time.Second},
		{"Collector.Interval", cfg.Collector.Interval, 5 * time.Minute},
		{"Collector.Workers", cfg.Collector.Workers, 4},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
}

func TestLoad_OverridesFromEnvironment(t *testing.T) {
	env := validEnv()
	env["APP_ENV"] = "production"
	env["LOG_LEVEL"] = "warn"
	env["API_HTTP_ADDR"] = ":9999"
	env["API_SHUTDOWN_TIMEOUT"] = "25s"
	env["COLLECTOR_WORKERS"] = "16"
	env["COLLECTOR_INTERVAL"] = "1h30m"
	setEnv(t, env)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if !cfg.IsProduction() {
		t.Errorf("IsProduction() = false, want true for APP_ENV=production")
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "warn")
	}
	if cfg.API.Addr != ":9999" {
		t.Errorf("API.Addr = %q, want %q", cfg.API.Addr, ":9999")
	}
	if cfg.API.ShutdownTimeout != 25*time.Second {
		t.Errorf("API.ShutdownTimeout = %v, want 25s", cfg.API.ShutdownTimeout)
	}
	if cfg.Collector.Workers != 16 {
		t.Errorf("Collector.Workers = %d, want 16", cfg.Collector.Workers)
	}
	if cfg.Collector.Interval != 90*time.Minute {
		t.Errorf("Collector.Interval = %v, want 1h30m", cfg.Collector.Interval)
	}
}

func TestLoad_MissingRequiredVariable(t *testing.T) {
	setEnv(t, map[string]string{}) // DATABASE_URL deliberately absent

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded with no DATABASE_URL, want an error")
	}
	// errors.Is must see through both the fmt.Errorf wrapping in Load AND the
	// errors.Join in loader.err. This asserts the wrapping chain is intact -- if
	// someone changes a %w to a %v, this test fails.
	if !errors.Is(err, ErrRequired) {
		t.Errorf("error = %v, want it to wrap ErrRequired", err)
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error %q does not name the offending variable", err)
	}
}

// TestLoad_BlankValueIsTreatedAsUnset pins down the "" == unset decision. It is a
// deliberate behaviour, not an accident, so it gets a test to stop a future refactor
// from quietly changing it.
func TestLoad_BlankValueIsTreatedAsUnset(t *testing.T) {
	env := validEnv()
	env["API_HTTP_ADDR"] = "" // as an empty ConfigMap key would arrive
	setEnv(t, env)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}
	if cfg.API.Addr != ":8080" {
		t.Errorf("API.Addr = %q, want the default %q for a blank value", cfg.API.Addr, ":8080")
	}
}

func TestLoad_InvalidValues(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		wantIs    error
		wantInMsg string
	}{
		{
			name:      "unparseable duration",
			env:       map[string]string{"API_READ_TIMEOUT": "ten seconds"},
			wantInMsg: "API_READ_TIMEOUT",
		},
		{
			name:      "unparseable integer",
			env:       map[string]string{"COLLECTOR_WORKERS": "many"},
			wantInMsg: "COLLECTOR_WORKERS",
		},
		{
			name:      "unknown environment",
			env:       map[string]string{"APP_ENV": "staging"},
			wantIs:    ErrInvalid,
			wantInMsg: "APP_ENV",
		},
		{
			name:      "unknown log level",
			env:       map[string]string{"LOG_LEVEL": "verbose"},
			wantIs:    ErrInvalid,
			wantInMsg: "LOG_LEVEL",
		},
		{
			name:      "negative worker count",
			env:       map[string]string{"COLLECTOR_WORKERS": "-1"},
			wantIs:    ErrInvalid,
			wantInMsg: "COLLECTOR_WORKERS",
		},
		{
			name:      "zero shutdown timeout",
			env:       map[string]string{"API_SHUTDOWN_TIMEOUT": "0s"},
			wantIs:    ErrInvalid,
			wantInMsg: "API_SHUTDOWN_TIMEOUT",
		},
		{
			// Nonsensical pool sizing that pgx would silently clamp instead of
			// rejecting, so we must catch it ourselves.
			name: "idle floor above open ceiling",
			env: map[string]string{
				"DB_MAX_OPEN_CONNS": "5",
				"DB_MIN_IDLE_CONNS": "10",
			},
			wantIs:    ErrInvalid,
			wantInMsg: "DB_MIN_IDLE_CONNS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv()
			for k, v := range tt.env {
				env[k] = v
			}
			setEnv(t, env)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() succeeded, want an error for %s", tt.name)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error = %v, want it to wrap %v", err, tt.wantIs)
			}
			if !strings.Contains(err.Error(), tt.wantInMsg) {
				t.Errorf("error %q does not mention %q", err, tt.wantInMsg)
			}
		})
	}
}

// TestLoad_ReportsAllErrorsAtOnce is the test that justifies errors.Join.
//
// An operator with three broken variables should learn about all three from one
// failed start, not discover them across three deploy cycles. If someone
// "simplifies" Validate to return on first error, this test catches it.
func TestLoad_ReportsAllErrorsAtOnce(t *testing.T) {
	setEnv(t, map[string]string{
		// DATABASE_URL missing entirely (1)
		"APP_ENV":           "nonsense", // (2)
		"LOG_LEVEL":         "loud",     // (3)
		"COLLECTOR_WORKERS": "0",        // (4)
	})

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded with four broken variables, want an error")
	}

	msg := err.Error()
	for _, want := range []string{"DATABASE_URL", "APP_ENV", "LOG_LEVEL", "COLLECTOR_WORKERS"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message is missing %q; got:\n%s", want, msg)
		}
	}
}
