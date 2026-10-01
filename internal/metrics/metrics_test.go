package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A nil *Metrics must be usable everywhere: code built without metrics
// (most tests, the migrate command) calls these methods freely.
func TestNilMetricsIsANoOp(t *testing.T) {
	var m *Metrics
	m.ObserveHTTP("GET", "/x", 200, 0.1)
	m.OrderCreated(2)
	m.OrderFailed("internal")
	m.ReservationsFinished("released", 1)
	m.PaymentSucceeded()
	m.PaymentFailed("declined")
	m.OutboxProcessed(1, 1, 1)
	m.ProductCache("hit")
	m.RateLimited()
}

func TestRecordingMethods(t *testing.T) {
	m := New()
	m.ObserveHTTP("GET", "/api/v1/orders/{id}", 404, 0.02)
	m.ObserveHTTP("GET", "/api/v1/orders/{id}", 404, 0.03)
	m.OrderCreated(3)
	m.OrderFailed("out_of_stock")
	m.ReservationsFinished("confirmed", 3)
	m.ReservationsFinished("expired", 0) // zero: no series created
	m.PaymentSucceeded()
	m.PaymentFailed("timeout")
	m.OutboxProcessed(5, 1, 0)
	m.ProductCache("miss")
	m.RateLimited()

	checks := []struct {
		name   string
		labels []string
		want   float64
	}{
		{"http_requests_total", []string{"method", "GET", "route", "/api/v1/orders/{id}", "status", "404"}, 2},
		{"http_request_duration_seconds", []string{"method", "GET", "route", "/api/v1/orders/{id}"}, 2},
		{"orders_created_total", nil, 1},
		{"orders_failed_total", []string{"reason", "out_of_stock"}, 1},
		{"inventory_reservations_total", []string{"event", "created"}, 3},
		{"inventory_reservations_total", []string{"event", "confirmed"}, 3},
		{"inventory_reservations_total", []string{"event", "expired"}, 0},
		{"payments_success_total", nil, 1},
		{"payments_failed_total", []string{"reason", "timeout"}, 1},
		{"outbox_events_processed_total", []string{"result", "published"}, 5},
		{"outbox_events_processed_total", []string{"result", "retry"}, 1},
		{"product_cache_requests_total", []string{"result", "miss"}, 1},
		{"http_rate_limited_total", nil, 1},
	}
	for _, c := range checks {
		if got := m.Value(c.name, c.labels...); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
}

func TestHandlerServesPrometheusText(t *testing.T) {
	m := New()
	m.OrderCreated(1)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, want := range []string{
		"# HELP orders_created_total",
		"# TYPE orders_created_total counter",
		"orders_created_total 1",
		"go_goroutines",                 // Go runtime collector
		"process_resident_memory_bytes", // process collector (Linux/Windows)
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics output is missing %q", want)
		}
	}
}

// Two instances do not share state (no global registry): tests can run
// in parallel, each with its own metrics.
func TestInstancesAreIndependent(t *testing.T) {
	a, b := New(), New()
	a.RateLimited()
	if b.Value("http_rate_limited_total") != 0 {
		t.Error("metrics leaked between instances")
	}
}
