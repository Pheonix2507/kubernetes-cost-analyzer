package httpapi

import (
	"net/http"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/health"
)

// handleLive serves the LIVENESS probe.
//
// It checks NOTHING and returns 200 unconditionally. That is not laziness -- it is
// the correct implementation, and the reasoning is worth internalising:
//
// Failing liveness makes the kubelet KILL the container. The only question it should
// answer is therefore "is this process so broken that restarting it is the right
// remedy?" A restart fixes deadlocks, exhausted goroutines and corrupted in-process
// state. It does NOT fix an unreachable database, a slow downstream service, or an
// expired credential -- and attempting to fix those by restarting makes things
// strictly worse, because now the dependency is still broken AND you have thrown away
// warm caches and connection pools across every replica simultaneously.
//
// If this handler responds at all, the process is scheduling goroutines and its
// listener is accepting connections. That is exactly the scope of the question.
//
// See internal/health for the full liveness/readiness argument.
func handleLive() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// handleReady serves the READINESS probe: can this replica serve traffic right now?
//
// Failing it removes the pod from Service endpoints without restarting it, so traffic
// drains away and returns automatically once the dependency recovers. This is where
// dependency checks belong.
//
// It takes the *health.Aggregator rather than a concrete database handle, so adding
// the Prometheus and Kubernetes API checks in later phases requires no change here --
// they are new health.Checker implementations passed to NewAggregator in main.
func handleReady(readiness *health.Aggregator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// r.Context() is cancelled when the client disconnects, so an abandoned
		// probe stops doing work immediately rather than holding a database
		// connection until it times out.
		report := readiness.Run(r.Context())

		// 503 Service Unavailable, NOT 500.
		//
		// The distinction is semantic and load balancers act on it: 503 means "I am
		// working correctly but temporarily cannot serve, try elsewhere or retry",
		// while 500 means "I am broken". Readiness failure is the former. Returning
		// 500 here would also make our own error-rate alerts fire during an ordinary
		// dependency blip that the system is already handling correctly by itself.
		status := http.StatusOK
		if report.Status == health.StatusDown {
			status = http.StatusServiceUnavailable
		}

		// The full per-dependency report is returned in both cases. When a probe
		// fails, the kubelet records the response body in the pod's events -- so
		// `kubectl describe pod` will name the failing dependency and its error.
		// That turns "readiness probe failed" into an actionable message.
		writeJSON(w, r, status, report)
	}
}

// handleVersion reports which build is running.
//
// Deliberately separate from the health endpoints: probes are polled every few
// seconds and must stay trivial, whereas this is for humans and deploy tooling
// answering "did my rollout actually land?"
func handleVersion() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, http.StatusOK, map[string]string{
			"version":    buildinfo.Version,
			"commit":     buildinfo.Commit,
			"build_time": buildinfo.BuildTime,
		})
	}
}
