package ratelimit

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"

	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/identity"
)

// KeyFunc picks WHO a request is counted against.
type KeyFunc func(r *http.Request) string

// ByIP counts requests per client IP address. Used for public routes
// (register, login, product browsing) where there is no user yet.
//
// We use the TCP peer address (RemoteAddr). We do NOT trust the
// X-Forwarded-For header: any client can put any IP in it and dodge the
// limit. Behind a real load balancer you would configure which proxy's
// header to trust. (Locally through Docker, every client appears as the
// Docker gateway IP, so all anonymous traffic shares one budget.)
func ByIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "ip:" + host
}

// ByUser counts requests per authenticated user, falling back to the IP.
// Must run after RequireAuth. Per-user limits are fairer than per-IP: many
// users behind one office NAT do not share one budget.
func ByUser(r *http.Request) string {
	if p, ok := identity.FromContext(r.Context()); ok {
		return "user:" + strconv.FormatInt(p.UserID, 10)
	}
	return ByIP(r)
}

// Middleware rejects requests over the limit with 429 Too Many Requests.
//
// FAIL OPEN: if Redis is unreachable, the request is ALLOWED (and a warning
// logged). Rate limiting is protection, not correctness - refusing every
// request because the counter store is down would turn a Redis outage into
// a full API outage. (A bank's login endpoint might choose to fail closed.)
func Middleware(l *Limiter, key KeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d, err := l.Allow(r.Context(), key(r))
			if err != nil {
				slog.WarnContext(r.Context(), "rate limiter unavailable; allowing request", "error", err)
				next.ServeHTTP(w, r)
				return
			}

			// Standard-ish headers so well-behaved clients can slow down
			// before they hit the limit.
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(d.Limit))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))

			if !d.Allowed {
				retry := int(math.Ceil(d.RetryAfter.Seconds()))
				w.Header().Set("Retry-After", strconv.Itoa(retry))
				httpx.WriteError(w, r, http.StatusTooManyRequests, httpx.CodeRateLimited,
					"too many requests; retry after "+strconv.Itoa(retry)+" seconds")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
