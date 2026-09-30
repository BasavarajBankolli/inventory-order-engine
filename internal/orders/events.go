package orders

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"inventory-order-engine/internal/events"
)

// Event payloads published by the orders module. These JSON shapes are a
// PUBLIC contract with every consumer: add fields freely, but never remove
// or rename one.

type orderCreatedPayload struct {
	OrderID     int64  `json:"order_id"`
	UserID      int64  `json:"user_id"`
	TotalAmount int64  `json:"total_amount"`
	Currency    string `json:"currency"`
	Items       []Item `json:"items"`
}

type inventoryReservedPayload struct {
	OrderID   int64          `json:"order_id"`
	ExpiresAt time.Time      `json:"expires_at"`
	Items     []reservedItem `json:"items"`
}

type reservedItem struct {
	ProductID int64 `json:"product_id"`
	Quantity  int   `json:"quantity"`
}

type orderConfirmedPayload struct {
	OrderID           int64  `json:"order_id"`
	PaymentID         int64  `json:"payment_id"`
	ProviderReference string `json:"provider_reference"`
	Amount            int64  `json:"amount"`
	Currency          string `json:"currency"`
}

// orderClosedPayload is used for OrderCancelled and OrderExpired.
type orderClosedPayload struct {
	OrderID int64  `json:"order_id"`
	Reason  string `json:"reason"`
}

// Reasons in OrderCancelled / OrderExpired events.
const (
	reasonCustomerCancelled  = "customer_cancelled"
	reasonPaymentDeclined    = "payment_declined"
	reasonReservationExpired = "reservation_expired"
	reasonNotChargedInTime   = "not_charged_before_expiry"
)

// emit records an order event in the outbox INSIDE tx. If the surrounding
// transaction rolls back, the event disappears with it: no event is ever
// published about a change that did not happen.
func (s *Service) emit(ctx context.Context, tx pgx.Tx, eventType string, orderID int64, payload any) error {
	return s.outbox.WithTx(tx).Add(ctx, eventType, events.AggregateOrder, orderID, payload)
}
