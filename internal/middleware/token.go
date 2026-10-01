package middleware

import (
	"crypto/subtle"
	"net/http"

	"inventory-order-engine/internal/httpx"
)

// StaticBearerToken protects an operational endpoint (such as /metrics) with
// one shared secret: requests must send "Authorization: Bearer <token>".
// An empty token means no protection (local development).
//
// This is for machines (Prometheus), not users: users get JWTs. Prometheus
// sends it with `authorization: { credentials: <token> }` in its scrape config.
func StaticBearerToken(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if token == "" {
			return next
		}
		want := []byte("Bearer " + token)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// ConstantTimeCompare: a normal == stops at the first wrong byte,
			// and that timing difference can leak the secret byte by byte.
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
				httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "a valid metrics token is required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
