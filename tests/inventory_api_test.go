package tests

import (
	"net/http"
	"strconv"
	"testing"
)

type inventoryResponse struct {
	ProductID         int64 `json:"product_id"`
	AvailableQuantity int   `json:"available_quantity"`
	ReservedQuantity  int   `json:"reserved_quantity"`
	Version           int64 `json:"version"`
}

// createProduct creates a product through the API and returns its id.
func (a *testAPI) createProduct(adminToken, sku string, price int64) int64 {
	a.t.Helper()
	var p productResponse
	resp := a.do("POST", "/api/v1/products",
		map[string]any{"sku": sku, "name": "Product " + sku, "price": price, "currency": "INR"}, adminToken, &p)
	expectStatus(a.t, resp, http.StatusCreated)
	return p.ID
}

func TestInventory_CreatedWithProductAndAdjustable(t *testing.T) {
	api := newTestAPI(t)
	admin := api.loginAs("admin@example.com", true)
	id := api.createProduct(admin, "INV-01", 1000)
	path := "/api/v1/products/" + strconv.FormatInt(id, 10) + "/inventory"

	// A new product starts with an empty inventory row (same transaction).
	var inv inventoryResponse
	expectStatus(t, api.do("GET", path, nil, admin, &inv), http.StatusOK)
	if inv.ProductID != id || inv.AvailableQuantity != 0 || inv.ReservedQuantity != 0 || inv.Version != 1 {
		t.Fatalf("initial inventory = %+v", inv)
	}

	// Relative restock.
	expectStatus(t, api.do("PATCH", path, map[string]any{"adjustment": 25}, admin, &inv), http.StatusOK)
	if inv.AvailableQuantity != 25 || inv.Version != 2 {
		t.Errorf("after +25: %+v", inv)
	}

	// Absolute set with the version we just read.
	expectStatus(t, api.do("PATCH", path, map[string]any{"available_quantity": 18, "version": 2}, admin, &inv), http.StatusOK)
	if inv.AvailableQuantity != 18 || inv.Version != 3 {
		t.Errorf("after set 18: %+v", inv)
	}

	// Same version again is now stale -> 409.
	var conflict errorResponse
	expectStatus(t, api.do("PATCH", path, map[string]any{"available_quantity": 5, "version": 2}, admin, &conflict), http.StatusConflict)
	if conflict.Error.Code != "VERSION_CONFLICT" {
		t.Errorf("code = %q, want VERSION_CONFLICT", conflict.Error.Code)
	}

	// Removing more than available -> 409.
	var insufficient errorResponse
	expectStatus(t, api.do("PATCH", path, map[string]any{"adjustment": -19}, admin, &insufficient), http.StatusConflict)
	if insufficient.Error.Code != "INSUFFICIENT_STOCK" {
		t.Errorf("code = %q, want INSUFFICIENT_STOCK", insufficient.Error.Code)
	}
}

func TestInventory_PermissionsAndErrors(t *testing.T) {
	api := newTestAPI(t)
	admin := api.loginAs("admin@example.com", true)
	customer := api.loginAs("customer@example.com", false)
	id := api.createProduct(admin, "INV-02", 1000)
	path := "/api/v1/products/" + strconv.FormatInt(id, 10) + "/inventory"

	expectStatus(t, api.do("GET", path, nil, "", nil), http.StatusUnauthorized)
	expectStatus(t, api.do("GET", path, nil, customer, nil), http.StatusForbidden)
	expectStatus(t, api.do("PATCH", path, map[string]any{"adjustment": 1}, customer, nil), http.StatusForbidden)

	tests := []struct {
		name       string
		path       string
		body       any
		wantStatus int
		wantField  string
	}{
		{"empty body", path, map[string]any{}, 400, "body"},
		{"both fields", path, map[string]any{"adjustment": 1, "available_quantity": 1, "version": 1}, 400, "body"},
		{"zero adjustment", path, map[string]any{"adjustment": 0}, 400, "adjustment"},
		{"absolute without version", path, map[string]any{"available_quantity": 10}, 400, "version"},
		{"negative absolute", path, map[string]any{"available_quantity": -3, "version": 1}, 400, "available_quantity"},
		{"reserved is not editable", path, map[string]any{"reserved_quantity": 5}, 400, ""},
		{"missing product", "/api/v1/products/999999/inventory", map[string]any{"adjustment": 1}, 404, ""},
		{"bad id", "/api/v1/products/abc/inventory", map[string]any{"adjustment": 1}, 400, "id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body errorResponse
			expectStatus(t, api.do("PATCH", tt.path, tt.body, admin, &body), tt.wantStatus)
			if tt.wantField != "" && body.Error.Fields[tt.wantField] == "" {
				t.Errorf("fields = %v, want a problem for %q", body.Error.Fields, tt.wantField)
			}
		})
	}

	// Archived product: inventory is gone too.
	expectStatus(t, api.do("DELETE", "/api/v1/products/"+strconv.FormatInt(id, 10), nil, admin, nil), http.StatusNoContent)
	expectStatus(t, api.do("GET", path, nil, admin, nil), http.StatusNotFound)
}
