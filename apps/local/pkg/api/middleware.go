package api

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// statusRecorder wraps http.ResponseWriter to capture the status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.statusCode = code
	sr.ResponseWriter.WriteHeader(code)
}

// LoggingMiddleware logs all incoming requests as structured JSON.
func LoggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(sr, r)
		duration := time.Since(start)
		slog.Info("Request processed",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sr.statusCode,
			"duration", duration.String(),
			"remote_addr", r.RemoteAddr,
		)
	}
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
// Returns "" if the header is missing or malformed.
func bearerToken(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return ""
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
		return ""
	}
	return parts[1]
}

// UserAuthMiddleware enforces bearer token authentication using AGENTLEDGER_USER_TOKEN.
// This token grants access to user-agent endpoints only: /authorize, GET /budget/, GET /transactions/, GET /status/.
func UserAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		expectedKey := os.Getenv("AGENTLEDGER_USER_TOKEN")
		if expectedKey == "" {
			http.Error(w, "Server improperly configured (missing AGENTLEDGER_USER_TOKEN)", http.StatusInternalServerError)
			return
		}
		token := bearerToken(r)
		if token == "" {
			http.Error(w, "Unauthorized: missing or malformed Authorization header", http.StatusUnauthorized)
			return
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(expectedKey)) != 1 {
			http.Error(w, "Unauthorized: invalid token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}
}

// SupervisorAuthMiddleware enforces bearer token authentication using AGENTLEDGER_SUPERVISOR_TOKEN.
// This token grants access to supervisor endpoints only: /approve, /deny, /pending, /budget/ (write), /credit, /vault/update.
// The supervisor token is intentionally separate from the user token — a supervisor agent cannot initiate spends,
// and a user agent cannot approve, deny, or manage budgets.
func SupervisorAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		expectedKey := os.Getenv("AGENTLEDGER_SUPERVISOR_TOKEN")
		if expectedKey == "" {
			http.Error(w, "Server improperly configured (missing AGENTLEDGER_SUPERVISOR_TOKEN)", http.StatusInternalServerError)
			return
		}
		token := bearerToken(r)
		if token == "" {
			http.Error(w, "Unauthorized: missing or malformed Authorization header", http.StatusUnauthorized)
			return
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(expectedKey)) != 1 {
			http.Error(w, "Unauthorized: invalid token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}
}

// EitherAuthMiddleware accepts either the user token or the supervisor token.
// Use this for read-only endpoints that both roles should access (e.g. GET /transactions/).
func EitherAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userKey := os.Getenv("AGENTLEDGER_USER_TOKEN")
		supervisorKey := os.Getenv("AGENTLEDGER_SUPERVISOR_TOKEN")
		if userKey == "" && supervisorKey == "" {
			http.Error(w, "Server improperly configured (missing auth tokens)", http.StatusInternalServerError)
			return
		}
		token := bearerToken(r)
		if token == "" {
			http.Error(w, "Unauthorized: missing or malformed Authorization header", http.StatusUnauthorized)
			return
		}
		userMatch := userKey != "" && subtle.ConstantTimeCompare([]byte(token), []byte(userKey)) == 1
		supervisorMatch := supervisorKey != "" && subtle.ConstantTimeCompare([]byte(token), []byte(supervisorKey)) == 1
		if !userMatch && !supervisorMatch {
			http.Error(w, "Unauthorized: invalid token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}
}

// SupervisorTierMiddleware returns a middleware that enforces a minimum supervisor tier.
// If AGENTLEDGER_SUPERVISOR_TIER is below minTier, the request is rejected with 403.
// Tier 1 (default): approve, deny, list_pending
// Tier 2 (opt-in):  + set_budget, credit
// Tier 3 (opt-in, explicit consent required at init): + vault_update
func SupervisorTierMiddleware(minTier int, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tierStr := os.Getenv("AGENTLEDGER_SUPERVISOR_TIER")
		tier := 1 // default
		if tierStr != "" {
			if t, err := strconv.Atoi(tierStr); err == nil {
				tier = t
			} else {
				http.Error(w, "Server improperly configured (invalid AGENTLEDGER_SUPERVISOR_TIER)", http.StatusInternalServerError)
				return
			}
		}
		if tier < minTier {
			http.Error(w, "Forbidden: supervisor tier too low for this operation", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}
}

// BodyLimitMiddleware caps the request body at 1 MB to prevent memory exhaustion from oversized payloads.
func BodyLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB
		}
		next.ServeHTTP(w, r)
	}
}

// rateLimiter implements a simple per-IP token bucket rate limiter.
// Designed for the local edition's sidecar use case (small number of clients).
type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	rate     int           // tokens added per interval
	interval time.Duration // how often tokens are added
	burst    int           // max tokens (bucket capacity)
}

type bucket struct {
	tokens   int
	lastFill time.Time
}

func newRateLimiter(rate int, interval time.Duration, burst int) *rateLimiter {
	rl := &rateLimiter{
		buckets:  make(map[string]*bucket),
		rate:     rate,
		interval: interval,
		burst:    burst,
	}
	// Periodically evict stale buckets to prevent unbounded map growth.
	go func() {
		for range time.Tick(5 * time.Minute) {
			rl.evictStale(5 * time.Minute)
		}
	}()
	return rl
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.burst, lastFill: time.Now()}
		rl.buckets[key] = b
	}

	// Refill tokens based on elapsed time
	elapsed := time.Since(b.lastFill)
	refill := int(elapsed/rl.interval) * rl.rate
	if refill > 0 {
		b.tokens += refill
		if b.tokens > rl.burst {
			b.tokens = rl.burst
		}
		b.lastFill = time.Now()
	}

	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

// evictStale removes buckets that have not been used within maxAge.
func (rl *rateLimiter) evictStale(maxAge time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	for key, b := range rl.buckets {
		if now.Sub(b.lastFill) > maxAge {
			delete(rl.buckets, key)
		}
	}
}

// defaultRateLimiter allows 20 requests per second per IP with a burst of 40.
// Generous for legitimate use; stops brute-force attempts.
var defaultRateLimiter = newRateLimiter(20, time.Second, 40)

// RateLimitMiddleware rejects requests that exceed the per-IP rate limit.
func RateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Use RemoteAddr (ip:port) stripped to just IP as the key
		ip := r.RemoteAddr
		if idx := strings.LastIndex(ip, ":"); idx != -1 {
			ip = ip[:idx]
		}
		if !defaultRateLimiter.allow(ip) {
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	}
}
