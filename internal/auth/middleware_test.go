package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/identity"
)

// okHandler records the principal it saw and answers 200.
func okHandler(seen *identity.Principal) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := identity.FromContext(r.Context()); ok {
			*seen = p
		}
		w.WriteHeader(http.StatusOK)
	})
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not a JSON error: %v", err)
	}
	return body.Error.Code
}

func TestRequireAuth(t *testing.T) {
	tokens := NewTokenManager(testSecret, time.Hour)
	valid, _, _ := tokens.Issue(7, identity.RoleCustomer)

	tests := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{"valid token", "Bearer " + valid, http.StatusOK},
		{"lowercase scheme", "bearer " + valid, http.StatusOK},
		{"missing header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + valid, http.StatusUnauthorized},
		{"no token", "Bearer ", http.StatusUnauthorized},
		{"garbage token", "Bearer not.a.jwt", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen identity.Principal
			h := RequireAuth(tokens)(okHandler(&seen))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusOK {
				if seen.UserID != 7 {
					t.Errorf("principal in context = %+v, want user 7", seen)
				}
				return
			}
			if code := errorCode(t, rec); code != httpx.CodeUnauthenticated {
				t.Errorf("code = %q, want %q", code, httpx.CodeUnauthenticated)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 response is missing the WWW-Authenticate header")
			}
		})
	}
}

func TestRequireRole(t *testing.T) {
	adminOnly := RequireRole(identity.RoleAdmin)

	tests := []struct {
		name       string
		principal  *identity.Principal
		wantStatus int
	}{
		{"admin allowed", &identity.Principal{UserID: 1, Role: identity.RoleAdmin}, http.StatusOK},
		{"customer forbidden", &identity.Principal{UserID: 2, Role: identity.RoleCustomer}, http.StatusForbidden},
		{"anonymous unauthenticated", nil, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen identity.Principal
			h := adminOnly(okHandler(&seen))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.principal != nil {
				req = req.WithContext(identity.NewContext(req.Context(), *tt.principal))
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusForbidden && errorCode(t, rec) != httpx.CodeForbidden {
				t.Errorf("code = %q, want FORBIDDEN", errorCode(t, rec))
			}
		})
	}
}
