package middleware

import (
	"net/http"
	"slices"
	"strings"
)

// CORS lets browser apps served from OTHER origins (the React frontend on
// localhost:5173 or on Vercel) call this API.
//
// Browsers block a page from reading responses of another origin unless the
// server says it is allowed (the Same-Origin Policy). CORS is how the server
// says so:
//
//	request:   Origin: https://shop.vercel.app
//	response:  Access-Control-Allow-Origin: https://shop.vercel.app
//
// For requests with an Authorization header or a JSON body, the browser
// first sends a "preflight" OPTIONS request asking which methods and headers
// are allowed; we answer it here with 204 before routing.
//
// Only origins in `allowed` get these headers ("*" allows any origin). We
// use bearer tokens, not cookies, so no Access-Control-Allow-Credentials.
// An empty list disables CORS entirely (same-origin use, curl, Postman).
func CORS(allowed []string) func(http.Handler) http.Handler {
	allowAll := slices.Contains(allowed, "*")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" || (!allowAll && !slices.Contains(allowed, origin)) {
				next.ServeHTTP(w, r) // not a (permitted) cross-origin request
				return
			}

			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			// Responses differ per Origin: tell caches not to mix them up.
			h.Add("Vary", "Origin")
			// Let JavaScript read these response headers too.
			h.Set("Access-Control-Expose-Headers", strings.Join([]string{
				"X-Request-ID", "Location", "Idempotent-Replayed", "Retry-After",
				"X-RateLimit-Limit", "X-RateLimit-Remaining",
			}, ", "))

			// Preflight: answer and stop (no routing, no auth, no rate limit).
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-ID")
				h.Set("Access-Control-Max-Age", "600") // browsers may cache this answer for 10 min
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
