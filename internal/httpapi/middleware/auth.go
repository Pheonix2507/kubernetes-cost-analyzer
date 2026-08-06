package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"sort"
	"strings"
)

// APIKeyHeader is the non-standard header this API also accepts.
const APIKeyHeader = "X-API-Key"

// unauthenticatedPaths are exempt from authentication.
//
// PROBES MUST BE EXEMPT, and this is not a convenience. The kubelet issues liveness and readiness
// probes with no credentials and no way to supply them, so requiring auth on /healthz means every
// probe returns 401, the kubelet treats that as a failure, and it kills the container. The service
// would enter CrashLoopBackOff the moment authentication was enabled -- caused entirely by the
// security control.
//
// This is safe because those endpoints disclose nothing: liveness returns a fixed "ok", and
// readiness returns dependency names and statuses. Note the reasoning is "they leak nothing", NOT
// "they are internal" -- /readyz does include error strings, which is why internal/health documents
// that it must never be exposed through a public ingress.
//
// /version IS NOT HERE, and an audit found the OpenAPI spec claimed otherwise -- it documented
// /version as overriding security while the code returned 401 for it. Proven with auth enabled:
// /healthz and /readyz answered 200, /version answered 401.
//
// The code is right and the spec was wrong, for two reasons. The kubelet does not probe /version, so
// nothing operationally requires it to be open -- and the exemption is justified by operational
// NECESSITY plus "leaks nothing", not by either alone. And build version and commit are exactly what
// an attacker fingerprints to look up known CVEs in our dependency tree, so it is the one endpoint
// here where being closed has a concrete benefit.
//
// UnauthenticatedPaths below exists so the spec is checked against THIS map rather than against a
// hand-written copy in a test. That was the actual defect: the drift test asserted a list of its own,
// so the test and the spec agreed with each other and both disagreed with the code.
var unauthenticatedPaths = map[string]struct{}{
	"/healthz": {},
	"/readyz":  {},
}

// UnauthenticatedPaths returns the exempt paths, sorted.
//
// Exported for one reason: this map is the single most security-sensitive line in the package. Adding
// an entry silently disables authentication AND rate limiting for that path, and nothing about the
// diff would look alarming -- so the openapi drift test and a dedicated middleware test both assert
// against this function rather than against their own copies of the list.
func UnauthenticatedPaths() []string {
	out := make([]string, 0, len(unauthenticatedPaths))
	for p := range unauthenticatedPaths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// APIKeyAuth requires a valid key on every request outside unauthenticatedPaths.
//
// WHY AN API KEY AND NOT JWT
// -------------------------
// Both are bearer tokens: whoever holds one is authenticated, and neither is inherently more
// secure. The real difference is what they can express and who manages them.
//
// A JWT carries signed CLAIMS -- a user id, a tenant, an expiry -- so the server can authorise
// without a lookup, and tokens expire without being revoked. That is worth its complexity when
// there are many users with different permissions, which is Phase 11's multi-cluster world.
//
// Today this is a single-tenant internal tool where every caller may see everything. A shared key
// expresses exactly that, and pretending otherwise by issuing JWTs with no meaningful claims would
// add signature verification, key rotation and clock-skew handling to buy nothing.
//
// What a key CANNOT do, stated plainly: it does not expire, it identifies a client only as well as
// the operator's key hygiene allows, and revoking one means editing config and restarting. When any
// of those becomes unacceptable, that is the signal to move to real tokens -- not the fact that JWT
// sounds more professional.
func APIKeyAuth(keys []string, log *slog.Logger) Middleware {
	// Keys are HASHED ONCE at construction and compared as digests.
	//
	// Two reasons. Comparing fixed-length digests means the comparison time cannot vary with how
	// much of the key matched -- see the note on ConstantTimeCompare below. And the plaintext key
	// is not retained in a slice that could end up in a panic dump or a debugger's view of the
	// middleware's captured variables.
	hashed := make([][32]byte, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		hashed = append(hashed, sha256.Sum256([]byte(k)))
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, exempt := unauthenticatedPaths[r.URL.Path]; exempt {
				next.ServeHTTP(w, r)
				return
			}

			// No keys configured means authentication is DISABLED. config.Validate refuses to start
			// in production without keys, so reaching here with none means development.
			if len(hashed) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			presented := presentedKey(r)
			if presented == "" {
				// 401 with WWW-Authenticate, which is what makes this a correct 401 rather than a
				// 403: the header tells the client HOW to authenticate. 403 would mean "you are
				// authenticated and still not allowed", which is a different situation.
				w.Header().Set("WWW-Authenticate", `Bearer realm="kubernetes-cost-analyzer"`)
				unauthorised(w, r, log, "missing")
				return
			}

			if !authorised(hashed, presented) {
				unauthorised(w, r, log, "invalid")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// presentedKey reads the key from either supported header.
//
// Authorization: Bearer is the standard and what tooling expects. X-API-Key is also accepted
// because it is what a lot of internal tooling and dashboards send, and refusing it on principle
// achieves nothing except friction.
func presentedKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		// The scheme is matched case-INSENSITIVELY: RFC 7235 defines it as case-insensitive, and
		// clients really do send "bearer".
		const prefix = "bearer "
		if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
			return strings.TrimSpace(auth[len(prefix):])
		}
	}
	return strings.TrimSpace(r.Header.Get(APIKeyHeader))
}

// authorised reports whether the presented key matches any configured key.
func authorised(hashed [][32]byte, presented string) bool {
	sum := sha256.Sum256([]byte(presented))

	// subtle.ConstantTimeCompare, NEVER ==.
	//
	// A byte-slice or string comparison returns as soon as it finds a difference, so it takes
	// measurably longer the more of the prefix matched. That timing difference leaks the secret: an
	// attacker submits keys differing in the first byte, keeps whichever was slowest, and repeats
	// per position -- recovering the key in linear rather than exponential attempts. It is a real
	// attack, not a theoretical one, and it has broken real authentication.
	//
	// ConstantTimeCompare always examines every byte. Note that hashing first also defends against
	// this by making every comparison the same length, so the two measures reinforce each other.
	//
	// The loop deliberately does NOT break on a match: returning early would reintroduce a timing
	// signal proportional to how many keys were checked, revealing a valid key's position in the
	// list.
	var match int
	for i := range hashed {
		match |= subtle.ConstantTimeCompare(hashed[i][:], sum[:])
	}
	return match == 1
}

// unauthorised writes a 401.
//
// The response says only that authentication failed -- never whether the key was absent, malformed
// or simply wrong. Distinguishing them tells an attacker their key is the right SHAPE, which is
// information worth withholding for nothing in return. The distinction IS logged, where it helps
// an operator debug a misconfigured client.
func unauthorised(w http.ResponseWriter, r *http.Request, log *slog.Logger, reason string) {
	log.Warn("rejected an unauthenticated request",
		"reason", reason,
		"path", r.URL.Path,
		"remote_addr", r.RemoteAddr,
		// The presented key is NEVER logged, not even truncated. A prefix is enough to brute-force
		// the rest, and logs are shipped, indexed and retained far longer than anyone intends.
	)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"a valid API key is required"}}`))
}
