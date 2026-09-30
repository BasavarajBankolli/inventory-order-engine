package auth

import (
	"net/http"
	"slices"
	"strings"

	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/identity"
)

// RequireAuth returns middleware that only lets requests with a valid
// "Authorization: Bearer <token>" header through. On success the caller's
// identity is stored in the request context for handlers to read.
func RequireAuth(tokens *TokenManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				unauthenticated(w, r, "missing or malformed Authorization header")
				return
			}

			p, err := tokens.Verify(token)
			if err != nil {
				unauthenticated(w, r, "invalid or expired token")
				return
			}

			ctx := identity.NewContext(r.Context(), p)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole returns middleware that allows only the given roles.
// It must run AFTER RequireAuth.
//
// 401 vs 403:
//
//	401 Unauthorized = "I don't know who you are"   (no/invalid token)
//	403 Forbidden    = "I know who you are, but no" (wrong role)
func RequireRole(allowed ...identity.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := identity.FromContext(r.Context())
			if !ok {
				unauthenticated(w, r, "authentication required")
				return
			}
			if !slices.Contains(allowed, p.Role) {
				httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "you do not have permission to perform this action")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the token from "Authorization: Bearer <token>".
// The scheme name is case-insensitive per the HTTP spec.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func unauthenticated(w http.ResponseWriter, r *http.Request, message string) {
	// Tells clients which authentication scheme this API expects.
	w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
	httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, message)
}
