package orders

import (
	"errors"
	"testing"

	"inventory-order-engine/internal/products"
	"inventory-order-engine/internal/validate"
)

// Unit tests for request validation and total calculation (no database).

func catalogue() map[int64]products.Product {
	return map[int64]products.Product{
		1: {ID: 1, Price: 129900, Currency: "INR", Status: products.StatusActive},
		2: {ID: 2, Price: 29900, Currency: "INR", Status: products.StatusActive},
		3: {ID: 3, Price: 500, Currency: "INR", Status: products.StatusInactive},
		4: {ID: 4, Price: 500, Currency: "INR", Status: products.StatusArchived},
		5: {ID: 5, Price: 1999, Currency: "USD", Status: products.StatusActive},
	}
}

func TestBuildItems_TotalsAndSnapshot(t *testing.T) {
	items, total, currency, err := buildItems([]ItemRequest{
		{ProductID: 1, Quantity: 2},
		{ProductID: 2, Quantity: 3},
	}, catalogue())
	if err != nil {
		t.Fatalf("buildItems() error = %v", err)
	}

	// 2 x 129900 + 3 x 29900 = 259800 + 89700 = 349500
	if total != 349500 || currency != "INR" {
		t.Errorf("total = %d %s, want 349500 INR", total, currency)
	}
	want := []Item{
		{ProductID: 1, Quantity: 2, UnitPrice: 129900, TotalPrice: 259800},
		{ProductID: 2, Quantity: 3, UnitPrice: 29900, TotalPrice: 89700},
	}
	for i := range want {
		if items[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, items[i], want[i])
		}
	}
}

func TestBuildItems_LargestPossibleOrderDoesNotOverflow(t *testing.T) {
	byID := map[int64]products.Product{}
	var req []ItemRequest
	for id := int64(1); id <= maxItemsPerOrder; id++ {
		byID[id] = products.Product{ID: id, Price: 1_000_000_000, Currency: "INR", Status: products.StatusActive}
		req = append(req, ItemRequest{ProductID: id, Quantity: maxQuantity})
	}

	_, total, _, err := buildItems(req, byID)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(50 * 1000 * 1_000_000_000); total != want || total <= 0 {
		t.Errorf("total = %d, want %d", total, want)
	}
}

func TestBuildItems_Errors(t *testing.T) {
	tests := []struct {
		name      string
		req       []ItemRequest
		wantErr   error  // for sentinel errors
		wantField string // for validation errors
	}{
		{"missing product", []ItemRequest{{ProductID: 99, Quantity: 1}}, nil, "items[0].product_id"},
		{"archived counts as missing", []ItemRequest{{ProductID: 1, Quantity: 1}, {ProductID: 4, Quantity: 1}}, nil, "items[1].product_id"},
		{"inactive", []ItemRequest{{ProductID: 3, Quantity: 1}}, ErrProductUnavailable, ""},
		{"mixed currency", []ItemRequest{{ProductID: 1, Quantity: 1}, {ProductID: 5, Quantity: 1}}, nil, "items"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := buildItems(tt.req, catalogue())
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			var verr validate.Errors
			if !errors.As(err, &verr) || verr[tt.wantField] == "" {
				t.Errorf("error = %v, want a validation problem for %q", err, tt.wantField)
			}
		})
	}
}

func TestValidateItems(t *testing.T) {
	tooMany := make([]ItemRequest, maxItemsPerOrder+1)
	for i := range tooMany {
		tooMany[i] = ItemRequest{ProductID: int64(i + 1), Quantity: 1}
	}

	tests := []struct {
		name      string
		items     []ItemRequest
		wantField string // "" = valid
	}{
		{"valid", []ItemRequest{{ProductID: 1, Quantity: 1}, {ProductID: 2, Quantity: 1000}}, ""},
		{"empty", nil, "items"},
		{"too many lines", tooMany, "items"},
		{"zero quantity", []ItemRequest{{ProductID: 1, Quantity: 0}}, "items[0].quantity"},
		{"huge quantity", []ItemRequest{{ProductID: 1, Quantity: 1001}}, "items[0].quantity"},
		{"bad product id", []ItemRequest{{ProductID: -1, Quantity: 1}}, "items[0].product_id"},
		{"duplicate product", []ItemRequest{{ProductID: 7, Quantity: 1}, {ProductID: 7, Quantity: 2}}, "items[1].product_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateItems(tt.items)
			if tt.wantField == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			var verr validate.Errors
			if !errors.As(err, &verr) || verr[tt.wantField] == "" {
				t.Errorf("error = %v, want a problem for %q", err, tt.wantField)
			}
		})
	}
}
