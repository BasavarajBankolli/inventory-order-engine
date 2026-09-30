// Package orders handles placing, reading and cancelling orders.
//
// Stage 5 scope: orders and items are created with a price snapshot and a
// total, move through an explicit state machine, and can be cancelled.
// Orders do NOT touch stock yet; reserving inventory is added in Stage 6.
package orders

import (
	"errors"
	"fmt"
	"time"

	"inventory-order-engine/internal/products"
	"inventory-order-engine/internal/validate"
)

// Errors returned by this package. Compare with errors.Is.
var (
	ErrNotFound           = errors.New("order not found")
	ErrProductUnavailable = errors.New("product is not available for sale")
)

// Limits for one order. They also guarantee the total cannot overflow:
// 50 items x 1,000 units x 1,000,000,000 max price = 5 x 10^13,
// far below int64's limit of about 9.2 x 10^18.
const (
	maxItemsPerOrder = 50
	maxQuantity      = 1000
)

// Order is an order with (optionally) its line items.
type Order struct {
	ID          int64     `json:"id"`
	UserID      int64     `json:"user_id"`
	Status      Status    `json:"status"`
	TotalAmount int64     `json:"total_amount"` // minor units, like product prices
	Currency    string    `json:"currency"`
	Items       []Item    `json:"items,omitempty"` // empty in list responses
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Idempotency data (Stage 8). Unexported, so never part of the JSON.
	idempotencyKey string // "" = order was placed without a key
	requestHash    string // fingerprint(items) of the original request
}

// Item is one line of an order. UnitPrice is a snapshot of the product's
// price when the order was placed; later price changes do not affect it.
type Item struct {
	ProductID  int64 `json:"product_id"`
	Quantity   int   `json:"quantity"`
	UnitPrice  int64 `json:"unit_price"`
	TotalPrice int64 `json:"total_price"`
}

// ItemRequest is one line as requested by the customer.
type ItemRequest struct {
	ProductID int64
	Quantity  int
}

// validateItems checks the request shape before touching the database.
func validateItems(items []ItemRequest) error {
	v := validate.Errors{}
	if len(items) == 0 {
		v.Add("items", "must contain at least one item")
		return v.Err()
	}
	if len(items) > maxItemsPerOrder {
		v.Add("items", fmt.Sprintf("must contain at most %d items", maxItemsPerOrder))
		return v.Err()
	}

	seen := make(map[int64]bool, len(items))
	for i, it := range items {
		field := fmt.Sprintf("items[%d]", i)
		v.Check(it.ProductID > 0, field+".product_id", "must be a positive integer")
		v.Check(it.Quantity >= 1 && it.Quantity <= maxQuantity, field+".quantity",
			fmt.Sprintf("must be between 1 and %d", maxQuantity))
		if seen[it.ProductID] {
			v.Add(field+".product_id", "is listed more than once; combine the quantities into one line")
		}
		seen[it.ProductID] = true
	}
	return v.Err()
}

// buildItems turns the requested lines into priced order items and
// calculates the order total. It is a pure function (no database): the
// caller loads the products first and passes them in, keyed by id.
//
// Rules:
//   - every product must exist and not be archived   -> 400 validation error
//   - every product must be ACTIVE (not INACTIVE)    -> ErrProductUnavailable
//   - all products must share one currency          -> 400 validation error
//   - line total = unit price x quantity; order total = sum of line totals
func buildItems(requested []ItemRequest, byID map[int64]products.Product) (items []Item, total int64, currency string, err error) {
	v := validate.Errors{}
	items = make([]Item, 0, len(requested))

	for i, req := range requested {
		p, ok := byID[req.ProductID]
		if !ok || p.Status == products.StatusArchived {
			v.Add(fmt.Sprintf("items[%d].product_id", i), fmt.Sprintf("product %d does not exist", req.ProductID))
			continue
		}
		if p.Status != products.StatusActive {
			return nil, 0, "", fmt.Errorf("%w: product %d", ErrProductUnavailable, p.ID)
		}

		if currency == "" {
			currency = p.Currency
		} else if p.Currency != currency {
			v.Add("items", "all products in one order must use the same currency")
		}

		line := Item{
			ProductID:  p.ID,
			Quantity:   req.Quantity,
			UnitPrice:  p.Price,
			TotalPrice: p.Price * int64(req.Quantity),
		}
		items = append(items, line)
		total += line.TotalPrice
	}

	if err := v.Err(); err != nil {
		return nil, 0, "", err
	}
	return items, total, currency, nil
}
