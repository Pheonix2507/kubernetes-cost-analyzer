package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/metrics"
)

// The collector's observability listener.
//
// WHY A PROCESS THAT SERVES NOBODY NEEDS AN HTTP SERVER
// ====================================================
// The collector answers no queries and has no users. It gets a listener anyway, because two separate
// problems turn out to be the same problem: YOU CANNOT SCRAPE A PROCESS THAT DOES NOT LISTEN, and you
// cannot PROBE one either.
//
// Instrumenting the collection loop -- cycle duration, rows written, namespaces failed -- is pointless if
// Prometheus has nowhere to fetch the numbers. The metrics would exist in memory and be read by nobody.
//
// The probe half is worse. Without a listener, Kubernetes has exactly one liveness signal: whether the
// process has exited. So a collector wedged on a hung Prometheus query, or deadlocked on a mutex, looks
// perfectly healthy indefinitely. A wedged process that never restarts is worse than one that crashes,
// because a crash is visible in the restart count and a wedge is visible nowhere.
//
// This closes a deferral carried since Phase 4, which read "the collector has no HTTP liveness/readiness
// endpoint (Phase 10)". It landed in Phase 9 instead, because instrumentation forced the same listener
// and building it twice would have been silly.
//
// WHY NOT REUSE internal/httpapi.Server
// It takes a config.API and applies read/write timeouts, TLS settings and a shutdown timeout tuned for a
// public API serving user traffic. This listener is internal, serves three fixed endpoints, and is
// reached only by the kubelet and Prometheus. Threading a config.API through the collector to configure
// a /metrics endpoint would couple the two binaries' configuration for no benefit. The timeouts below are
// chosen for what this actually is.
func observabilityServer(
	addr string,
	log *slog.Logger,
	m *metrics.Collector,
	readiness *health.Aggregator,
) *http.Server {
	mux := http.NewServeMux()

	// Method-qualified, so a POST gets an automatic 405 rather than being treated as a GET. Same
	// reasoning as the API's router.
	// The same handler the API serves, from the same constructor -- see metrics.Registry.Handler.
	mux.Handle("GET /metrics", m.Handler(log))

	// LIVENESS CHECKS NOTHING, exactly as in internal/httpapi/health.go, and the reason is worth
	// restating because it is counter-intuitive: a restart cannot fix a dependency. If liveness checked
	// Prometheus, then a Prometheus outage would make Kubernetes kill and restart this collector in a
	// loop -- adding a crash-looping pod to an incident it cannot possibly help with, and losing the
	// in-memory informer caches every time.
	//
	// Liveness answers one question: is this process still able to serve? A handler that returns is proof
	// enough.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// READINESS CHECKS DEPENDENCIES, and for the collector it means something different from the API.
	//
	// The API's readiness gates SERVICE ENDPOINTS: failing it removes the pod from load balancing, which
	// is a useful, targeted action. The collector is in no Service and receives no traffic, so failing
	// readiness routes nothing away.
	//
	// It is still worth serving, for two reasons. A rollout will not proceed past a pod that never
	// becomes ready, so a collector that cannot reach Postgres does not silently replace a working one.
	// And it gives an operator one URL that answers "why is this not collecting" without reading logs.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		// r.Context(), so a probe that gives up mid-flight cancels the checks it triggered rather than
		// leaving this holding a database connection until its own timeout expires.
		report := readiness.Run(r.Context())

		code := http.StatusOK
		if report.Status == health.StatusDown {
			// 503, not 500. The distinction matters to the kubelet and to a human reading it: 503 means
			// "working correctly but temporarily unable", which is exactly what an unreachable dependency
			// is, while 500 means "broken".
			code = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		// The FULL per-dependency report either way. When a probe fails the kubelet records the response
		// body in the pod's events, so "which dependency" is available from `kubectl describe pod`
		// without anyone opening a log aggregator.
		//
		// Encoded directly to the response rather than buffered, because unlike the API's writeJSON there
		// is no risk worth the buffer here: the payload is a handful of fields and the reader is a probe.
		if err := json.NewEncoder(w).Encode(report); err != nil {
			log.Debug("could not write readiness report", slog.Any("error", err))
		}
	})

	return &http.Server{
		Addr:    addr,
		Handler: mux,
		// Go's defaults are ZERO, meaning no timeout at all. That is a slow-loris waiting to happen even
		// on an internal listener -- and "internal" is doing a lot of work in a cluster where any pod can
		// reach any other unless a NetworkPolicy says otherwise.
		//
		// Shorter than the API's, because every client here is a machine on the same network making a
		// tiny request. Ten seconds is generous for a scrape and mean enough to drop anything stuck.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// runObservabilityServer serves until the context is cancelled, then shuts down gracefully.
func runObservabilityServer(ctx context.Context, srv *http.Server, log *slog.Logger) error {
	errCh := make(chan error, 1)

	go func() {
		log.Info("observability server listening",
			slog.String("addr", srv.Addr),
			slog.String("endpoints", "/metrics /healthz /readyz"))
		// ErrServerClosed is the NORMAL result of a graceful shutdown, so it is filtered here rather than
		// treated as a failure. Without this the errgroup would report a clean shutdown as the error that
		// caused it.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		// A bind failure. Returned so the errgroup cancels everything: a collector nobody can scrape or
		// probe should not run silently, because a wedge would then be undetectable -- which is the exact
		// failure this listener exists to prevent.
		return err
	case <-ctx.Done():
		// context.Background(), NOT ctx.
		//
		// ctx is already cancelled -- that is why we are here -- and Shutdown on a cancelled context
		// returns immediately without draining, so in-flight requests are cut off. This is the same trap
		// documented in internal/httpapi/server.go and in the advisory lock's Release: cleanup must not
		// depend on the thing being cleaned up after.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		log.Info("shutting down observability server")
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	}
}
