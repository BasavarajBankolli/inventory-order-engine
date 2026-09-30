package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"inventory-order-engine/internal/database"
)

// Payment statuses (see migration 000008).
type Status string

const (
	StatusPending   Status = "PENDING"
	StatusSucceeded Status = "SUCCEEDED"
	StatusFailed    Status = "FAILED"
)

// ErrNotFound is returned when an order has no payment yet.
var ErrNotFound = errors.New("payment not found")

// Payment is one payment row.
type Payment struct {
	ID                int64     `json:"id"`
	OrderID           int64     `json:"order_id"`
	Amount            int64     `json:"amount"`
	Currency          string    `json:"currency"`
	Status            Status    `json:"status"`
	ProviderReference string    `json:"provider_reference,omitempty"`
	FailureReason     string    `json:"failure_reason,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// IdempotencyKey is what we send to the provider for this payment. It is
// derived from our own row id, so every retry of this payment uses the
// same key and the provider never charges twice.
func (p Payment) IdempotencyKey() string {
	return fmt.Sprintf("payment-%d", p.ID)
}

// Repository contains all SQL for the payments table.
type Repository struct {
	db database.DBTX
}

// NewRepository creates a payments Repository.
func NewRepository(db database.DBTX) *Repository {
	return &Repository{db: db}
}

// WithTx returns a copy of the repository whose queries run inside tx.
func (r *Repository) WithTx(tx pgx.Tx) *Repository {
	return &Repository{db: tx}
}

// COALESCE turns NULL columns into "" so they scan into plain strings.
const paymentColumns = `id, order_id, amount, currency, status,
	COALESCE(provider_reference, ''), COALESCE(failure_reason, ''), created_at, updated_at`

func scanPayment(row pgx.Row) (Payment, error) {
	var p Payment
	err := row.Scan(&p.ID, &p.OrderID, &p.Amount, &p.Currency, &p.Status,
		&p.ProviderReference, &p.FailureReason, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("scan payment: %w", err)
	}
	return p, nil
}

// Create inserts a PENDING payment for an order.
func (r *Repository) Create(ctx context.Context, orderID, amount int64, currency string) (Payment, error) {
	return scanPayment(r.db.QueryRow(ctx, `
		INSERT INTO payments (order_id, amount, currency, status)
		VALUES ($1, $2, $3, 'PENDING')
		RETURNING `+paymentColumns, orderID, amount, currency))
}

// GetByOrder returns the payment of an order, or ErrNotFound.
func (r *Repository) GetByOrder(ctx context.Context, orderID int64) (Payment, error) {
	return scanPayment(r.db.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments WHERE order_id = $1`, orderID))
}

// MarkSucceeded records the provider's confirmation. Only a PENDING payment
// can succeed; ErrNotFound means it was not PENDING any more.
func (r *Repository) MarkSucceeded(ctx context.Context, id int64, reference string) (Payment, error) {
	return scanPayment(r.db.QueryRow(ctx, `
		UPDATE payments SET status = 'SUCCEEDED', provider_reference = $2, updated_at = now()
		WHERE id = $1 AND status = 'PENDING'
		RETURNING `+paymentColumns, id, reference))
}

// MarkFailed records a decline. Only a PENDING payment can fail.
func (r *Repository) MarkFailed(ctx context.Context, id int64, reason string) (Payment, error) {
	return scanPayment(r.db.QueryRow(ctx, `
		UPDATE payments SET status = 'FAILED', failure_reason = $2, updated_at = now()
		WHERE id = $1 AND status = 'PENDING'
		RETURNING `+paymentColumns, id, reason))
}
