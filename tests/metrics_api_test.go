package tests

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"inventory-order-engine/internal/app"
	"inventory-order-engine/internal/config"
	"inventory-order-engine/internal/metrics"
)

// GET /metrics after real traffic: HTTP metrics are labelled by route
// pattern (never by raw id) and business metrics follow what the API did.
func TestMetricsEndpoint(t *testing.T) {
	m := metrics.New()
	api := newTestAPIWith(t, func(_ *config.Config, o *app.Options) { o.Metrics = m })

	admin := api.loginAs("admin@x.com", true)
	alice := api.loginAs("alice@x.com", false)
	product := api.createStockedProduct(admin, "MET-1", 1000, 5)

	orderID := api.placeOrder(alice, product, 2)
	expectStatus(t, api.do("POST", payPath(orderID), nil, alice, nil), http.StatusOK)
	expectStatus(t, api.do("GET", "/api/v1/products/987654", nil, "", nil), http.StatusNotFound)

	resp, err := http.Get(api.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusOK)
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)

	for _, want := range []string{
		`http_requests_total{method="GET",route="/api/v1/products/{id}",status="404"} 1`,
		`http_requests_total{method="POST",route="/api/v1/orders/{id}/pay",status="200"} 1`,
		`http_request_duration_seconds_bucket{method="POST",route="/api/v1/orders",le="+Inf"} 1`,
		`orders_created_total 1`,
		`payments_success_total 1`,
		`inventory_reservations_total{event="confirmed"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %s", want)
		}
	}
	// Raw ids in labels would create one time series per order/product.
	if strings.Contains(body, "/987654") || strings.Contains(body, `route="/api/v1/orders/`+strconv.FormatInt(orderID, 10)) {
		t.Error("a raw id leaked into a metric label")
	}
}

// Without metrics configured, the endpoint does not exist.
func TestMetricsEndpoint_DisabledWithoutMetrics(t *testing.T) {
	api := newTestAPI(t)
	resp, err := http.Get(api.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	expectStatus(t, resp, http.StatusNotFound)
}
