package tests

import (
	"net/http"
	"strconv"
	"testing"
)

type orderItemResponse struct {
	ProductID  int64 `json:"product_id"`
	Quantity   int   `json:"quantity"`
	UnitPrice  int64 `json:"unit_price"`
	TotalPrice int64 `json:"total_price"`
}

type orderResponse struct {
	ID          int64               `json:"id"`
	UserID      int64               `json:"user_id"`
	Status      string              `json:"status"`
	TotalAmount int64               `json:"total_amount"`
	Currency    string              `json:"currency"`
	Items       []orderItemResponse `json:"items"`
}

type orderListResponse struct {
	Items []orderResponse `json:"items"`
	Total int64           `json:"total"`
}

func orderBody(lines ...[2]int64) map[string]any {
	items := make([]map[string]any, len(lines))
	for i, l := range lines {
		items[i] = map[string]any{"product_id": l[0], "quantity": l[1]}
	}
	return map[string]any{"items": items}
}

func TestOrders_CreateGetListCancel(t *testing.T) {
	api := newTestAPI(t)
	admin := api.loginAs("admin@example.com", true)
	alice := api.loginAs("alice@example.com", false)
	kb := api.createStockedProduct(admin, "KB-1", 249900, 10)
	mug := api.createStockedProduct(admin, "MUG-1", 29900, 10)

	var o orderResponse
	resp := api.do("POST", "/api/v1/orders", orderBody([2]int64{kb, 1}, [2]int64{mug, 2}), alice, &o)
	expectStatus(t, resp, http.StatusCreated)
	if o.Status != "RESERVED" || o.TotalAmount != 249900+2*29900 || len(o.Items) != 2 || o.Items[1].TotalPrice != 59800 {
		t.Fatalf("created order = %+v", o)
	}
	path := "/api/v1/orders/" + strconv.FormatInt(o.ID, 10)
	if resp.Header.Get("Location") != path {
		t.Errorf("Location = %q, want %q", resp.Header.Get("Location"), path)
	}

	var got orderResponse
	expectStatus(t, api.do("GET", path, nil, alice, &got), http.StatusOK)
	if got.ID != o.ID || len(got.Items) != 2 {
		t.Errorf("GET order = %+v", got)
	}

	var list orderListResponse
	expectStatus(t, api.do("GET", "/api/v1/orders", nil, alice, &list), http.StatusOK)
	if list.Total != 1 || list.Items[0].ID != o.ID || len(list.Items[0].Items) != 0 {
		t.Errorf("list = %+v (items are omitted in lists)", list)
	}

	var cancelled orderResponse
	expectStatus(t, api.do("POST", path+"/cancel", nil, alice, &cancelled), http.StatusOK)
	if cancelled.Status != "CANCELLED" {
		t.Errorf("status after cancel = %s", cancelled.Status)
	}

	var again errorResponse
	expectStatus(t, api.do("POST", path+"/cancel", nil, alice, &again), http.StatusConflict)
	if again.Error.Code != "INVALID_STATE_TRANSITION" {
		t.Errorf("code = %q, want INVALID_STATE_TRANSITION", again.Error.Code)
	}
}

func TestOrders_OwnershipAndAuth(t *testing.T) {
	api := newTestAPI(t)
	admin := api.loginAs("admin@example.com", true)
	alice := api.loginAs("alice@example.com", false)
	bob := api.loginAs("bob@example.com", false)
	p := api.createStockedProduct(admin, "P-1", 1000, 10)

	var o orderResponse
	expectStatus(t, api.do("POST", "/api/v1/orders", orderBody([2]int64{p, 1}), alice, &o), http.StatusCreated)
	path := "/api/v1/orders/" + strconv.FormatInt(o.ID, 10)

	expectStatus(t, api.do("POST", "/api/v1/orders", orderBody([2]int64{p, 1}), "", nil), http.StatusUnauthorized)
	expectStatus(t, api.do("GET", path, nil, bob, nil), http.StatusNotFound) // not 403: no information leak
	expectStatus(t, api.do("POST", path+"/cancel", nil, bob, nil), http.StatusNotFound)
	expectStatus(t, api.do("GET", path, nil, admin, nil), http.StatusOK)

	var bobList orderListResponse
	api.do("GET", "/api/v1/orders", nil, bob, &bobList)
	if bobList.Total != 0 {
		t.Errorf("bob sees %d orders, want 0", bobList.Total)
	}
	var adminList orderListResponse
	api.do("GET", "/api/v1/orders", nil, admin, &adminList)
	if adminList.Total != 1 {
		t.Errorf("admin sees %d orders, want 1", adminList.Total)
	}
}

func TestOrders_Errors(t *testing.T) {
	api := newTestAPI(t)
	admin := api.loginAs("admin@example.com", true)
	alice := api.loginAs("alice@example.com", false)
	active := api.createStockedProduct(admin, "ON-1", 1000, 10)
	inactive := api.createProduct(admin, "OFF-1", 1000)
	api.do("PATCH", "/api/v1/products/"+strconv.FormatInt(inactive, 10), map[string]any{"status": "INACTIVE"}, admin, nil)

	tests := []struct {
		name       string
		method     string
		path       string
		body       any
		wantStatus int
		wantCode   string
		wantField  string
	}{
		{"no items", "POST", "/api/v1/orders", map[string]any{"items": []any{}}, 400, "VALIDATION_ERROR", "items"},
		{"zero quantity", "POST", "/api/v1/orders", orderBody([2]int64{active, 0}), 400, "VALIDATION_ERROR", "items[0].quantity"},
		{"duplicate line", "POST", "/api/v1/orders", orderBody([2]int64{active, 1}, [2]int64{active, 1}), 400, "VALIDATION_ERROR", "items[1].product_id"},
		{"unknown product", "POST", "/api/v1/orders", orderBody([2]int64{999999, 1}), 400, "VALIDATION_ERROR", "items[0].product_id"},
		{"inactive product", "POST", "/api/v1/orders", orderBody([2]int64{inactive, 1}), 409, "PRODUCT_UNAVAILABLE", ""},
		{"client cannot set status", "POST", "/api/v1/orders", map[string]any{"items": []any{}, "status": "CONFIRMED"}, 400, "VALIDATION_ERROR", ""},
		{"client cannot set price", "POST", "/api/v1/orders", map[string]any{"items": []any{map[string]any{"product_id": active, "quantity": 1, "unit_price": 1}}}, 400, "VALIDATION_ERROR", ""},
		{"missing order", "GET", "/api/v1/orders/999999", nil, 404, "NOT_FOUND", ""},
		{"bad status filter", "GET", "/api/v1/orders?status=LOST", nil, 400, "VALIDATION_ERROR", "status"},
		{"bad id", "POST", "/api/v1/orders/abc/cancel", nil, 400, "VALIDATION_ERROR", "id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body errorResponse
			expectStatus(t, api.do(tt.method, tt.path, tt.body, alice, &body), tt.wantStatus)
			if body.Error.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", body.Error.Code, tt.wantCode)
			}
			if tt.wantField != "" && body.Error.Fields[tt.wantField] == "" {
				t.Errorf("fields = %v, want a problem for %q", body.Error.Fields, tt.wantField)
			}
		})
	}
}

func TestOrders_ReserveAndRelease(t *testing.T) {
	api := newTestAPI(t)
	admin := api.loginAs("admin@example.com", true)
	alice := api.loginAs("alice@example.com", false)
	bob := api.loginAs("bob@example.com", false)
	p := api.createStockedProduct(admin, "LAST-1", 1000, 2)

	// Alice takes both units: they move from available to reserved.
	var o orderResponse
	expectStatus(t, api.do("POST", "/api/v1/orders", orderBody([2]int64{p, 2}), alice, &o), http.StatusCreated)
	if inv := api.stockOf(admin, p); inv.AvailableQuantity != 0 || inv.ReservedQuantity != 2 {
		t.Fatalf("after order: %+v, want 0 available / 2 reserved", inv)
	}

	// Bob is out of luck.
	var oos errorResponse
	expectStatus(t, api.do("POST", "/api/v1/orders", orderBody([2]int64{p, 1}), bob, &oos), http.StatusConflict)
	if oos.Error.Code != "OUT_OF_STOCK" {
		t.Errorf("code = %q, want OUT_OF_STOCK", oos.Error.Code)
	}

	// Alice cancels: the units come back and Bob can buy one.
	expectStatus(t, api.do("POST", "/api/v1/orders/"+strconv.FormatInt(o.ID, 10)+"/cancel", nil, alice, nil), http.StatusOK)
	if inv := api.stockOf(admin, p); inv.AvailableQuantity != 2 || inv.ReservedQuantity != 0 {
		t.Fatalf("after cancel: %+v, want 2 available / 0 reserved", inv)
	}
	expectStatus(t, api.do("POST", "/api/v1/orders", orderBody([2]int64{p, 1}), bob, nil), http.StatusCreated)
}
