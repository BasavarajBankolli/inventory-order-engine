// Package events implements the TRANSACTIONAL OUTBOX: how this service
// tells the outside world (emails, analytics, warehouse, other services)
// that something happened - reliably.
//
// The problem it solves ("dual write"):
//
//	COMMIT order                 ✓
//	kafka.Publish(OrderCreated)  ✗ crash / network error   -> event lost forever
//
//	kafka.Publish(OrderCreated)  ✓
//	COMMIT order                 ✗ rollback                -> event about an order that does not exist
//
// There is no way to commit to PostgreSQL and a message broker atomically.
// The outbox avoids the second system: the event is just another row,
// INSERTed in the SAME PostgreSQL transaction as the business change. A
// background job (Processor) then publishes committed rows and marks them
// done. Result: events are never lost and never invented - at the price of
// being delivered AT LEAST once (see Processor).
package events

import (
	"encoding/json"
	"time"
)

// Event types published by this service. Consumers switch on these names,
// so treat them as a public API: add new ones, never rename old ones.
const (
	OrderCreated      = "OrderCreated"
	InventoryReserved = "InventoryReserved"
	OrderConfirmed    = "OrderConfirmed"
	OrderCancelled    = "OrderCancelled"
	OrderExpired      = "OrderExpired"
)

// AggregateOrder is the aggregate type of every order-related event. The
// aggregate (type + id) is what per-entity ordering is based on.
const AggregateOrder = "order"

// Status of an outbox row.
type Status string

const (
	StatusPending   Status = "PENDING"
	StatusProcessed Status = "PROCESSED"
	StatusFailed    Status = "FAILED"
)

// Event is one outbox row, and also the message that gets published.
type Event struct {
	ID            int64           `json:"id"` // consumers use it to ignore duplicates
	Type          string          `json:"type"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   int64           `json:"aggregate_id"`
	Payload       json.RawMessage `json:"payload"`
	CreatedAt     time.Time       `json:"created_at"`

	// Delivery bookkeeping (not part of the published message).
	Status    Status `json:"-"`
	Attempts  int    `json:"-"`
	LastError string `json:"-"`
}
