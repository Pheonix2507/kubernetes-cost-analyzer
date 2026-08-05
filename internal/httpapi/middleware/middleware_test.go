package middleware

import (
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// okHandler records whether it was reached, which is how these tests tell "allowed through" from
// "rejected by middleware".
func okHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if reached != nil {
			*reached = true
		}
		w.WriteHeader(http.StatusOK)
	})
}

const testKey = "0123456789abcdef0123456789abcdef"

// =============================================================================
// Authentication
// =============================================================================

func TestAPIKeyAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		header     string
		value      string
		wantStatus int
		why        string
	}{
		{
			name: "bearer token", header: "Authorization", value: "Bearer " + testKey,
			wantStatus: http.StatusOK, why: "the standard scheme",
		},
		{
			name: "lowercase bearer", header: "Authorization", value: "bearer " + testKey,
			wantStatus: http.StatusOK,
			why: "RFC 7235 defines the scheme as case-INSENSITIVE, and clients really do send " +
				"lowercase",
		},
		{
			name: "x-api-key header", header: APIKeyHeader, value: testKey,
			wantStatus: http.StatusOK,
			why: "accepted because it is what a lot of internal tooling sends; refusing it on " +
				"principle achieves nothing but friction",
		},
		{
			name: "wrong key", header: "Authorization", value: "Bearer wrongwrongwrongwrongwrong",
			wantStatus: http.StatusUnauthorized, why: "",
		},
		{
			name: "no credentials", header: "", value: "", wantStatus: http.StatusUnauthorized, why: "",
		},
		{
			name: "wrong scheme", header: "Authorization", value: "Basic " + testKey,
			wantStatus: http.StatusUnauthorized,
			why:        "Basic is a different scheme with different semantics; it is not a bearer token",
		},
		{
			name: "key as a prefix of a longer string", header: "Authorization",
			value: "Bearer " + testKey + "extra", wantStatus: http.StatusUnauthorized,
			why: "the comparison must be on the WHOLE value; a prefix match would accept anything " +
				"beginning with a valid key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var reached bool
			h := APIKeyAuth([]string{testKey}, testLogger())(okHandler(&reached))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil)
			if tt.header != "" {
				req.Header.Set(tt.header, tt.value)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d\nwhy: %s", rec.Code, tt.wantStatus, tt.why)
			}
			if tt.wantStatus == http.StatusUnauthorized && reached {
				t.Error("the handler ran despite a 401; no handler may execute unauthenticated")
			}
		})
	}
}

// TestAPIKeyAuth_ProbesAreExempt is not a convenience test -- it prevents a self-inflicted outage.
//
// The kubelet issues probes with no credentials and no way to supply them. Requiring auth on
// /healthz means every probe returns 401, the kubelet reads that as a failure, and it kills the
// container. The service would enter CrashLoopBackOff the moment authentication was enabled.
func TestAPIKeyAuth_ProbesAreExempt(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/healthz", "/readyz"} {
		var reached bool
		h := APIKeyAuth([]string{testKey}, testLogger())(okHandler(&reached))

		rec := httptest.NewRecorder()
		// NO credentials, exactly as the kubelet sends.
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK || !reached {
			t.Errorf("%s returned %d without credentials; the kubelet cannot authenticate, so a "+
				"401 here would kill the container", path, rec.Code)
		}
	}
}

// TestAPIKeyAuth_NoKeysDisablesAuth covers the development path. config.Validate refuses this in
// production, so reaching it means development -- where requiring ceremony for `make run-api` buys
// nothing.
func TestAPIKeyAuth_NoKeysDisablesAuth(t *testing.T) {
	t.Parallel()

	var reached bool
	h := APIKeyAuth(nil, testLogger())(okHandler(&reached))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil))

	if rec.Code != http.StatusOK || !reached {
		t.Errorf("status = %d with no keys configured, want 200 (auth disabled in development)", rec.Code)
	}
}

// TestAPIKeyAuth_MultipleKeysForRotation covers why the config takes a LIST. With one key, rotation
// means every client breaks at the instant the value changes.
func TestAPIKeyAuth_MultipleKeysForRotation(t *testing.T) {
	t.Parallel()

	oldKey := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newKey := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	h := APIKeyAuth([]string{oldKey, newKey}, testLogger())(okHandler(nil))

	for _, key := range []string{oldKey, newKey} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("key %q rejected; both must work during rotation", key[:4]+"...")
		}
	}
}

// TestAPIKeyAuth_401CarriesWWWAuthenticate covers what makes this a correct 401 rather than a 403.
// The header tells the client HOW to authenticate; 403 would mean "authenticated and still not
// allowed", which is a different situation.
func TestAPIKeyAuth_401CarriesWWWAuthenticate(t *testing.T) {
	t.Parallel()

	h := APIKeyAuth([]string{testKey}, testLogger())(okHandler(nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil))

	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want it to name the Bearer scheme", got)
	}
}

// TestAPIKeyAuth_ResponseDoesNotDistinguishFailureModes covers information disclosure. Telling a
// caller their key was the right SHAPE but wrong VALUE is information worth withholding for nothing
// in return.
func TestAPIKeyAuth_ResponseDoesNotDistinguishFailureModes(t *testing.T) {
	t.Parallel()

	h := APIKeyAuth([]string{testKey}, testLogger())(okHandler(nil))

	bodies := map[string]string{}
	for name, value := range map[string]string{
		"missing": "",
		"wrong":   "Bearer wrongwrongwrongwrongwrong",
		"garbage": "Bearer !!!",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil)
		if value != "" {
			req.Header.Set("Authorization", value)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		bodies[name] = rec.Body.String()
	}

	if bodies["missing"] != bodies["wrong"] || bodies["wrong"] != bodies["garbage"] {
		t.Errorf("responses differ by failure mode, which tells an attacker their key is the right "+
			"shape:\n  missing: %s\n  wrong:   %s\n  garbage: %s",
			bodies["missing"], bodies["wrong"], bodies["garbage"])
	}
	// And the key itself must never be echoed.
	for name, body := range bodies {
		if strings.Contains(body, "wrongwrong") || strings.Contains(body, testKey) {
			t.Errorf("%s response echoes the presented credential: %s", name, body)
		}
	}
}

// TestAuthorised_IsConstantTimeShaped does not measure timing -- that would be hopelessly flaky in
// CI. It asserts the PROPERTY that makes constant-time comparison possible: every comparison is
// over a fixed-length digest, so no amount of matching prefix changes how much work is done.
//
// The attack it prevents is real and has broken real authentication: a byte comparison returns on
// the first difference, so it takes measurably longer the more of the prefix matched. An attacker
// submits keys differing in the first byte, keeps whichever was slowest, and repeats per position --
// recovering the key in linear rather than exponential attempts.
func TestAuthorised_IsConstantTimeShaped(t *testing.T) {
	t.Parallel()

	keys := [][32]byte{}
	for _, k := range []string{testKey, "another-key-that-is-long-enough!"} {
		keys = append(keys, sha256.Sum256([]byte(k)))
	}

	if !authorised(keys, testKey) {
		t.Error("a valid key was rejected")
	}
	// A key sharing a long prefix must be rejected exactly like one sharing none. Hashing first
	// guarantees the comparison length is identical either way.
	if authorised(keys, testKey[:len(testKey)-1]+"X") {
		t.Error("a near-miss key was accepted")
	}
	if authorised(keys, "") {
		t.Error("an empty key was accepted")
	}
	if authorised(nil, testKey) {
		t.Error("a key was accepted against an empty key set")
	}
}

// =============================================================================
// Rate limiting
// =============================================================================

// TestRateLimit_AllowsBurstThenThrottles covers the token-bucket shape. A dashboard fires several
// parallel requests on load and then goes quiet, which a fixed window would reject even though the
// average rate is trivial.
func TestRateLimit_AllowsBurstThenThrottles(t *testing.T) {
	t.Parallel()

	// A very low sustained rate so the bucket does not refill during the test.
	h := RateLimit(RateLimitConfig{RequestsPerSecond: 0.01, Burst: 3, TTL: time.Minute},
		testLogger())(okHandler(nil))

	statuses := []int{}
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil)
		req.Header.Set(APIKeyHeader, testKey)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		statuses = append(statuses, rec.Code)
	}

	// The first three spend the bucket.
	for i := 0; i < 3; i++ {
		if statuses[i] != http.StatusOK {
			t.Errorf("request %d = %d, want 200 within the burst of 3", i+1, statuses[i])
		}
	}
	// The rest are throttled.
	for i := 3; i < 5; i++ {
		if statuses[i] != http.StatusTooManyRequests {
			t.Errorf("request %d = %d, want 429 beyond the burst", i+1, statuses[i])
		}
	}
}

// TestRateLimit_429CarriesRetryAfter covers what turns a rejection into an instruction. Without it a
// well-behaved client can only guess, and most guess "immediately" -- making the overload worse.
func TestRateLimit_429CarriesRetryAfter(t *testing.T) {
	t.Parallel()

	h := RateLimit(RateLimitConfig{RequestsPerSecond: 0.01, Burst: 1, TTL: time.Minute},
		testLogger())(okHandler(nil))

	var rec *httptest.ResponseRecorder
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil)
		req.Header.Set(APIKeyHeader, testKey)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	retry := rec.Header().Get("Retry-After")
	if retry == "" {
		t.Fatal("no Retry-After header; a client can only guess, and most guess 'immediately'")
	}
	if n, err := strconv.Atoi(retry); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive number of seconds", retry)
	}
}

// TestRateLimit_ProbesAreNeverThrottled prevents the same self-inflicted outage as the auth
// exemption: a 429 on a probe reads as a failure and kills the container, so the limiter would take
// the service down under exactly the load it exists to survive.
func TestRateLimit_ProbesAreNeverThrottled(t *testing.T) {
	t.Parallel()

	h := RateLimit(RateLimitConfig{RequestsPerSecond: 0.01, Burst: 1, TTL: time.Minute},
		testLogger())(okHandler(nil))

	for _, path := range []string{"/healthz", "/readyz"} {
		for i := 0; i < 20; i++ {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s request %d = %d, want 200: throttling a probe kills the container",
					path, i+1, rec.Code)
			}
		}
	}
}

// TestRateLimit_ClientsAreIndependent proves the limiter keys per client. A shared bucket would mean
// one busy caller throttling everyone else.
func TestRateLimit_ClientsAreIndependent(t *testing.T) {
	t.Parallel()

	h := RateLimit(RateLimitConfig{RequestsPerSecond: 0.01, Burst: 1, TTL: time.Minute},
		testLogger())(okHandler(nil))

	send := func(key string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil)
		req.Header.Set(APIKeyHeader, key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	keyA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	keyB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	if got := send(keyA); got != http.StatusOK {
		t.Fatalf("first request for key A = %d, want 200", got)
	}
	if got := send(keyA); got != http.StatusTooManyRequests {
		t.Fatalf("second request for key A = %d, want 429", got)
	}
	// B must be unaffected by A having exhausted its bucket.
	if got := send(keyB); got != http.StatusOK {
		t.Errorf("first request for key B = %d, want 200: one busy caller must not throttle another",
			got)
	}
}

// TestRateLimit_EvictsIdleLimiters covers the standard bug in hand-rolled rate limiters.
//
// The map grows by one entry per distinct client, forever. With per-IP keying and any public
// exposure that is unbounded memory growth an ATTACKER CONTROLS: a few million requests from
// spoofed sources and the process is OOMKilled by its own rate limiter.
func TestRateLimit_EvictsIdleLimiters(t *testing.T) {
	t.Parallel()

	// A very short TTL so eviction is observable without sleeping.
	l := &limiterSet{
		limiters: map[string]*clientLimiter{},
		rate:     100,
		burst:    100,
		ttl:      10 * time.Millisecond,
		log:      testLogger(),
	}

	for i := 0; i < 50; i++ {
		l.allow("client-" + strconv.Itoa(i))
	}
	if got := len(l.limiters); got != 50 {
		t.Fatalf("got %d limiters, want 50", got)
	}

	// Let them all go idle, then make one more request to trigger the inline sweep.
	time.Sleep(20 * time.Millisecond)
	l.allow("fresh-client")

	// Only the fresh one should remain; the 50 idle ones are evicted.
	if got := len(l.limiters); got > 2 {
		t.Errorf("got %d limiters after the TTL elapsed, want at most 2; without eviction this map "+
			"grows forever and an attacker controls its size", got)
	}
}

// TestRateLimit_ZeroRateDisables covers the misconfiguration direction that matters. A setting that
// silently denies all traffic is far worse than one that allows it.
func TestRateLimit_ZeroRateDisables(t *testing.T) {
	t.Parallel()

	h := RateLimit(RateLimitConfig{RequestsPerSecond: 0}, testLogger())(okHandler(nil))

	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/costs/summary", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d with rate limiting disabled, want 200", i+1, rec.Code)
		}
	}
}

// TestClientKey_PrefersTheAPIKey covers why IP-based limiting is wrong here. Behind an ingress every
// request arrives from the same proxy address, so IP keying would throttle every caller collectively.
func TestClientKey_PrefersTheAPIKey(t *testing.T) {
	t.Parallel()

	withKey := httptest.NewRequest(http.MethodGet, "/x", nil)
	withKey.Header.Set(APIKeyHeader, testKey)
	withKey.RemoteAddr = "10.0.0.1:1234"

	sameProxyDifferentKey := httptest.NewRequest(http.MethodGet, "/x", nil)
	sameProxyDifferentKey.Header.Set(APIKeyHeader, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	sameProxyDifferentKey.RemoteAddr = "10.0.0.1:5678"

	if clientKey(withKey) == clientKey(sameProxyDifferentKey) {
		t.Error("two different keys behind the same proxy share a bucket; IP keying would throttle " +
			"every caller collectively")
	}
	// The raw secret must never become the map key.
	if strings.Contains(clientKey(withKey), testKey) {
		t.Errorf("client key contains the raw credential: %s", clientKey(withKey))
	}

	// Without a key, the peer address is used -- with the ephemeral PORT stripped, or every
	// connection would be a distinct client and the limit would never apply.
	a := httptest.NewRequest(http.MethodGet, "/x", nil)
	a.RemoteAddr = "10.0.0.5:1111"
	b := httptest.NewRequest(http.MethodGet, "/x", nil)
	b.RemoteAddr = "10.0.0.5:2222"
	if clientKey(a) != clientKey(b) {
		t.Error("the same address on two ports is treated as two clients; the ephemeral port must " +
			"be stripped or the limit never applies")
	}
}

// TestClientKey_IgnoresXForwardedFor covers a bypass. X-Forwarded-For is client-supplied, so
// trusting it means an attacker sets a fresh value per request and gets an unlimited quota.
func TestClientKey_IgnoresXForwardedFor(t *testing.T) {
	t.Parallel()

	a := httptest.NewRequest(http.MethodGet, "/x", nil)
	a.RemoteAddr = "10.0.0.1:1234"
	a.Header.Set("X-Forwarded-For", "1.2.3.4")

	b := httptest.NewRequest(http.MethodGet, "/x", nil)
	b.RemoteAddr = "10.0.0.1:1234"
	b.Header.Set("X-Forwarded-For", "5.6.7.8")

	if clientKey(a) != clientKey(b) {
		t.Error("X-Forwarded-For changes the bucket, so a client can mint unlimited quota by " +
			"varying a header it controls")
	}
}
