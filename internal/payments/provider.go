// Package payments talks to the payment provider and stores payment rows.
//
// The rest of the system only knows the Provider interface below, so the
// mock can be swapped for a real provider (Stripe, Razorpay, ...) by
// writing one new type - no order or inventory code changes.
package payments

import (
	"context"
	"errors"

	"inventory-order-engine/internal/money"
)

// Provider charges money. Implementations: MockProvider (this package).
//
// Deviation from the original spec (Charge(ctx, amount Money)): we pass a
// ChargeRequest that also carries an IdempotencyKey. Real providers accept
// such a key so that a RETRIED charge request (after a timeout) returns the
// original result instead of charging the customer a second time. Without
// it, "retry after timeout" could mean "charge twice".
type Provider interface {
	Charge(ctx context.Context, req ChargeRequest) (Result, error)
}

// ChargeRequest describes one charge.
type ChargeRequest struct {
	Amount money.Money
	// IdempotencyKey must be the same for every attempt of the same
	// payment. We use "payment-<payments.id>".
	IdempotencyKey string
}

// ResultStatus is the provider's DEFINITE answer.
type ResultStatus string

const (
	ResultSucceeded ResultStatus = "SUCCEEDED"
	ResultDeclined  ResultStatus = "DECLINED"
)

// Result is a definite answer from the provider.
//
// A DECLINE is a normal Result, not an error: the provider answered, and the
// answer was "no". An ERROR from Charge means we did NOT get an answer
// (timeout, network failure), so we do not know whether money was taken.
type Result struct {
	Status        ResultStatus
	Reference     string // provider's charge id, set when SUCCEEDED
	FailureReason string // set when DECLINED, e.g. "card_declined"
}

// ErrProviderUnavailable wraps errors where the outcome is unknown.
var ErrProviderUnavailable = errors.New("payment provider did not answer")
