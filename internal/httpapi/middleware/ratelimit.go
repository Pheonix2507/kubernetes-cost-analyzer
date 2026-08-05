package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimitConfig configures the limiter.
type RateLimitConfig struct {
	// RequestsPerSecond is the sustained rate allowed per client.
	RequestsPerSecond float64
	// Burst is how many requests may arrive at once before throttling applies.
	//
	// A token bucket refills at RequestsPerSecond and holds at most Burst tokens, so a client may
	// spend the whole bucket instantly and then proceeds at the sustained rate. That is the right
	// shape for a dashboard, which fires several parallel requests on load and then goes quiet --
	// a fixed window would reject the initial fan-out even when the average rate is trivial.
	Burst int
	// TTL is how long an idle client's limiter is retained. See the note on the eviction loop.
	TTL time.Duration
}

// RateLimit throttles requests per client.
//
// WHY A TOKEN BUCKET RATHER THAN A FIXED WINDOW
// ---------------------------------------------
// A fixed window ("100 requests per minute") has a boundary problem: a client can send 100 at
// 11:59:59 and 100 more at 12:00:00, so the real peak is twice the intended limit. It is also
// hostile to bursty-but-light traffic, which is exactly what a dashboard produces.
//
// A token bucket smooths this: tokens accrue continuously, so there is no boundary to exploit, and
// the burst allowance is explicit rather than an accident of when the window happens to start.
//
// WHAT THIS PROTECTS AGAINST, HONESTLY
// It is not a defence against a determined attacker -- it is per-key, in-process, and a distributed
// caller with several keys or several replicas routes around it. What it does protect against is
// the realistic failure: a runaway script or a dashboard in a reload loop issuing thousands of
// requests, each of which aggregates over a partitioned fact table. Left unbounded that is a
// self-inflicted denial of service against Postgres, and one careless client would take the API
// down for everyone.
//
// A shared cross-replica limit needs Redis or an ingress-level policy. That is the right answer at
// scale and considerable machinery for a problem this does not yet have.
func RateLimit(cfg RateLimitConfig, log *slog.Logger) Middleware {
	if cfg.RequestsPerSecond <= 0 {
		// Zero means disabled rather than "block everything", because a misconfiguration that
		// silently denies all traffic is far worse than one that allows it.
		return func(next http.Handler) http.Handler { return next }
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 1
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 10 * time.Minute
	}

	l := &limiterSet{
		limiters: map[string]*clientLimiter{},
		rate:     rate.Limit(cfg.RequestsPerSecond),
		burst:    cfg.Burst,
		ttl:      cfg.TTL,
		log:      log,
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// PROBES ARE NEVER RATE LIMITED.
			//
			// The kubelet polls /healthz and /readyz every few seconds forever. Throttling them
			// means a 429, which the kubelet reads as a failed probe, which kills the container --
			// so the rate limiter would take the service down under exactly the load it exists to
			// survive. Same reasoning as the auth exemption.
			if _, exempt := unauthenticatedPaths[r.URL.Path]; exempt {
				next.ServeHTTP(w, r)
				return
			}

			key := clientKey(r)
			if !l.allow(key) {
				retryAfter := 1
				if cfg.RequestsPerSecond < 1 {
					retryAfter = int(1/cfg.RequestsPerSecond) + 1
				}
				// Retry-After turns a 429 into an instruction rather than a rejection. Without it a
				// well-behaved client can only guess, and most guess "immediately", which makes the
				// overload worse.
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"too many requests; see Retry-After"}}`))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// clientKey identifies the caller to limit.
//
// PREFERS THE API KEY over the IP address, and the difference matters. Behind an ingress or a
// service mesh every request arrives from the same proxy IP, so IP-based limiting would either
// throttle every caller collectively or, if the proxy's IP were exempted, throttle nobody.
//
// The key is HASHED before use as a map key, so the secret is never held in a data structure that
// could be dumped, logged or inspected.
//
// X-Forwarded-For is deliberately NOT trusted: it is a client-supplied header, so an attacker
// sets a fresh value per request and gets an unlimited quota. Trusting it requires knowing exactly
// how many proxies sit in front and reading from the right end -- configuration this does not have
// and should not guess at.
func clientKey(r *http.Request) string {
	if k := presentedKey(r); k != "" {
		sum := sha256.Sum256([]byte(k))
		return "key:" + hex.EncodeToString(sum[:8])
	}
	// Unauthenticated requests (development, where no keys are configured) fall back to the peer
	// address. SplitHostPort strips the ephemeral port, which would otherwise make every single
	// connection a distinct client and defeat the limit entirely.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "ip:" + host
}

// clientLimiter is one client's bucket plus when it was last used.
type clientLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// limiterSet holds a limiter per client.
type limiterSet struct {
	// A MUTEX, not a sync.Map. The access pattern here is read-then-conditionally-write on the
	// same key, which sync.Map does not make atomic -- two goroutines racing on a new key would
	// both construct a limiter and one would be discarded, briefly doubling that client's
	// allowance. LoadOrStore avoids that but allocates a limiter on every request just to discard
	// it. A plain mutex is simpler and correct, and this is not a contended hot path.
	mu       sync.Mutex
	limiters map[string]*clientLimiter

	rate  rate.Limit
	burst int
	ttl   time.Duration
	log   *slog.Logger

	lastSweep time.Time
}

// allow reports whether this client may proceed, and records the request.
func (l *limiterSet) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()

	// EVICTION IS NOT OPTIONAL, and its absence is the standard bug in hand-rolled rate limiters.
	//
	// The map grows by one entry per distinct client, forever. With per-IP keying and any public
	// exposure that is unbounded memory growth an attacker controls directly: a few million
	// requests from spoofed sources and the process is OOMKilled by its own rate limiter. Even
	// with API-key keying, key rotation slowly accumulates dead entries for the process's life.
	//
	// Swept inline under the lock we already hold, rather than from a background goroutine. That
	// avoids a goroutine whose lifetime nobody manages, and it means the sweep happens exactly
	// when the map is being used -- an idle service does not need cleaning.
	if now.Sub(l.lastSweep) > l.ttl {
		l.sweep(now)
		l.lastSweep = now
	}

	cl, found := l.limiters[key]
	if !found {
		cl = &clientLimiter{limiter: rate.NewLimiter(l.rate, l.burst)}
		l.limiters[key] = cl
	}
	cl.lastSeen = now

	return cl.limiter.Allow()
}

// sweep removes limiters unused for longer than the TTL.
//
// Discarding a limiter resets that client's bucket, which technically hands an idle client a fresh
// burst allowance. That is harmless: a client idle for the whole TTL has by definition not been
// consuming its quota, so its bucket would have refilled to full anyway.
func (l *limiterSet) sweep(now time.Time) {
	before := len(l.limiters)
	for key, cl := range l.limiters {
		if now.Sub(cl.lastSeen) > l.ttl {
			delete(l.limiters, key)
		}
	}
	if removed := before - len(l.limiters); removed > 0 {
		l.log.Debug("evicted idle rate limiters", "removed", removed, "remaining", len(l.limiters))
	}
}
