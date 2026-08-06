// Package config loads, parses and validates every runtime setting the
// application reads from its environment.
//
// WHY THIS PACKAGE EXISTS
// -----------------------
// Configuration read ad hoc via os.Getenv scattered through the codebase has four
// problems, and all four cause production incidents:
//
//  1. A typo in a variable name silently yields "" and the service starts anyway,
//     misbehaving in a way that looks like a code bug.
//  2. Nobody can enumerate what the service is configurable by, so nobody can
//     safely deploy it to a new environment.
//  3. Parse failures surface on FIRST USE -- hours after boot, in whichever request
//     unluckily needed that value -- rather than at startup.
//  4. Nothing is testable, because behaviour depends on ambient process state.
//
// So configuration is loaded exactly once, at startup, in one place, and the process
// REFUSES TO START if anything is wrong. A service that dies immediately with a
// clear message is infinitely easier to operate than one that starts and then
// behaves strangely. In Kubernetes terms: fail the container, let the CrashLoopBackOff
// and its logs tell the operator precisely what to fix.
//
// WHY NO CONFIG LIBRARY (viper, koanf)
// ------------------------------------
// This package deliberately imports nothing outside the standard library. It is
// ~200 lines, has no global state, and is trivially testable with t.Setenv. viper
// is powerful but pulls in a large dependency tree, encourages a package-level
// global (viper.GetString anywhere = the same untestable os.Getenv problem with
// extra steps), and its precedence rules between files, flags and env are a common
// source of "why is this value not what I set" confusion. Reach for a library when
// we genuinely need live reload or multi-format merging. We do not.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors. Callers can test for these with errors.Is even after wrapping,
// which is why they are declared values rather than strings formatted at the site.
var (
	// ErrRequired means a mandatory variable was absent or blank.
	ErrRequired = errors.New("required environment variable is not set")
	// ErrInvalid means a variable was present but its value is unacceptable.
	ErrInvalid = errors.New("invalid value")
)

// Environment names a deployment environment.
//
// A named string type rather than a bare string: it makes the function signature
// self-documenting, and it means an accidental swap of two string arguments is a
// compile error instead of a runtime surprise.
type Environment string

// The deployment environments we recognise. Anything else is rejected by Validate
// rather than silently treated as development.
const (
	EnvDevelopment Environment = "development"
	EnvProduction  Environment = "production"
)

// defaultClusterName is the development placeholder for CLUSTER_NAME.
//
// A named constant rather than a literal, and that is the whole reason it exists: "is this still the
// placeholder?" is a question Validate has to be able to ask, and the same string written in the loader
// and in the validator is two places for them to drift apart.
const defaultClusterName = "default"

// Config is the fully validated configuration for every binary in this project.
//
// It is grouped into nested structs rather than being one flat list of thirty
// fields. That is not cosmetic: it means a function that only needs database
// settings takes a config.Database, so its signature states exactly what it
// depends on. Passing the whole *Config everywhere would let any function reach
// any setting, and the dependency graph becomes unknowable.
type Config struct {
	Env      Environment
	LogLevel string

	// ClusterName names the cluster on every fact row. See defaultClusterName for why production must
	// set it explicitly.
	//
	// ClusterName is denormalised onto every fact row, so it must be stable for the life of
	// the cluster: changing it makes yesterday's rows look like they belong to a different
	// cluster, and a report grouping by cluster would show one estate as two.
	ClusterName string

	API        API
	Database   Database
	Kube       Kube
	Pricing    Pricing
	Prometheus Prometheus
	Collector  Collector
}

// API holds the HTTP server settings.
type API struct {
	// Addr is a host:port string, e.g. ":8080".
	Addr string

	// ReadTimeout and WriteTimeout bound how long a single request may spend
	// reading its body or writing its response. Without these, a slow or
	// malicious client can hold a connection (and its goroutine, and its file
	// descriptor) open indefinitely -- the Slowloris attack, and also just what
	// happens on a bad mobile network. Go's defaults are NO TIMEOUT, which is
	// never what you want on a public listener.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// IdleTimeout bounds how long a keep-alive connection may sit unused.
	IdleTimeout time.Duration

	// ShutdownTimeout is how long in-flight requests get to finish after we
	// receive SIGTERM before we stop waiting and close their connections.
	//
	// This MUST stay comfortably below the pod's terminationGracePeriodSeconds
	// (default 30s). If it is longer, the kubelet sends SIGKILL while we are still
	// politely waiting, and we lose the in-flight requests we were trying to
	// protect -- strictly worse than not implementing graceful shutdown at all.
	ShutdownTimeout time.Duration

	// APIKeys are the accepted bearer tokens, comma-separated in the environment.
	//
	// A LIST rather than one key, so a key can be ROTATED without downtime: add the new one,
	// migrate clients, remove the old. With a single key, rotation means every client breaks at
	// the instant the value changes.
	APIKeys []string

	// RateLimitPerSecond and RateLimitBurst bound requests per client. Zero disables limiting.
	RateLimitPerSecond float64
	RateLimitBurst     int
}

// Database holds PostgreSQL connection settings.
type Database struct {
	URL string

	// MaxOpenConns caps connections from THIS process. Sizing matters more than it
	// looks: Postgres allocates a backend process per connection, so its practical
	// ceiling is a few hundred. Multiply your per-pod pool size by your replica
	// count -- 20 conns x 50 pods = 1000 connections and a refused database. This
	// is one of the most common ways a service that scaled fine at 5 replicas falls
	// over at 50.
	MaxOpenConns int32

	// MinIdleConns is a FLOOR, not a ceiling -- note the deliberate difference from
	// database/sql's MaxIdleConns, which caps how many idle connections are kept.
	//
	// pgx's pool has no idle ceiling. It keeps at least this many connections warm
	// so that a request arriving after a quiet period does not pay for a TCP
	// handshake plus TLS plus Postgres authentication (easily 5-50ms) before it can
	// run a query. Naming this MaxIdleConns, as an earlier version of this file did,
	// invites you to "lower it to save resources" and get the exact opposite of the
	// intended effect.
	MinIdleConns int32

	// ConnMaxLifetime recycles connections periodically. Without it, connections
	// pinned to one database instance survive failovers and load-balancer changes,
	// so traffic keeps flowing to the wrong place long after the topology moved.
	ConnMaxLifetime time.Duration
}

// Kube holds settings for talking to the Kubernetes API server.
type Kube struct {
	// ConfigPath is an explicit kubeconfig path. Empty means: try in-cluster
	// credentials first, then fall back to the standard kubeconfig discovery
	// (the KUBECONFIG env var, else ~/.kube/config). See internal/kube.RESTConfig.
	ConfigPath string

	// Context selects a kubeconfig context. Empty uses the current-context.
	//
	// Worth setting explicitly for anything destructive. It is not destructive here
	// -- we only ever read -- but "which cluster am I pointed at" is the question
	// behind most self-inflicted production incidents.
	Context string

	// ResyncInterval makes every informer re-deliver its entire cache to event
	// handlers on a timer. ZERO (the default) disables it.
	//
	// Resync is NOT a refresh: the watch already keeps the cache current, and a
	// resync re-sends objects that have not changed. It exists for CONTROLLERS,
	// which use it to re-reconcile drift between desired and actual state that
	// happened outside Kubernetes. We only read the cache, so a resync would burn
	// CPU re-processing identical objects to no effect.
	ResyncInterval time.Duration

	// CacheSyncTimeout bounds the initial List-and-populate on startup. Exceeding it
	// is fatal: an informer that never syncs would serve empty inventory and report
	// a cluster with no cost, which is worse than not starting.
	CacheSyncTimeout time.Duration

	// QPS and Burst are CLIENT-SIDE rate limits, and the defaults are a trap.
	//
	// client-go ships with QPS=5 and Burst=10. Those are per-process limits on
	// requests to the API server, enforced locally before anything leaves the
	// process. Exceed them and client-go SILENTLY QUEUES your calls -- no error, no
	// log, just latency that looks like a slow API server.
	//
	// It is the classic "why is my controller so slow" bug, and the answer is never
	// in the API server's metrics because the requests never arrived. Informers make
	// few calls once synced, but the initial List burst across several resource types
	// can brush against 5 QPS, so we raise it deliberately.
	QPS   float32
	Burst int
}

// Pricing holds settings for the cost rate catalogue.
type Pricing struct {
	// CataloguePath points at the YAML pricing catalogue.
	//
	// A FILE rather than environment variables, because prices are a table: dozens of
	// instance types each with several fields. Expressing that as env vars would mean
	// something like PRICING_M5_LARGE_HOURLY per entry, which is unreadable, undiffable and
	// impossible to review. In Kubernetes this becomes a mounted ConfigMap.
	CataloguePath string
}

// Prometheus holds settings for querying Prometheus (our usage data source).
type Prometheus struct {
	URL     string
	Timeout time.Duration
}

// Collector holds settings for the background collection loop.
type Collector struct {
	Interval time.Duration
	Workers  int

	// HTTPAddr is where the collector serves /metrics, /healthz and /readyz.
	//
	// WHY A BATCH-LIKE PROCESS NEEDS A LISTENER AT ALL
	// -----------------------------------------------
	// It has none of the usual reasons: it serves no users and answers no queries. It needs one anyway,
	// for two things that are the same problem wearing different clothes.
	//
	// YOU CANNOT SCRAPE A PROCESS THAT DOES NOT LISTEN. Instrumenting the collection loop is pointless
	// if Prometheus has nowhere to fetch the numbers from -- the metrics would exist in memory and be
	// read by nobody.
	//
	// AND YOU CANNOT PROBE ONE EITHER. Without a listener, Kubernetes has no liveness signal beyond
	// "the process has not exited", so a collector wedged on a hung Prometheus query looks perfectly
	// healthy forever. A wedged process that never restarts is worse than a crashing one, because
	// crashes are visible.
	//
	// :8081 rather than :8080, so the collector and the API can run side by side on one developer
	// machine. That is not merely convenient: `make run-api` and `make run-collector` in two terminals
	// is the normal local loop, and a port clash would make it impossible to observe both at once.
	HTTPAddr string
}

// IsProduction reports whether we are running in production.
//
// A method, not a comparison scattered at call sites: if we later add a "staging"
// environment that should behave production-like, there is exactly one line to change.
func (c *Config) IsProduction() bool { return c.Env == EnvProduction }

// Load reads configuration from the process environment, applies defaults, and
// validates the result.
//
// It returns a *Config (a pointer) because Config is a moderately large struct that
// is created once and read everywhere; copying it on every call would be wasteful
// and, worse, would let callers mutate their own copy and wonder why nothing changed.
//
// The returned error may wrap MULTIPLE problems -- see the note on errors.Join in
// (*Config).Validate.
func Load() (*Config, error) {
	l := &loader{}

	cfg := &Config{
		Env:         Environment(l.str("APP_ENV", string(EnvDevelopment))),
		LogLevel:    l.str("LOG_LEVEL", "info"),
		ClusterName: l.str("CLUSTER_NAME", defaultClusterName),

		API: API{
			Addr:            l.str("API_HTTP_ADDR", ":8080"),
			ReadTimeout:     l.duration("API_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:    l.duration("API_WRITE_TIMEOUT", 15*time.Second),
			IdleTimeout:     l.duration("API_IDLE_TIMEOUT", 60*time.Second),
			ShutdownTimeout: l.duration("API_SHUTDOWN_TIMEOUT", 15*time.Second),

			APIKeys: l.csv("API_KEYS"),
			// 20/sec sustained with a burst of 40 is generous for a dashboard and still bounds a
			// runaway script. Each request aggregates over a partitioned fact table, so the cost
			// of an unbounded caller falls on Postgres rather than here.
			RateLimitPerSecond: l.float("API_RATE_LIMIT_PER_SECOND", 20),
			RateLimitBurst:     l.integer("API_RATE_LIMIT_BURST", 40),
		},

		Database: Database{
			// Required: there is no sensible default for a database URL, and
			// guessing localhost would let a production deployment start up
			// pointed at nothing.
			URL:             l.required("DATABASE_URL"),
			MaxOpenConns:    l.int32("DB_MAX_OPEN_CONNS", 20),
			MinIdleConns:    l.int32("DB_MIN_IDLE_CONNS", 5),
			ConnMaxLifetime: l.duration("DB_CONN_MAX_LIFETIME", 30*time.Minute),
		},

		Kube: Kube{
			ConfigPath:       l.str("KUBECONFIG", ""),
			Context:          l.str("KUBE_CONTEXT", ""),
			ResyncInterval:   l.duration("KUBE_RESYNC_INTERVAL", 0),
			CacheSyncTimeout: l.duration("KUBE_CACHE_SYNC_TIMEOUT", 60*time.Second),
			QPS:              l.float32("KUBE_QPS", 50),
			Burst:            l.integer("KUBE_BURST", 100),
		},

		Pricing: Pricing{
			CataloguePath: l.str("PRICING_CATALOGUE_PATH", "deploy/pricing/catalogue.yaml"),
		},

		Prometheus: Prometheus{
			URL:     l.str("PROMETHEUS_URL", "http://localhost:19090"),
			Timeout: l.duration("PROMETHEUS_TIMEOUT", 30*time.Second),
		},

		Collector: Collector{
			Interval: l.duration("COLLECTOR_INTERVAL", 5*time.Minute),
			HTTPAddr: l.str("COLLECTOR_HTTP_ADDR", ":8081"),
			Workers:  l.integer("COLLECTOR_WORKERS", 4),
		},
	}

	// Report PARSE errors and VALIDATION errors together, not in two phases.
	//
	// An earlier version of this returned early on parse errors, before calling
	// Validate. TestLoad_ReportsAllErrorsAtOnce caught it: a missing DATABASE_URL
	// masked three other broken variables entirely, so the operator would have
	// fixed one, redeployed, and met the next -- exactly the whack-a-mole that
	// errors.Join exists to prevent. Splitting the phases quietly defeated the
	// design.
	//
	// There is no double-reporting risk from joining them: a variable that fails to
	// parse falls back to its (valid) default, so Validate has nothing to add
	// about it.
	if err := errors.Join(l.err(), cfg.Validate()); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// validLogLevels is a set.
//
// map[string]struct{} rather than map[string]bool: struct{} occupies zero bytes, so
// the map stores only keys. A common and idiomatic Go trick for sets.
var validLogLevels = map[string]struct{}{
	"debug": {}, "info": {}, "warn": {}, "error": {},
}

// Validate checks semantic constraints that parsing alone cannot catch.
//
// WHY errors.Join AND NOT AN EARLY RETURN
// ---------------------------------------
// Returning on the first problem means an operator fixes one variable, redeploys,
// waits for the rollout, and discovers the next one. With five mistakes that is five
// deploy cycles. errors.Join (Go 1.20+) reports every problem at once, so one
// restart reveals everything that is wrong. Config validation is precisely where
// accumulating errors beats failing fast.
func (c *Config) Validate() error {
	var errs []error

	switch c.Env {
	case EnvDevelopment, EnvProduction:
		// ok
	default:
		errs = append(errs, fmt.Errorf("APP_ENV=%q: %w (want %q or %q)",
			c.Env, ErrInvalid, EnvDevelopment, EnvProduction))
	}

	if _, ok := validLogLevels[c.LogLevel]; !ok {
		errs = append(errs, fmt.Errorf("LOG_LEVEL=%q: %w (want debug, info, warn or error)",
			c.LogLevel, ErrInvalid))
	}

	if strings.TrimSpace(c.API.Addr) == "" {
		errs = append(errs, fmt.Errorf("API_HTTP_ADDR: %w (must not be blank)", ErrInvalid))
	}
	// EVERY server timeout must be positive, not just the shutdown one.
	//
	// net/http treats a ZERO OR NEGATIVE timeout as NO TIMEOUT AT ALL. So
	// API_READ_TIMEOUT=0s does not mean "instant", it silently removes the bound --
	// and with it the Slowloris protection that internal/httpapi/server.go claims to
	// provide. A config value that quietly disables a security control while the code
	// comments promise it is enforced is worse than no comment at all.
	//
	// An operator who genuinely wants an effectively unbounded read can set a large
	// duration; they cannot accidentally get one from a typo or an empty ConfigMap
	// value that parsed as 0.
	for _, t := range []struct {
		name  string
		value time.Duration
	}{
		{"API_READ_TIMEOUT", c.API.ReadTimeout},
		{"API_WRITE_TIMEOUT", c.API.WriteTimeout},
		{"API_IDLE_TIMEOUT", c.API.IdleTimeout},
		{"API_SHUTDOWN_TIMEOUT", c.API.ShutdownTimeout},
		{"PROMETHEUS_TIMEOUT", c.Prometheus.Timeout},
		{"DB_CONN_MAX_LIFETIME", c.Database.ConnMaxLifetime},
	} {
		if t.value <= 0 {
			errs = append(errs, fmt.Errorf("%s=%s: %w (must be positive; zero or negative "+
				"disables the timeout entirely)", t.name, t.value, ErrInvalid))
		}
	}

	// AUTHENTICATION IS MANDATORY IN PRODUCTION.
	//
	// Without this the service starts happily with no keys and serves cost data to anyone who can
	// reach it -- and nothing about a healthy startup would indicate that. "Secure by default"
	// means the insecure configuration must be the one that refuses to run, not the one that is
	// convenient.
	//
	// Development is exempt so `make run-api` needs no ceremony, and the API logs a warning at
	// startup so an unauthenticated instance is never silent about it.
	if c.IsProduction() && len(c.API.APIKeys) == 0 {
		errs = append(errs, fmt.Errorf("API_KEYS: %w (at least one key is required when APP_ENV=production; "+
			"an unauthenticated cost API serves spend data to anyone who can reach it)", ErrRequired))
	}
	for i, k := range c.API.APIKeys {
		// A short key is brute-forceable, and a rate limiter does not save it: an attacker with
		// several source addresses gets several quotas. 16 characters of real entropy is the
		// minimum worth calling a secret.
		if len(k) < 16 {
			errs = append(errs, fmt.Errorf("API_KEYS[%d]: %w (each key must be at least 16 characters)", i, ErrInvalid))
		}
	}
	if c.API.RateLimitPerSecond < 0 {
		errs = append(errs, fmt.Errorf("API_RATE_LIMIT_PER_SECOND=%v: %w (must not be negative; use 0 to disable)",
			c.API.RateLimitPerSecond, ErrInvalid))
	}
	if c.API.RateLimitBurst < 0 {
		errs = append(errs, fmt.Errorf("API_RATE_LIMIT_BURST=%d: %w (must not be negative)",
			c.API.RateLimitBurst, ErrInvalid))
	}

	if c.Database.MaxOpenConns <= 0 {
		errs = append(errs, fmt.Errorf("DB_MAX_OPEN_CONNS=%d: %w (must be positive)",
			c.Database.MaxOpenConns, ErrInvalid))
	}
	// An idle floor above the open ceiling is unsatisfiable, and pgx would silently
	// clamp it rather than complain -- so we reject it here where the message can
	// name both variables.
	if c.Database.MinIdleConns > c.Database.MaxOpenConns {
		errs = append(errs, fmt.Errorf("DB_MIN_IDLE_CONNS=%d: %w (cannot exceed DB_MAX_OPEN_CONNS=%d)",
			c.Database.MinIdleConns, ErrInvalid, c.Database.MaxOpenConns))
	}
	if c.Database.MinIdleConns < 0 {
		errs = append(errs, fmt.Errorf("DB_MIN_IDLE_CONNS=%d: %w (must not be negative)",
			c.Database.MinIdleConns, ErrInvalid))
	}

	if c.Kube.CacheSyncTimeout <= 0 {
		errs = append(errs, fmt.Errorf("KUBE_CACHE_SYNC_TIMEOUT=%s: %w (must be positive)",
			c.Kube.CacheSyncTimeout, ErrInvalid))
	}
	if c.Kube.QPS <= 0 {
		errs = append(errs, fmt.Errorf("KUBE_QPS=%v: %w (must be positive)", c.Kube.QPS, ErrInvalid))
	}
	// Burst below QPS makes the burst allowance the real ceiling and the QPS setting a
	// lie, which is exactly the kind of misconfiguration that presents as unexplained
	// latency rather than as an error.
	if c.Kube.Burst < int(c.Kube.QPS) {
		errs = append(errs, fmt.Errorf("KUBE_BURST=%d: %w (must be >= KUBE_QPS=%v)",
			c.Kube.Burst, ErrInvalid, c.Kube.QPS))
	}
	// A negative resync is nonsense; zero is the meaningful "disabled" value.
	if c.Kube.ResyncInterval < 0 {
		errs = append(errs, fmt.Errorf("KUBE_RESYNC_INTERVAL=%s: %w (must not be negative)",
			c.Kube.ResyncInterval, ErrInvalid))
	}

	if strings.TrimSpace(c.ClusterName) == "" {
		errs = append(errs, fmt.Errorf("CLUSTER_NAME: %w (must not be blank; it is stored on every cost row)", ErrInvalid))
	}
	// PRODUCTION MUST NAME ITS CLUSTER, and an audit is why this check exists.
	//
	// The default is the placeholder "default", and validation only rejected BLANK -- so a deployment
	// that simply never set the variable passed every check and wrote 74,925 rows attributed to a
	// cluster called "default". The monthly statements read "cluster/default", which is not a name
	// anybody would recognise as their estate.
	//
	// Why it is worth failing startup over rather than warning. Migration 000001 states the rule:
	// cluster_name is denormalised onto every fact row and must be stable for the life of the cluster,
	// because changing it makes yesterday's rows look like they belong to a different cluster and a
	// report grouped by cluster shows one estate as two. So the placeholder is not merely untidy -- it
	// is a value someone will eventually correct, and correcting it splits history permanently.
	//
	// Phase 11 makes it worse rather than better: two real clusters that both defaulted would MERGE
	// into one, silently summing unrelated spend. A wrong total is worse than a refused startup.
	//
	// Development keeps the default so `make run-api` needs no ceremony, exactly as with API_KEYS, and
	// the collector logs the effective name at startup so it is never a silent choice.
	if c.IsProduction() && strings.TrimSpace(c.ClusterName) == defaultClusterName {
		errs = append(errs, fmt.Errorf("CLUSTER_NAME: %w (must be set explicitly when APP_ENV=production; "+
			"it is denormalised onto every cost row, and %q is a placeholder that splits history the day "+
			"somebody corrects it)", ErrRequired, defaultClusterName))
	}
	if strings.TrimSpace(c.Pricing.CataloguePath) == "" {
		errs = append(errs, fmt.Errorf("PRICING_CATALOGUE_PATH: %w (must not be blank)", ErrInvalid))
	}

	if c.Collector.Workers <= 0 {
		errs = append(errs, fmt.Errorf("COLLECTOR_WORKERS=%d: %w (must be positive)",
			c.Collector.Workers, ErrInvalid))
	}
	if strings.TrimSpace(c.Collector.HTTPAddr) == "" {
		// Blank would make net/http bind a RANDOM free port, so /metrics would be served somewhere
		// nothing is configured to scrape and the probes would point at a port that changes on every
		// restart. Silently unobservable is worse than refusing to start.
		errs = append(errs, fmt.Errorf("COLLECTOR_HTTP_ADDR: %w (must not be blank; a blank address binds a random port)", ErrInvalid))
	}
	if c.Collector.Interval > 0 && c.API.Addr == c.Collector.HTTPAddr {
		// Both binaries reading one .env is the normal local setup, so an identical address is an easy
		// mistake -- and the symptom is a confusing "address already in use" from whichever started
		// second, rather than anything pointing at the config.
		errs = append(errs, fmt.Errorf("COLLECTOR_HTTP_ADDR=%s: %w (must differ from API_HTTP_ADDR, or the two processes cannot run together)",
			c.Collector.HTTPAddr, ErrInvalid))
	}
	if c.Collector.Interval <= 0 {
		errs = append(errs, fmt.Errorf("COLLECTOR_INTERVAL=%s: %w (must be positive)",
			c.Collector.Interval, ErrInvalid))
	}

	return errors.Join(errs...)
}

// -----------------------------------------------------------------------------
// loader
// -----------------------------------------------------------------------------

// loader accumulates parse errors while reading the environment, so that Load can
// report every malformed variable in one pass instead of stopping at the first.
//
// It is unexported: nothing outside this package should be reading raw env vars.
// That is the whole point -- one door in, and it is Load.
type loader struct {
	errs []error
}

// str returns the value of key, or def if it is unset OR BLANK.
//
// Treating "" as "unset" is a deliberate choice. In Kubernetes, an env var sourced
// from a missing ConfigMap key, or written as `MY_VAR: ""` in a manifest, arrives as
// an empty string rather than being absent. Honouring "" literally would mean
// booting with an empty address and no explanation. It also makes tests tidy,
// because t.Setenv cannot unset a variable but can set it to "".
func (l *loader) str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

// required returns the value of key, recording an error if it is missing or blank.
//
// It returns "" on failure and lets Load continue, so that one run reports every
// missing variable rather than only the first.
func (l *loader) required(key string) string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		l.errs = append(l.errs, fmt.Errorf("%s: %w", key, ErrRequired))
		return ""
	}
	return v
}

// duration parses a Go duration string such as "30s", "5m" or "1h30m".
func (l *loader) duration(key string, def time.Duration) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		// %w wraps the underlying parse error so `errors.Is` and the full cause
		// chain survive. Using %v here would flatten it to a string and throw the
		// original away -- the single most common error-handling mistake in Go.
		l.errs = append(l.errs, fmt.Errorf("%s=%q: %w", key, raw, err))
		return def
	}
	return d
}

// integer parses a base-10 integer.
func (l *loader) integer(key string, def int) int {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q: %w", key, raw, err))
		return def
	}
	return n
}

// int32 parses an integer and REJECTS anything outside int32's range.
//
// WHY A DEDICATED HELPER INSTEAD OF int32(l.integer(...))
// ------------------------------------------------------
// A plain conversion narrows SILENTLY by discarding high bits, and the result can look
// perfectly valid. DB_MAX_OPEN_CONNS=4294967297 (2^32 + 1) narrows to 1, so instead of
// an error the service starts with a one-connection pool and mysteriously serialises
// every query. DB_MAX_OPEN_CONNS=2147483648 narrows to -2147483648, which the range
// check below at least catches -- but only by luck.
//
// Silent wrongness is the worst class of config bug, because nothing points at the
// config. Rejecting out-of-range values makes it a startup error naming the variable.
func (l *loader) int32(key string, def int32) int32 {
	n := l.integer64(key, int64(def))
	if n < math.MinInt32 || n > math.MaxInt32 {
		l.errs = append(l.errs, fmt.Errorf("%s=%d: %w (out of range for int32)", key, n, ErrInvalid))
		return def
	}
	return int32(n)
}

// integer64 parses a base-10 int64.
func (l *loader) integer64(key string, def int64) int64 {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q: %w", key, raw, err))
		return def
	}
	return n
}

// float32 parses a float and rejects values float32 cannot represent.
//
// Narrowing a float64 beyond float32's range yields +Inf rather than wrapping, and
// +Inf passes a naive "must be positive" check. A QPS of +Inf then disables client-side
// rate limiting altogether, which is the opposite of what a large number implies.
func (l *loader) float32(key string, def float32) float32 {
	f := l.float(key, float64(def))
	if math.IsInf(f, 0) || math.IsNaN(f) || f > math.MaxFloat32 || f < -math.MaxFloat32 {
		l.errs = append(l.errs, fmt.Errorf("%s=%v: %w (out of range for float32)", key, f, ErrInvalid))
		return def
	}
	return float32(f)
}

// csv parses a comma-separated list, discarding blanks.
//
// Blanks are discarded rather than preserved because "a,,b" and a trailing comma are what a
// hand-edited environment variable actually looks like, and an empty string in a list of API keys
// would be a key that matches an empty presented value.
func (l *loader) csv(key string) []string {
	raw := l.str(key, "")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// float parses a base-10 floating point value.
func (l *loader) float(key string, def float64) float64 {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q: %w", key, raw, err))
		return def
	}
	return f
}

// err returns all accumulated errors joined together, or nil if there were none.
// errors.Join returns nil when every argument is nil, so this needs no length check.
func (l *loader) err() error { return errors.Join(l.errs...) }
