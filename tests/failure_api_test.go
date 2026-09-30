package tests

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"inventory-order-engine/internal/app"
	"inventory-order-engine/internal/auth"
	"inventory-order-engine/internal/config"
	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/logging"
)

// DATABASE FAILURE: PostgreSQL is unreachable while the API is running.
//
// Expected behaviour:
//   - /health stays 200 (the process is fine; restarting it would not help)
//   - /ready says 503 (take this instance out of rotation)
//   - every endpoint that needs the database answers a clean JSON 500 with
//     a request_id - never a hang, never a crash, never internal details.
func TestDatabaseDown_APIFailsCleanly(t *testing.T) {
	// A pool pointing at a port where nothing listens. pgxpool connects
	// lazily, so creating it succeeds; every query then fails.
	pool, err := pgxpool.New(context.Background(), "postgres://app:secret-password@127.0.0.1:1/inventory?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	cfg := config.Config{JWTSecret: "api-test-secret-that-is-at-least-32-bytes", JWTTTL: time.Hour,
		ReservationTTL: time.Minute, PaymentTimeout: time.Second, PaymentReconcileAfter: time.Minute,
		MockPaymentOutcome: "SUCCESS"}
	handler, err := app.NewHandler(cfg, pool, logging.New(io.Discard, slog.LevelInfo), app.Options{BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	api := &testAPI{t: t, srv: srv}

	expectStatus(t, api.do("GET", "/health", nil, "", nil), http.StatusOK)

	var ready struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	expectStatus(t, api.do("GET", "/ready", nil, "", &ready), http.StatusServiceUnavailable)
	if ready.Status != "not_ready" || ready.Checks["postgres"] != "unavailable" {
		t.Errorf("/ready = %+v", ready)
	}

	// A valid token, so authenticated endpoints get past the middleware
	// (tokens are verified without the database) and hit PostgreSQL.
	token, _, _ := auth.NewTokenManager(cfg.JWTSecret, time.Hour).Issue(1, identity.RoleAdmin)

	requests := []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/v1/auth/login", map[string]string{"email": "a@b.com", "password": "whatever-1"}},
		{"POST", "/api/v1/auth/register", map[string]string{"email": "a@b.com", "name": "A", "password": "whatever-1"}},
		{"GET", "/api/v1/products", nil},
		{"GET", "/api/v1/products/1", nil},
		{"GET", "/api/v1/users/me", nil},
		{"POST", "/api/v1/orders", orderBody([2]int64{1, 1})},
		{"GET", "/api/v1/orders", nil},
		{"POST", "/api/v1/orders/1/pay", nil},
		{"PATCH", "/api/v1/products/1/inventory", map[string]int{"adjustment": 1}},
	}
	for _, rq := range requests {
		t.Run(rq.method+" "+rq.path, func(t *testing.T) {
			start := time.Now()
			var body errorResponse
			resp := api.do(rq.method, rq.path, rq.body, token, &body)

			expectStatus(t, resp, http.StatusInternalServerError)
			if body.Error.Code != "INTERNAL_ERROR" || body.Error.RequestID == "" {
				t.Errorf("error = %+v, want INTERNAL_ERROR with a request_id", body.Error)
			}
			// Internal details (host, port, driver messages, the DB
			// password) must stay in the logs.
			for _, secret := range []string{"127.0.0.1", "dial", "secret-password", "connect"} {
				if strings.Contains(body.Error.Message, secret) {
					t.Errorf("message leaks %q: %q", secret, body.Error.Message)
				}
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("took %v; a dead database must not hang requests", d)
			}
		})
	}
}
