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
	"APP_ENV", "LOG_LEVEL", "CLUSTER_NAME",
	"API_HTTP_ADDR", "API_READ_TIMEOUT", "API_WRITE_TIMEOUT",
	"API_IDLE_TIMEOUT", "API_SHUTDOWN_TIMEOUT",
	"API_KEYS", "API_RATE_LIMIT_PER_SECOND", "API_RATE_LIMIT_BURST",
	"DATABASE_URL", "DB_MAX_OPEN_CONNS", "DB_MIN_IDLE_CONNS", "DB_CONN_MAX_LIFETIME",
	"KUBECONFIG", "KUBE_CONTEXT", "KUBE_RESYNC_INTERVAL", "KUBE_CACHE_SYNC_TIMEOUT",
	"KUBE_QPS", "KUBE_BURST",
	"PRICING_CATALOGUE_PATH",
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
		{"ClusterName", cfg.ClusterName, "default"},
		{"API.Addr", cfg.API.Addr, ":8080"},
		{"API.ReadTimeout", cfg.API.ReadTimeout, 10 * time.Second},
		{"API.WriteTimeout", cfg.API.WriteTimeout, 15 * time.Second},
		{"API.IdleTimeout", cfg.API.IdleTimeout, 60 * time.Second},
		{"API.ShutdownTimeout", cfg.API.ShutdownTimeout, 15 * time.Second},
		{"API.RateLimitPerSecond", cfg.API.RateLimitPerSecond, float64(20)},
		{"API.RateLimitBurst", cfg.API.RateLimitBurst, 40},
		{"Database.MaxOpenConns", cfg.Database.MaxOpenConns, int32(20)},
		{"Database.MinIdleConns", cfg.Database.MinIdleConns, int32(5)},
		{"Database.ConnMaxLifetime", cfg.Database.ConnMaxLifetime, 30 * time.Minute},
		{"Kube.CacheSyncTimeout", cfg.Kube.CacheSyncTimeout, 60 * time.Second},
		{"Kube.ResyncInterval", cfg.Kube.ResyncInterval, time.Duration(0)},
		{"Kube.QPS", cfg.Kube.QPS, float32(50)},
		{"Kube.Burst", cfg.Kube.Burst, 100},
		{"Pricing.CataloguePath", cfg.Pricing.CataloguePath, "deploy/pricing/catalogue.yaml"},
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
	// Required in production: see the API_KEYS validation. The test previously passed only
	// because that rule did not exist yet.
	env["API_KEYS"] = "0123456789abcdef0123456789abcdef"
	// Also required in production, and this test caught the rule the moment it was added -- it had been
	// passing with the placeholder cluster name, which is exactly the deployment an audit found writing
	// 74,925 rows attributed to a cluster called "default".
	env["CLUSTER_NAME"] = "prod-eu-west-1"
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

// TestLoad_APIKeysAreParsedAsAList covers key rotation: two keys must both be accepted so a new
// one can be introduced before the old is withdrawn.
func TestLoad_APIKeysAreParsedAsAList(t *testing.T) {
	env := validEnv()
	env["API_KEYS"] = " 0123456789abcdef0123456789abcdef , fedcba9876543210fedcba9876543210 ,, "
	setEnv(t, env)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}
	if len(cfg.API.APIKeys) != 2 {
		t.Fatalf("got %d keys, want 2 (blanks and whitespace must be discarded): %#v",
			len(cfg.API.APIKeys), cfg.API.APIKeys)
	}
	// Trimmed, or a key with a stray space would never match what a client sends.
	if cfg.API.APIKeys[0] != "0123456789abcdef0123456789abcdef" {
		t.Errorf("key not trimmed: %q", cfg.API.APIKeys[0])
	}
}

// TestLoad_DevelopmentAllowsNoKeys covers the deliberate asymmetry: development must not need
// ceremony, and production must not be able to run open.
func TestLoad_DevelopmentAllowsNoKeys(t *testing.T) {
	setEnv(t, validEnv()) // APP_ENV defaults to development, API_KEYS blank

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() rejected a development config with no API keys: %v", err)
	}
	if len(cfg.API.APIKeys) != 0 {
		t.Errorf("expected no keys, got %v", cfg.API.APIKeys)
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
			// The rule that matters most in this table: an unauthenticated cost API serves spend
			// data to anyone who can reach it, and a healthy startup would say nothing about it.
			name:      "production without API keys",
			env:       map[string]string{"APP_ENV": "production"},
			wantIs:    ErrRequired,
			wantInMsg: "API_KEYS",
		},
		{
			name: "API key too short to be a secret",
			env: map[string]string{
				"APP_ENV":  "production",
				"API_KEYS": "short",
			},
			wantIs:    ErrInvalid,
			wantInMsg: "API_KEYS",
		},
		{
			name:      "negative rate limit",
			env:       map[string]string{"API_RATE_LIMIT_PER_SECOND": "-1"},
			wantIs:    ErrInvalid,
			wantInMsg: "API_RATE_LIMIT_PER_SECOND",
		},
		{
			// REGRESSION: net/http treats a zero timeout as NO timeout, silently
			// removing the Slowloris protection server.go claims to provide.
			name:      "zero read timeout",
			env:       map[string]string{"API_READ_TIMEOUT": "0s"},
			wantIs:    ErrInvalid,
			wantInMsg: "API_READ_TIMEOUT",
		},
		{
			name:      "negative write timeout",
			env:       map[string]string{"API_WRITE_TIMEOUT": "-5s"},
			wantIs:    ErrInvalid,
			wantInMsg: "API_WRITE_TIMEOUT",
		},
		{
			name:      "zero idle timeout",
			env:       map[string]string{"API_IDLE_TIMEOUT": "0s"},
			wantIs:    ErrInvalid,
			wantInMsg: "API_IDLE_TIMEOUT",
		},
		{
			name:      "zero prometheus timeout",
			env:       map[string]string{"PROMETHEUS_TIMEOUT": "0s"},
			wantIs:    ErrInvalid,
			wantInMsg: "PROMETHEUS_TIMEOUT",
		},
		{
			// REGRESSION: int32(l.integer(...)) narrowed SILENTLY. 2^32+1 became 1, so
			// the service started with a one-connection pool and no error anywhere.
			name:      "pool size beyond int32 must not wrap silently",
			env:       map[string]string{"DB_MAX_OPEN_CONNS": "4294967297"},
			wantIs:    ErrInvalid,
			wantInMsg: "DB_MAX_OPEN_CONNS",
		},
		{
			name:      "pool size at the int32 boundary",
			env:       map[string]string{"DB_MAX_OPEN_CONNS": "2147483648"},
			wantIs:    ErrInvalid,
			wantInMsg: "DB_MAX_OPEN_CONNS",
		},
		{
			// REGRESSION: float32(huge float64) becomes +Inf, which passes a naive
			// "must be positive" check and then disables rate limiting entirely.
			name:      "qps beyond float32 must not become +Inf",
			env:       map[string]string{"KUBE_QPS": "1e40", "KUBE_BURST": "100"},
			wantIs:    ErrInvalid,
			wantInMsg: "KUBE_QPS",
		},
		{
			// Burst below QPS makes the QPS setting meaningless.
			name: "kube burst below qps",
			env: map[string]string{
				"KUBE_QPS":   "50",
				"KUBE_BURST": "10",
			},
			wantIs:    ErrInvalid,
			wantInMsg: "KUBE_BURST",
		},
		{
			name:      "unparseable float",
			env:       map[string]string{"KUBE_QPS": "fast"},
			wantInMsg: "KUBE_QPS",
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

// TestValidate_ProductionMustNameItsCluster is a REGRESSION TEST for an audit finding.
//
// CLUSTER_NAME defaulted to the placeholder "default" and validation rejected only BLANK, so a
// deployment that never set the variable passed every check. The live database had 74,925 rows
// attributed to cluster "default", and the monthly statements read "cluster/default".
//
// Why this is worth failing startup over rather than warning about. Migration 000001 states the rule:
// cluster_name is denormalised onto every fact row and must be stable for the life of the cluster,
// because changing it makes yesterday's rows look like a different cluster and a report grouped by
// cluster shows one estate as two. The placeholder is therefore a value somebody will eventually
// correct, and correcting it splits history permanently.
//
// Phase 11 makes it worse: two real clusters that both defaulted would MERGE, silently summing
// unrelated spend. A wrong total is worse than a refused startup.
func TestValidate_ProductionMustNameItsCluster(t *testing.T) {
	tests := []struct {
		name        string
		env         string
		clusterName string
		wantErr     bool
		why         string
	}{
		{"production with the placeholder", "production", "default", true,
			"the deployment never set it, and every cost row would carry a name nobody recognises"},
		{"production with a real name", "production", "prod-eu-west-1", false,
			"explicitly named, which is all the rule asks for"},
		{"development with the placeholder", "development", "default", false,
			"`make run-api` must need no ceremony, exactly as with API_KEYS"},
		{"production with a name that merely contains the placeholder", "production", "default-cluster-eu", false,
			"the check is equality, not a substring match -- a real cluster may legitimately be called " +
				"default-something, and refusing it would be a rule nobody could satisfy"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv()
			env["APP_ENV"] = tt.env
			env["CLUSTER_NAME"] = tt.clusterName
			if tt.env == "production" {
				env["API_KEYS"] = "0123456789abcdef0123456789abcdef"
			}
			setEnv(t, env)

			_, err := Load()
			named := err != nil && strings.Contains(err.Error(), "CLUSTER_NAME")
			if named != tt.wantErr {
				t.Errorf("CLUSTER_NAME error = %v, want %v\nwhy: %s\nerr: %v",
					named, tt.wantErr, tt.why, err)
			}
		})
	}
}
