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

	API        API
	Database   Database
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

// Prometheus holds settings for querying Prometheus (our usage data source).
type Prometheus struct {
	URL     string
	Timeout time.Duration
}

// Collector holds settings for the background collection loop.
type Collector struct {
	Interval time.Duration
	Workers  int
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
		Env:      Environment(l.str("APP_ENV", string(EnvDevelopment))),
		LogLevel: l.str("LOG_LEVEL", "info"),

		API: API{
			Addr:            l.str("API_HTTP_ADDR", ":8080"),
			ReadTimeout:     l.duration("API_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:    l.duration("API_WRITE_TIMEOUT", 15*time.Second),
			IdleTimeout:     l.duration("API_IDLE_TIMEOUT", 60*time.Second),
			ShutdownTimeout: l.duration("API_SHUTDOWN_TIMEOUT", 15*time.Second),
		},

		Database: Database{
			// Required: there is no sensible default for a database URL, and
			// guessing localhost would let a production deployment start up
			// pointed at nothing.
			URL:             l.required("DATABASE_URL"),
			MaxOpenConns:    int32(l.integer("DB_MAX_OPEN_CONNS", 20)),
			MinIdleConns:    int32(l.integer("DB_MIN_IDLE_CONNS", 5)),
			ConnMaxLifetime: l.duration("DB_CONN_MAX_LIFETIME", 30*time.Minute),
		},

		Prometheus: Prometheus{
			URL:     l.str("PROMETHEUS_URL", "http://localhost:19090"),
			Timeout: l.duration("PROMETHEUS_TIMEOUT", 30*time.Second),
		},

		Collector: Collector{
			Interval: l.duration("COLLECTOR_INTERVAL", 5*time.Minute),
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
	if c.API.ShutdownTimeout <= 0 {
		errs = append(errs, fmt.Errorf("API_SHUTDOWN_TIMEOUT=%s: %w (must be positive)",
			c.API.ShutdownTimeout, ErrInvalid))
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

	if c.Collector.Workers <= 0 {
		errs = append(errs, fmt.Errorf("COLLECTOR_WORKERS=%d: %w (must be positive)",
			c.Collector.Workers, ErrInvalid))
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

// err returns all accumulated errors joined together, or nil if there were none.
// errors.Join returns nil when every argument is nil, so this needs no length check.
func (l *loader) err() error { return errors.Join(l.errs...) }
