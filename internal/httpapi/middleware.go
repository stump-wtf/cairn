package httpapi

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/joestump/cairn/internal/errs"
)

// securityHeaders sets baseline response headers on every response: nosniff so a
// body's declared type is honored, a strict CSP for any HTML the API might emit,
// a no-referrer Referrer-Policy so an artifact id never leaks to a third party
// via the Referer header, and HSTS over TLS (SPEC-0002 REQ "Security Headers",
// SPEC-0006 REQ "Security Headers").
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimit throttles per-IP, returning 429 with Retry-After when exceeded. Id
// resolution and create are the chief targets, part of the ADR-0005
// defense-in-depth against enumeration (SPEC-0002 REQ "Rate Limiting").
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.limiter == nil {
			next.ServeHTTP(w, r)
			return
		}
		ok, retry := s.limiter.allow(clientIP(r))
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
			s.writeError(w, r, errs.New(errs.CodeRateLimited, "rate limit exceeded"), nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP extracts the client host for rate-limit keying. chi's RealIP
// middleware normalizes RemoteAddr from X-Forwarded-For upstream.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter is a per-key token bucket. rate is tokens per second, burst the
// bucket capacity. now is injectable for deterministic tests. Idle buckets are
// swept periodically so a stream of distinct client IPs cannot grow the map
// without bound (a slow memory-DoS); idleTTL of 0 disables the sweep.
type rateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*tokenBucket
	rate      float64
	burst     float64
	idleTTL   time.Duration
	lastSweep time.Time
	now       func() time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(ratePerSec float64, burst int) *rateLimiter {
	if ratePerSec <= 0 || burst <= 0 {
		return nil // disabled
	}
	// A bucket idle at least idleTTL has fully refilled to burst, so evicting and
	// later recreating it is indistinguishable from keeping it — no fairness loss.
	// Floor the TTL so tiny rates don't produce an unboundedly long sweep interval.
	idleTTL := time.Duration(float64(burst) / ratePerSec * float64(time.Second))
	if idleTTL < 10*time.Minute {
		idleTTL = 10 * time.Minute
	}
	return &rateLimiter{
		buckets:   make(map[string]*tokenBucket),
		rate:      ratePerSec,
		burst:     float64(burst),
		idleTTL:   idleTTL,
		lastSweep: time.Now(),
		now:       time.Now,
	}
}

// allow consumes one token for key, returning whether it was permitted and, if
// not, how long until a token frees up.
func (rl *rateLimiter) allow(key string) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	rl.sweepLocked(now)
	b, ok := rl.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	}
	// Refill based on elapsed time, capped at burst.
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(rl.burst, b.tokens+elapsed*rl.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	needed := (1 - b.tokens) / rl.rate
	return false, time.Duration(needed * float64(time.Second))
}

// sweepLocked drops buckets untouched for at least idleTTL, at most once per
// idleTTL so the O(n) scan is amortized. The caller must hold rl.mu. An idleTTL
// of 0 disables sweeping entirely.
func (rl *rateLimiter) sweepLocked(now time.Time) {
	if rl.idleTTL <= 0 || now.Sub(rl.lastSweep) < rl.idleTTL {
		return
	}
	for k, b := range rl.buckets {
		if now.Sub(b.last) >= rl.idleTTL {
			delete(rl.buckets, k)
		}
	}
	rl.lastSweep = now
}
