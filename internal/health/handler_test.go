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
// real PostgreSQL or Redis is needed. That lets us test the failure paths.

var (
	up   = func(context.Context) error { return nil }
	down = func(context.Context) error { return errors.New("dial tcp 10.0.0.5:6379: secret internal detail") }
)

func ready(t *testing.T, h *Handler) (int, response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	var body response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return rec.Code, body, rec.Body.String()
}

func TestHealth_AlwaysOK(t *testing.T) {
	// Even a failing required dependency must not affect liveness.
	h := NewHandler(Check{Name: "postgres", Fn: down, Required: true})

	rec := httptest.NewRecorder()
	h.Health(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestReady(t *testing.T) {
	tests := []struct {
		name       string
		postgres   CheckFunc
		redis      CheckFunc
		wantCode   int
		wantStatus string
	}{
		{"all up", up, up, 200, "ready"},
		{"optional redis down", up, down, 200, "degraded"},
		{"required postgres down", down, up, 503, "not_ready"},
		{"both down", down, down, 503, "not_ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHandler(
				Check{Name: "postgres", Fn: tt.postgres, Required: true},
				Check{Name: "redis", Fn: tt.redis, Required: false},
			)
			code, body, raw := ready(t, h)
			if code != tt.wantCode || body.Status != tt.wantStatus {
				t.Errorf("got %d %q, want %d %q", code, body.Status, tt.wantCode, tt.wantStatus)
			}
			if strings.Contains(raw, "secret internal detail") {
				t.Error("internal error text must not be returned to the client")
			}
		})
	}
}

func TestReady_ReportsEachDependency(t *testing.T) {
	h := NewHandler(
		Check{Name: "postgres", Fn: up, Required: true},
		Check{Name: "redis", Fn: down},
	)
	_, body, _ := ready(t, h)
	if body.Checks["postgres"] != "ok" || body.Checks["redis"] != "unavailable" {
		t.Errorf("checks = %v", body.Checks)
	}
}

func TestReady_SlowCheckTimesOut(t *testing.T) {
	h := NewHandler(Check{Name: "postgres", Required: true, Fn: func(ctx context.Context) error {
		<-ctx.Done() // simulate a dependency that never answers
		return ctx.Err()
	}})
	h.timeout = 10 * time.Millisecond // keep the test fast

	if code, _, _ := ready(t, h); code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
}
