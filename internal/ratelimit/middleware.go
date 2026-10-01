package ratelimit

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"

	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/metrics"
)

// KeyFunc picks WHO a request is counted against.
type KeyFunc func(r *http.Request) string

// ByIP counts requests per client IP address. Used for public routes
// (register, login, product browsing) where there is no user yet.
// trustedHops: see ClientIP.
func ByIP(trustedHops int) KeyFunc {
	return func(r *http.Request) string {
		return "ip:" + ClientIP(r, trustedHops)
	}
}

// ByUser counts requests per authenticated user, falling back to the IP.
// Must run after RequireAuth. Per-user limits are fairer than per-IP: many
// users behind one office NAT do not share one budget.
func ByUser(trustedHops int) KeyFunc {
	byIP := ByIP(trustedHops)
	return func(r *http.Request) string {
		if p, ok := identity.FromContext(r.Context()); ok {
			return "user:" + strconv.FormatInt(p.UserID, 10)
		}
		return byIP(r)
	}
}

// ClientIP finds the IP address of the real client.
//
// trustedHops = 0 (local Docker, direct connections): the TCP peer address
// (RemoteAddr). X-Forwarded-For is IGNORED, because any client can put any
// IP in it and dodge the limit.
//
// trustedHops = N (N load balancers in front, e.g. 1 on Render): the API
// only ever talks to the load balancer, so RemoteAddr is the SAME for every
// visitor. Each trusted proxy APPENDS the address it received the request
// from to X-Forwarded-For, so the entry N places from the right was written
// by our outermost proxy: that is the client. Entries further left were
// sent by the client and may be forged, so they are never used:
//
//	X-Forwarded-For: 6.6.6.6, 203.0.113.7    (6.6.6.6 forged by the client)
//	                          ^ appended by our 1 trusted proxy -> client IP
//
// If the header has fewer entries than expected (the request did not come
// through the proxy), we fall back to RemoteAddr.
func ClientIP(r *http.Request, trustedHops int) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if trustedHops <= 0 {
		return peer
	}

	var hops []string
	for _, header := range r.Header.Values("X-Forwarded-For") {
		for _, h := range strings.Split(header, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	if len(hops) < trustedHops {
		return peer
	}
	ip := hops[len(hops)-trustedHops]
	if net.ParseIP(ip) == nil { // garbage must not become a rate-limit key
		return peer
	}
	return ip
}

// Middleware rejects requests over the limit with 429 Too Many Requests.
//
// FAIL OPEN: if Redis is unreachable, the request is ALLOWED (and a warning
// logged). Rate limiting is protection, not correctness - refusing every
// request because the counter store is down would turn a Redis outage into
// a full API outage. (A bank's login endpoint might choose to fail closed.)
// m may be nil (no metrics).
func Middleware(l *Limiter, key KeyFunc, m *metrics.Metrics) func(http.Handler) http.Handler {
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
				m.RateLimited()
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
