package tests

import (
	"net/http"
	"strconv"
	"testing"
)

type productResponse struct {
	ID          int64  `json:"id"`
	SKU         string `json:"sku"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Price       int64  `json:"price"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
}

type productListResponse struct {
	Items  []productResponse `json:"items"`
	Total  int64             `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

func TestProducts_Permissions(t *testing.T) {
	api := newTestAPI(t)
	customer := api.loginAs("customer@example.com", false)
	admin := api.loginAs("admin@example.com", true)

	body := map[string]any{"sku": "KB-01", "name": "Keyboard", "price": 249900, "currency": "INR"}

	// Anonymous: 401 on writes.
	resp := api.do("POST", "/api/v1/products", body, "", nil)
	expectStatus(t, resp, http.StatusUnauthorized)

	// Customer: authenticated but not allowed -> 403.
	var forbidden errorResponse
	resp = api.do("POST", "/api/v1/products", body, customer, &forbidden)
	expectStatus(t, resp, http.StatusForbidden)
	if forbidden.Error.Code != "FORBIDDEN" {
		t.Errorf("code = %q, want FORBIDDEN", forbidden.Error.Code)
	}

	// Admin: allowed.
	var created productResponse
	resp = api.do("POST", "/api/v1/products", body, admin, &created)
	expectStatus(t, resp, http.StatusCreated)
	if resp.Header.Get("Location") != "/api/v1/products/"+strconv.FormatInt(created.ID, 10) {
		t.Errorf("Location = %q", resp.Header.Get("Location"))
	}

	path := "/api/v1/products/" + strconv.FormatInt(created.ID, 10)
	expectStatus(t, api.do("PATCH", path, map[string]any{"price": 1}, customer, nil), http.StatusForbidden)
	expectStatus(t, api.do("DELETE", path, nil, customer, nil), http.StatusForbidden)

	// Reads are public.
	expectStatus(t, api.do("GET", path, nil, "", nil), http.StatusOK)
	expectStatus(t, api.do("GET", "/api/v1/products", nil, "", nil), http.StatusOK)
}

func TestProducts_Lifecycle(t *testing.T) {
	api := newTestAPI(t)
	admin := api.loginAs("admin@example.com", true)

	// Create (input is normalised: sku and currency upper-cased).
	var p productResponse
	resp := api.do("POST", "/api/v1/products", map[string]any{
		"sku": "mouse-01", "name": " Wireless Mouse ", "description": "2.4GHz",
		"price": 129900, "currency": "inr",
	}, admin, &p)
	expectStatus(t, resp, http.StatusCreated)
	if p.SKU != "MOUSE-01" || p.Name != "Wireless Mouse" || p.Currency != "INR" || p.Status != "ACTIVE" {
		t.Fatalf("created = %+v", p)
	}
	path := "/api/v1/products/" + strconv.FormatInt(p.ID, 10)

	// Partial update: only price and status change.
	var updated productResponse
	resp = api.do("PATCH", path, map[string]any{"price": 99900, "status": "INACTIVE"}, admin, &updated)
	expectStatus(t, resp, http.StatusOK)
	if updated.Price != 99900 || updated.Status != "INACTIVE" || updated.Name != "Wireless Mouse" || updated.Description != "2.4GHz" {
		t.Errorf("updated = %+v", updated)
	}

	// Inactive products are hidden from the default list but still readable.
	var list productListResponse
	api.do("GET", "/api/v1/products", nil, "", &list)
	if list.Total != 0 {
		t.Errorf("default list total = %d, want 0 (product is inactive)", list.Total)
	}
	api.do("GET", "/api/v1/products?status=INACTIVE", nil, "", &list)
	if list.Total != 1 {
		t.Errorf("inactive list total = %d, want 1", list.Total)
	}
	expectStatus(t, api.do("GET", path, nil, "", nil), http.StatusOK)

	// Delete = archive: 204, then it is gone for every endpoint.
	expectStatus(t, api.do("DELETE", path, nil, admin, nil), http.StatusNoContent)
	expectStatus(t, api.do("GET", path, nil, "", nil), http.StatusNotFound)
	expectStatus(t, api.do("PATCH", path, map[string]any{"price": 1}, admin, nil), http.StatusNotFound)
	expectStatus(t, api.do("DELETE", path, nil, admin, nil), http.StatusNotFound)
}

func TestProducts_Errors(t *testing.T) {
	api := newTestAPI(t)
	admin := api.loginAs("admin@example.com", true)
	api.do("POST", "/api/v1/products", map[string]any{"sku": "DUP-01", "name": "A", "price": 100, "currency": "INR"}, admin, nil)

	tests := []struct {
		name       string
		method     string
		path       string
		body       any
		wantStatus int
		wantCode   string
		wantField  string
	}{
		{"duplicate sku", "POST", "/api/v1/products", map[string]any{"sku": "dup-01", "name": "B", "price": 100, "currency": "INR"}, 409, "SKU_ALREADY_EXISTS", ""},
		{"float price rejected", "POST", "/api/v1/products", map[string]any{"sku": "F-01", "name": "F", "price": 12.5, "currency": "INR"}, 400, "VALIDATION_ERROR", ""},
		{"negative price", "POST", "/api/v1/products", map[string]any{"sku": "N-01", "name": "N", "price": -1, "currency": "INR"}, 400, "VALIDATION_ERROR", "price"},
		{"missing fields", "POST", "/api/v1/products", map[string]any{}, 400, "VALIDATION_ERROR", "sku"},
		{"cannot change sku", "PATCH", "/api/v1/products/1", map[string]any{"sku": "NEW"}, 400, "VALIDATION_ERROR", ""},
		{"empty patch", "PATCH", "/api/v1/products/1", map[string]any{}, 400, "VALIDATION_ERROR", "body"},
		{"archive via patch", "PATCH", "/api/v1/products/1", map[string]any{"status": "ARCHIVED"}, 400, "VALIDATION_ERROR", "status"},
		{"non-numeric id", "GET", "/api/v1/products/abc", nil, 400, "VALIDATION_ERROR", "id"},
		{"missing product", "GET", "/api/v1/products/999999", nil, 404, "NOT_FOUND", ""},
		{"bad limit", "GET", "/api/v1/products?limit=ten", nil, 400, "VALIDATION_ERROR", "limit"},
		{"limit too large", "GET", "/api/v1/products?limit=1000", nil, 400, "VALIDATION_ERROR", "limit"},
		{"unknown sort", "GET", "/api/v1/products?sort=random", nil, 400, "VALIDATION_ERROR", "sort"},
		// %3B is an encoded ';'. (A raw ';' would make Go's URL parser drop
		// the whole parameter before it reaches our code.)
		{"sql in sort", "GET", "/api/v1/products?sort=id%3B%20DROP%20TABLE%20products", nil, 400, "VALIDATION_ERROR", "sort"},
		{"archived status filter", "GET", "/api/v1/products?status=ARCHIVED", nil, 400, "VALIDATION_ERROR", "status"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body errorResponse
			resp := api.do(tt.method, tt.path, tt.body, admin, &body)
			expectStatus(t, resp, tt.wantStatus)
			if body.Error.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", body.Error.Code, tt.wantCode)
			}
			if tt.wantField != "" && body.Error.Fields[tt.wantField] == "" {
				t.Errorf("fields = %v, want a problem for %q", body.Error.Fields, tt.wantField)
			}
		})
	}
}

func TestProducts_ListPaginationAndSearch(t *testing.T) {
	api := newTestAPI(t)
	admin := api.loginAs("admin@example.com", true)

	for i := 1; i <= 25; i++ {
		sku := "ITEM-" + strconv.Itoa(i)
		resp := api.do("POST", "/api/v1/products", map[string]any{"sku": sku, "name": "Item " + strconv.Itoa(i), "price": i * 100, "currency": "INR"}, admin, nil)
		expectStatus(t, resp, http.StatusCreated)
	}

	var page productListResponse
	api.do("GET", "/api/v1/products", nil, "", &page)
	if len(page.Items) != 20 || page.Total != 25 || page.Limit != 20 || page.Offset != 0 {
		t.Errorf("default page: %d items, total %d, limit %d, offset %d", len(page.Items), page.Total, page.Limit, page.Offset)
	}

	api.do("GET", "/api/v1/products?limit=10&offset=20&sort=price_asc", nil, "", &page)
	if len(page.Items) != 5 || page.Items[0].Price != 2100 {
		t.Errorf("last page = %d items, first price %d; want 5 items starting at 2100", len(page.Items), page.Items[0].Price)
	}

	api.do("GET", "/api/v1/products?q=item%202", nil, "", &page) // "Item 2", "Item 20".."Item 25"
	if page.Total != 7 {
		t.Errorf("search 'item 2' total = %d, want 7", page.Total)
	}
}
