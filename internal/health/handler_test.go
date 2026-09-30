package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These are unit tests: the dependency checks are fake functions, so no
// real PostgreSQL is needed. That lets us test the failure path easily.

func TestHealth_AlwaysOK(t *testing.T) {
	// Even a failing dependency must not affect liveness.
	h := NewHandler(map[string]CheckFunc{
		"postgres": func(context.Context) error { return errors.New("down") },
	})

	rec := httptest.NewRecorder()
	h.Health(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestReady_AllChecksPass(t *testing.T) {
	h := NewHandler(map[string]CheckFunc{
		"postgres": func(context.Context) error { return nil },
	})

	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ready" || body.Checks["postgres"] != "ok" {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestReady_FailingCheckReturns503WithoutLeakingError(t *testing.T) {
	h := NewHandler(map[string]CheckFunc{
		"postgres": func(context.Context) error { return nil },
		"redis":    func(context.Context) error { return errors.New("dial tcp 10.0.0.5:6379: secret internal detail") },
	})

	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "not_ready" || body.Checks["redis"] != "unavailable" || body.Checks["postgres"] != "ok" {
		t.Errorf("unexpected body: %+v", body)
	}
	if strings.Contains(rec.Body.String(), "secret internal detail") {
		t.Error("internal error text must not be returned to the client")
	}
}

func TestReady_SlowCheckTimesOut(t *testing.T) {
	h := NewHandler(map[string]CheckFunc{
		"postgres": func(ctx context.Context) error {
			<-ctx.Done() // simulate a dependency that never answers
			return ctx.Err()
		},
	})
	h.timeout = 10 * time.Millisecond // keep the test fast

	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
