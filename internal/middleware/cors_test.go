package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func corsRequest(t *testing.T, allowed []string, method, origin string, preflight bool) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	called := false
	h := CORS(allowed)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, "/api/v1/orders", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if preflight {
		req.Header.Set("Access-Control-Request-Method", "POST")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, called
}

func TestCORS_AllowedOrigin(t *testing.T) {
	rec, called := corsRequest(t, []string{"http://localhost:5173"}, "GET", "http://localhost:5173", false)
	if !called || rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Errorf("called=%v headers=%v", called, rec.Header())
	}
	if rec.Header().Get("Access-Control-Expose-Headers") == "" || rec.Header().Get("Vary") != "Origin" {
		t.Errorf("missing Expose-Headers/Vary: %v", rec.Header())
	}
}

func TestCORS_Preflight(t *testing.T) {
	rec, called := corsRequest(t, []string{"https://shop.example"}, "OPTIONS", "https://shop.example", true)
	if called {
		t.Error("preflight must be answered by the middleware, not routed")
	}
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Headers") == "" ||
		rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Errorf("preflight: %d %v", rec.Code, rec.Header())
	}
}

func TestCORS_DisallowedOriginGetsNoHeaders(t *testing.T) {
	rec, called := corsRequest(t, []string{"https://shop.example"}, "GET", "https://evil.example", false)
	if !called || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("called=%v headers=%v", called, rec.Header())
	}
}

func TestCORS_NoOriginAndWildcard(t *testing.T) {
	// Same-origin / curl requests are untouched.
	if rec, called := corsRequest(t, []string{"*"}, "GET", "", false); !called || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("no Origin: called=%v headers=%v", called, rec.Header())
	}
	if rec, _ := corsRequest(t, []string{"*"}, "GET", "https://any.example", false); rec.Header().Get("Access-Control-Allow-Origin") != "https://any.example" {
		t.Errorf("wildcard: %v", rec.Header())
	}
	// Empty list = CORS disabled.
	if rec, _ := corsRequest(t, nil, "GET", "https://any.example", false); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("disabled: %v", rec.Header())
	}
}
