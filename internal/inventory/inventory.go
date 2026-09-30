// Package inventory tracks how many units of each product can be sold.
//
// The rules that keep stock correct are methods on Inventory (Adjust,
// SetAvailable). They are plain Go with no database, so they are easy to
// read and unit-test. The service loads a LOCKED row, applies one of these
// methods, and saves the result inside a transaction.
package inventory

import (
	"errors"
	"time"
)

// Errors returned by this package. Compare with errors.Is.
var (
	ErrNotFound          = errors.New("inventory not found")
	ErrInsufficientStock = errors.New("not enough stock available")
	ErrQuantityTooLarge  = errors.New("quantity exceeds the maximum allowed")
	ErrNegativeQuantity  = errors.New("quantity must not be negative")
	ErrVersionConflict   = errors.New("inventory was changed by someone else")
)

// MaxQuantity matches the CHECK constraint in migration 000004.
const MaxQuantity = 1_000_000_000

// Inventory is the stock level of one product.
type Inventory struct {
	ProductID         int64     `json:"product_id"`
	AvailableQuantity int       `json:"available_quantity"`
	ReservedQuantity  int       `json:"reserved_quantity"`
	Version           int64     `json:"version"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Adjust changes available stock by delta: positive for a restock,
// negative for damaged or lost goods. Stock can never become negative.
func (inv *Inventory) Adjust(delta int) error {
	newQty := inv.AvailableQuantity + delta
	if newQty < 0 {
		return ErrInsufficientStock
	}
	if newQty > MaxQuantity {
		return ErrQuantityTooLarge
	}
	inv.AvailableQuantity = newQty
	return nil
}

// SetAvailable overwrites available stock with an absolute value, e.g. the
// result of a physical stock count in the warehouse.
func (inv *Inventory) SetAvailable(qty int) error {
	if qty < 0 {
		return ErrNegativeQuantity
	}
	if qty > MaxQuantity {
		return ErrQuantityTooLarge
	}
	inv.AvailableQuantity = qty
	return nil
}
