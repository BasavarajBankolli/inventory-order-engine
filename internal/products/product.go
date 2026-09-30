// Package products manages the product catalogue: what can be sold, under
// which SKU and at what price. Stock levels live in the inventory package.
package products

import (
	"errors"
	"regexp"
	"time"
)

// Errors returned by the repository and service. Compare with errors.Is.
var (
	ErrNotFound = errors.New("product not found")
	ErrSKUTaken = errors.New("sku already exists")
)

// Status is the lifecycle state of a product.
type Status string

const (
	StatusActive   Status = "ACTIVE"   // visible and orderable
	StatusInactive Status = "INACTIVE" // temporarily not for sale
	StatusArchived Status = "ARCHIVED" // soft-deleted; hidden from the API
)

// Product is one catalogue entry.
//
// Unlike users.User it has JSON tags directly: nothing here is secret, so a
// separate response type would only add code without adding safety.
type Product struct {
	ID          int64     `json:"id"`
	SKU         string    `json:"sku"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Price       int64     `json:"price"` // minor units: 129900 INR = Rs 1,299.00
	Currency    string    `json:"currency"`
	Status      Status    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Limits shared by validation code (the database enforces the important
// ones again with CHECK constraints).
const (
	maxNameLen        = 200
	maxDescriptionLen = 2000
	maxPrice          = 1_000_000_000 // minor units; see migration 000003
	maxSearchLen      = 100
	defaultLimit      = 20
	maxLimit          = 100
)

var (
	skuPattern      = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{1,63}$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)
