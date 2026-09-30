package tests

import (
	"net/http"
	"strconv"
	"testing"
)

type payResponse struct {
	Order   orderResponse `json:"order"`
	Payment struct {
		ID                int64  `json:"id"`
		Status            string `json:"status"`
		Amount            int64  `json:"amount"`
		ProviderReference string `json:"provider_reference"`
		FailureReason     string `json:"failure_reason"`
	} `json:"payment"`
}

func (a *testAPI) placeOrder(token string, productID int64, qty int64) int64 {
	a.t.Helper()
	var o orderResponse
	expectStatus(a.t, a.do("POST", "/api/v1/orders", orderBody([2]int64{productID, qty}), token, &o), http.StatusCreated)
	return o.ID
}

func payPath(orderID int64) string {
	return "/api/v1/orders/" + strconv.FormatInt(orderID, 10) + "/pay"
}

func TestPayments_API_SuccessFailureTimeout(t *testing.T) {
	api := newTestAPI(t)
	admin := api.loginAs("admin@example.com", true)
	alice := api.loginAs("alice@example.com", false)
	p := api.createStockedProduct(admin, "PAY-API", 1000, 10)

	// SUCCESS (empty body = default outcome): 200, CONFIRMED, units sold.
	o1 := api.placeOrder(alice, p, 2)
	var ok payResponse
	expectStatus(t, api.do("POST", payPath(o1), nil, alice, &ok), http.StatusOK)
	if ok.Order.Status != "CONFIRMED" || ok.Payment.Status != "SUCCEEDED" || ok.Payment.ProviderReference == "" {
		t.Errorf("success = %+v", ok)
	}
	if inv := api.stockOf(admin, p); inv.AvailableQuantity != 8 || inv.ReservedQuantity != 0 {
		t.Errorf("after success: %+v, want 8/0", inv)
	}

	// FAILURE: 402, order cancelled, stock restored.
	o2 := api.placeOrder(alice, p, 3)
	var declined errorResponse
	expectStatus(t, api.do("POST", payPath(o2), map[string]string{"simulate": "FAILURE"}, alice, &declined), http.StatusPaymentRequired)
	if declined.Error.Code != "PAYMENT_FAILED" {
		t.Errorf("code = %q, want PAYMENT_FAILED", declined.Error.Code)
	}
	var cancelled orderResponse
	api.do("GET", "/api/v1/orders/"+strconv.FormatInt(o2, 10), nil, alice, &cancelled)
	if cancelled.Status != "CANCELLED" {
		t.Errorf("order after decline = %s, want CANCELLED", cancelled.Status)
	}
	if inv := api.stockOf(admin, p); inv.AvailableQuantity != 8 || inv.ReservedQuantity != 0 {
		t.Errorf("after decline: %+v, want 8/0 (the 3 units came back)", inv)
	}

	// TIMEOUT: 504, order stays PAYMENT_PENDING, stock stays reserved...
	o3 := api.placeOrder(alice, p, 1)
	var timeout errorResponse
	expectStatus(t, api.do("POST", payPath(o3), map[string]string{"simulate": "TIMEOUT"}, alice, &timeout), http.StatusGatewayTimeout)
	if timeout.Error.Code != "PAYMENT_TIMEOUT" {
		t.Errorf("code = %q, want PAYMENT_TIMEOUT", timeout.Error.Code)
	}
	if inv := api.stockOf(admin, p); inv.ReservedQuantity != 1 {
		t.Errorf("after timeout: reserved = %d, want 1 (outcome unknown)", inv.ReservedQuantity)
	}
	// ...and the retry discovers the success.
	var retried payResponse
	expectStatus(t, api.do("POST", payPath(o3), nil, alice, &retried), http.StatusOK)
	if retried.Order.Status != "CONFIRMED" {
		t.Errorf("after retry: %s, want CONFIRMED", retried.Order.Status)
	}
}

func TestPayments_API_Errors(t *testing.T) {
	api := newTestAPI(t)
	admin := api.loginAs("admin@example.com", true)
	alice := api.loginAs("alice@example.com", false)
	bob := api.loginAs("bob@example.com", false)
	p := api.createStockedProduct(admin, "PAY-ERR", 1000, 10)
	o := api.placeOrder(alice, p, 1)

	expectStatus(t, api.do("POST", payPath(o), nil, "", nil), http.StatusUnauthorized)
	expectStatus(t, api.do("POST", payPath(o), nil, bob, nil), http.StatusNotFound)

	var bad errorResponse
	expectStatus(t, api.do("POST", payPath(o), map[string]string{"simulate": "MAYBE"}, alice, &bad), http.StatusBadRequest)
	if bad.Error.Fields["simulate"] == "" {
		t.Errorf("fields = %v, want a simulate problem", bad.Error.Fields)
	}

	expectStatus(t, api.do("POST", "/api/v1/orders/"+strconv.FormatInt(o, 10)+"/cancel", nil, alice, nil), http.StatusOK)
	var invalid errorResponse
	expectStatus(t, api.do("POST", payPath(o), nil, alice, &invalid), http.StatusConflict)
	if invalid.Error.Code != "INVALID_STATE_TRANSITION" {
		t.Errorf("code = %q, want INVALID_STATE_TRANSITION", invalid.Error.Code)
	}
}
