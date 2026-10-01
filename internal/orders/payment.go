package orders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/events"
	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/money"
	"inventory-order-engine/internal/payments"
)

var (
	// ErrPaymentDeclined: the provider said no. The order has been
	// cancelled and its stock released.
	ErrPaymentDeclined = errors.New("payment was declined")

	// ErrPaymentOutcomeUnknown: the provider did not answer in time. The
	// money MAY have been taken, so nothing is changed: the order stays
	// PAYMENT_PENDING and the client should retry. The retry reuses the same
	// provider idempotency key, so it can never charge twice.
	ErrPaymentOutcomeUnknown = errors.New("payment outcome unknown")

	// ErrReservationExpired: the order's stock hold ran out before payment.
	ErrReservationExpired = errors.New("reservation has expired")

	// errRefundRequired: money was taken for an order that can no longer be
	// confirmed. Should be unreachable; logged loudly if it ever happens.
	errRefundRequired = errors.New("payment succeeded for an order that is no longer payable; refund required")
)

// PayResult is the outcome of Pay.
type PayResult struct {
	Order   Order            `json:"order"`
	Payment payments.Payment `json:"payment"`
}

// Pay charges the customer for a RESERVED order.
//
//	STEP 1  transaction (short)
//	          lock order; RESERVED -> PAYMENT_PENDING; INSERT payment PENDING
//	        COMMIT
//	STEP 2  call the provider - NO transaction, NO locks held
//	STEP 3  transaction (short), depending on the provider's answer
//	          SUCCEEDED: payment SUCCEEDED, order CONFIRMED,
//	                     reservations CONFIRMED (reserved -= qty: units sold)
//	          DECLINED:  payment FAILED, order PAYMENT_FAILED -> CANCELLED,
//	                     reservations RELEASED (stock back to available)
//	          no answer: nothing changes (ErrPaymentOutcomeUnknown)
//	        COMMIT
//
// WHY NOT ONE BIG TRANSACTION? The provider call can take seconds. Holding
// the order and inventory row locks during it would make every other buyer
// of those products wait for a remote service. And a database transaction
// cannot "roll back" a real card charge anyway.
//
// Pay is safe to call again: for a PAYMENT_PENDING order it retries the
// same payment (same provider key), and for a CONFIRMED order it simply
// returns the existing result.
func (s *Service) Pay(ctx context.Context, caller identity.Principal, orderID int64) (PayResult, error) {
	// ---- STEP 1 ----------------------------------------------------------
	var (
		o           Order
		pay         payments.Payment
		alreadyPaid bool
	)
	err := database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		repo := s.orders.WithTx(tx)
		payRepo := s.payments.WithTx(tx)

		var err error
		if o, err = repo.GetForUpdate(ctx, orderID); err != nil {
			return err
		}
		if !canAccess(caller, o) {
			return ErrNotFound
		}

		switch o.Status {
		case StatusConfirmed: // paid before: return the stored result
			alreadyPaid = true
			pay, err = payRepo.GetByOrder(ctx, o.ID)
			return err

		case StatusPaymentPending: // retry after a timeout: reuse the payment
			// Once the hold has expired we no longer START charges; the
			// worker resolves this order with a read-only provider lookup.
			// This rule is what makes the worker's grace period safe.
			expired, err := s.inventory.HasExpiredReservations(ctx, tx, o.ID)
			if err != nil {
				return err
			}
			if expired {
				return ErrReservationExpired
			}
			pay, err = payRepo.GetByOrder(ctx, o.ID)
			return err

		case StatusReserved: // first attempt
			expired, err := s.inventory.HasExpiredReservations(ctx, tx, o.ID)
			if err != nil {
				return err
			}
			if expired {
				return ErrReservationExpired
			}
			if err := s.transition(ctx, repo, &o, StatusPaymentPending); err != nil {
				return err
			}
			pay, err = payRepo.Create(ctx, o.ID, o.TotalAmount, o.Currency)
			return err

		default:
			return fmt.Errorf("%w: an order in status %s cannot be paid", ErrInvalidTransition, o.Status)
		}
	})
	if err != nil {
		return PayResult{}, err
	}
	if alreadyPaid {
		return s.payResult(ctx, orderID, pay)
	}

	// ---- STEP 2 ----------------------------------------------------------
	chargeCtx, cancel := context.WithTimeout(ctx, s.paymentTimeout)
	res, err := s.provider.Charge(chargeCtx, payments.ChargeRequest{
		Amount:         money.New(pay.Amount, pay.Currency),
		IdempotencyKey: pay.IdempotencyKey(),
	})
	cancel()
	if err != nil {
		s.metrics.PaymentFailed("timeout")
		slog.WarnContext(ctx, "payment outcome unknown; order stays PAYMENT_PENDING",
			"order_id", orderID, "payment_id", pay.ID, "error", err)
		return PayResult{Order: o, Payment: pay}, fmt.Errorf("%w: %w", ErrPaymentOutcomeUnknown, err)
	}

	// ---- STEP 3 ----------------------------------------------------------
	return s.completePayment(ctx, orderID, pay.ID, res)
}

// completePayment records the provider's definite answer (step 3).
func (s *Service) completePayment(ctx context.Context, orderID, paymentID int64, res payments.Result) (PayResult, error) {
	var (
		pay            payments.Payment
		declined       bool
		refundRequired bool
		finished       int // reservations confirmed/released by THIS call (0 if a concurrent call did it)
	)
	err := database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		repo := s.orders.WithTx(tx)
		payRepo := s.payments.WithTx(tx)

		o, err := repo.GetForUpdate(ctx, orderID)
		if err != nil {
			return err
		}

		// A concurrent Pay call for the same order may have recorded the
		// (same, thanks to the provider key) answer already. Then there is
		// nothing left to do.
		if o.Status == StatusConfirmed || o.Status == StatusCancelled {
			pay, err = payRepo.GetByOrder(ctx, orderID)
			declined = pay.Status == payments.StatusFailed
			return err
		}

		if res.Status == payments.ResultSucceeded {
			if pay, err = payRepo.MarkSucceeded(ctx, paymentID, res.Reference); err != nil {
				return err
			}
			if o.Status != StatusPaymentPending {
				// Money taken but the order moved on (e.g. expired). Keep the
				// payment record (COMMIT), flag it, never lose track of money.
				refundRequired = true
				return nil
			}
			if err := s.transition(ctx, repo, &o, StatusConfirmed); err != nil {
				return err
			}
			if finished, err = s.inventory.ConfirmForOrder(ctx, tx, orderID); err != nil {
				return err
			}
			return s.emit(ctx, tx, events.OrderConfirmed, orderID, orderConfirmedPayload{
				OrderID: orderID, PaymentID: pay.ID, ProviderReference: pay.ProviderReference,
				Amount: pay.Amount, Currency: pay.Currency,
			})
		}

		// Declined.
		declined = true
		if pay, err = payRepo.MarkFailed(ctx, paymentID, res.FailureReason); err != nil {
			return err
		}
		if err := s.transition(ctx, repo, &o, StatusPaymentFailed); err != nil {
			return err
		}
		if err := s.transition(ctx, repo, &o, StatusCancelled); err != nil {
			return err
		}
		if finished, err = s.inventory.ReleaseForOrder(ctx, tx, orderID, inventory.ReservationReleased); err != nil {
			return err
		}
		return s.emit(ctx, tx, events.OrderCancelled, orderID,
			orderClosedPayload{OrderID: orderID, Reason: reasonPaymentDeclined})
	})
	if err != nil {
		return PayResult{}, err
	}

	if refundRequired {
		slog.ErrorContext(ctx, "REFUND REQUIRED: payment captured for an order that can no longer be confirmed",
			"order_id", orderID, "payment_id", paymentID, "provider_reference", res.Reference)
		return PayResult{}, errRefundRequired
	}

	result, err := s.payResult(ctx, orderID, pay)
	if err != nil {
		return PayResult{}, err
	}
	// Metrics after COMMIT, and only if THIS call recorded the outcome
	// (finished > 0): a concurrent duplicate Pay must not count it twice.
	if finished > 0 {
		if declined {
			s.metrics.PaymentFailed("declined")
			s.metrics.ReservationsFinished("released", finished)
		} else {
			s.metrics.PaymentSucceeded()
			s.metrics.ReservationsFinished("confirmed", finished)
		}
	}

	if declined {
		slog.InfoContext(ctx, "payment declined; order cancelled and stock released",
			"order_id", orderID, "payment_id", paymentID, "reason", pay.FailureReason)
		return result, fmt.Errorf("%w (%s)", ErrPaymentDeclined, pay.FailureReason)
	}
	slog.InfoContext(ctx, "payment succeeded; order confirmed",
		"order_id", orderID, "payment_id", paymentID, "provider_reference", pay.ProviderReference)
	return result, nil
}

// payResult loads the order (with items) for the response.
func (s *Service) payResult(ctx context.Context, orderID int64, pay payments.Payment) (PayResult, error) {
	o, err := s.orders.GetByID(ctx, orderID)
	if err != nil {
		return PayResult{}, err
	}
	return PayResult{Order: o, Payment: pay}, nil
}
