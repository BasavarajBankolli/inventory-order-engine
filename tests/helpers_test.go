// Package tests contains end-to-end API tests: real HTTP requests go
// through the real router, middleware, services and repositories into a
// real PostgreSQL schema. Only the network socket is local (httptest).
//
// They are skipped unless TEST_DATABASE_URL is set.
package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"inventory-order-engine/internal/app"
	"inventory-order-engine/internal/config"
	"inventory-order-engine/internal/logging"
	"inventory-order-engine/internal/testutil"
)

// testAPI is a running API server backed by a fresh, migrated schema.
type testAPI struct {
	t   *testing.T
	srv *httptest.Server
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	pool := testutil.NewMigratedPool(t)

	cfg := config.Config{
		JWTSecret: "api-test-secret-that-is-at-least-32-bytes",
		JWTTTL:    time.Hour,
	}
	handler, err := app.NewHandler(cfg, pool, logging.New(io.Discard, slog.LevelInfo),
		app.Options{BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &testAPI{t: t, srv: srv}
}

// do sends a request with an optional JSON body and bearer token, and
// decodes the JSON response into out (if out is not nil).
func (a *testAPI) do(method, path string, body any, token string, out any) *http.Response {
	a.t.Helper()

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, a.srv.URL+path, reader)
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			a.t.Fatalf("%s %s: decode response: %v", method, path, err)
		}
	}
	return resp
}

// errorResponse matches the standard error body.
type errorResponse struct {
	Error struct {
		Code      string            `json:"code"`
		Message   string            `json:"message"`
		Fields    map[string]string `json:"fields"`
		RequestID string            `json:"request_id"`
	} `json:"error"`
}

func expectStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status = %d, want %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want)
	}
}
