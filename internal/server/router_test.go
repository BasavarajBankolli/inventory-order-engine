package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"inventory-order-engine/internal/health"
	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/logging"
	"inventory-order-engine/internal/requestid"
)

// newTestServer starts a real HTTP server (on a random local port) running
// the full router, including all middleware.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	router := NewRouter(Deps{
		Logger: logging.New(io.Discard, slog.LevelInfo),
		Health: health.NewHandler(map[string]health.CheckFunc{
			"postgres": func(context.Context) error { return nil },
		}),
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func TestRouter_HealthAndReady(t *testing.T) {
	srv := newTestServer(t)

	for _, path := range []string{"/health", "/ready"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
		if resp.Header.Get(requestid.Header) == "" {
			t.Errorf("GET %s has no X-Request-ID header", path)
		}
	}
}

func TestRouter_UnknownRouteReturnsJSON404(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get(srv.URL + "/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var body httpx.ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("404 body is not our JSON error format: %v", err)
	}
	if body.Error.Code != httpx.CodeNotFound {
		t.Errorf("code = %q, want %q", body.Error.Code, httpx.CodeNotFound)
	}
	if body.Error.RequestID != resp.Header.Get(requestid.Header) {
		t.Errorf("body request_id %q != header %q", body.Error.RequestID, resp.Header.Get(requestid.Header))
	}
}

func TestRouter_WrongMethodReturnsJSON405(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Post(srv.URL+"/health", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	var body httpx.ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("405 body is not our JSON error format: %v", err)
	}
	if body.Error.Code != httpx.CodeMethodNotAllowed {
		t.Errorf("code = %q, want %q", body.Error.Code, httpx.CodeMethodNotAllowed)
	}
}
