package orders

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/payments"
)

// ExpiryReport counts what one ExpireOverdue run did.
type ExpiryReport struct {
	Expired   int // stock released, order EXPIRED
	Confirmed int // unknown payment turned out to be paid: order CONFIRMED
	Cancelled int // unknown payment turned out to be declined: order CANCELLED
	Skipped   int // nothing to do any more (paid/cancelled meanwhile) or provider unreachable
	Failed    int // unexpected errors (logged); retried on the next run
}

// Total returns how many orders were changed.
func (r ExpiryReport) Total() int { return r.Expired + r.Confirmed + r.Cancelled }

// ExpireOverdue is the background job behind reservation expiry. It is
// called by the worker every few seconds and handles at most batchSize
// orders of each kind per run.
//
//  1. RESERVED orders whose reservation has expired:
//     order -> EXPIRED, reserved stock -> available, reservations -> EXPIRED.
//
//  2. PAYMENT_PENDING orders whose reservation expired more than
//     reconcileAfter ago (a payment timed out and nobody retried). We must
//     not simply expire them: the customer may have been charged. So we ASK
//     THE PROVIDER (reconciliation) and act on the truth.
//
// Every order is handled in its own short transaction that locks the order
// and re-checks its state, so this job is idempotent: running it twice, in
// two worker processes at once, or racing a customer's cancel/pay can never
// release stock twice or expire a paid order.
func (s *Service) ExpireOverdue(ctx context.Context, batchSize int) (ExpiryReport, error) {
	var rep ExpiryReport

	reserved, err := s.orders.OverdueReservedIDs(ctx, batchSize)
	if err != nil {
		return rep, err
	}
	for _, id := range reserved {
		changed, err := s.expireReserved(ctx, id)
		switch {
		case err != nil:
			rep.Failed++
			slog.ErrorContext(ctx, "expire order failed", "order_id", id, "error", err)
		case changed:
			rep.Expired++
		default:
			rep.Skipped++
		}
	}

	pending, err := s.orders.OverduePaymentPendingIDs(ctx, s.reconcileAfter, batchSize)
	if err != nil {
		return rep, err
	}
	for _, id := range pending {
		s.reconcile(ctx, id, &rep)
	}
	return rep, nil
}

// expireReserved expires one RESERVED order if (still) overdue.
func (s *Service) expireReserved(ctx context.Context, orderID int64) (changed bool, err error) {
	err = database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		repo := s.orders.WithTx(tx)
		o, err := repo.GetForUpdate(ctx, orderID)
		if err != nil {
			return err
		}
		// Re-check under the lock: the customer may have paid or cancelled
		// between our SELECT and now. Then there is nothing to do.
		if o.Status != StatusReserved {
			return nil
		}
		expired, err := s.inventory.HasExpiredReservations(ctx, tx, orderID)
		if err != nil || !expired {
			return err
		}

		if err := s.transition(ctx, repo, &o, StatusExpired); err != nil {
			return err
		}
		released, err := s.inventory.ReleaseForOrder(ctx, tx, orderID, inventory.ReservationExpired)
		if err != nil {
			return err
		}
		changed = true
		slog.InfoContext(ctx, "reservation expired; order expired and stock released",
			"order_id", orderID, "reservations_released", released)
		return nil
	})
	return changed, err
}

// reconcile resolves one PAYMENT_PENDING order by asking the provider what
// really happened to its charge.
func (s *Service) reconcile(ctx context.Context, orderID int64, rep *ExpiryReport) {
	pay, err := s.payments.GetByOrder(ctx, orderID)
	if err != nil {
		rep.Failed++
		slog.ErrorContext(ctx, "reconcile: load payment failed", "order_id", orderID, "error", err)
		return
	}
	if pay.Status != payments.StatusPending {
		rep.Skipped++ // resolved meanwhile (e.g. the customer retried /pay)
		return
	}

	lookupCtx, cancel := context.WithTimeout(ctx, s.paymentTimeout)
	res, err := s.provider.Status(lookupCtx, pay.IdempotencyKey())
	cancel()

	switch {
	case errors.Is(err, payments.ErrChargeNotFound):
		// The provider never received the charge: no money was taken, so
		// it is safe to give the stock back.
		changed, err := s.expireUncharged(ctx, orderID, pay.ID)
		switch {
		case err != nil:
			rep.Failed++
			slog.ErrorContext(ctx, "reconcile: expire failed", "order_id", orderID, "error", err)
		case changed:
			rep.Expired++
		default:
			rep.Skipped++
		}

	case err != nil:
		// Still no answer. Do nothing and try again on the next run.
		rep.Skipped++
		slog.WarnContext(ctx, "reconcile: provider unavailable, will retry", "order_id", orderID, "error", err)

	default:
		// A definite answer: record it exactly as /pay would have.
		slog.InfoContext(ctx, "reconcile: provider knows this charge", "order_id", orderID, "result", res.Status)
		_, err := s.completePayment(ctx, orderID, pay.ID, res)
		switch {
		case err == nil:
			rep.Confirmed++
		case errors.Is(err, ErrPaymentDeclined):
			rep.Cancelled++
		default:
			rep.Failed++
			slog.ErrorContext(ctx, "reconcile: completing payment failed", "order_id", orderID, "error", err)
		}
	}
}

// expireUncharged expires a PAYMENT_PENDING order whose charge never reached
// the provider: payment FAILED, order EXPIRED, stock released.
func (s *Service) expireUncharged(ctx context.Context, orderID, paymentID int64) (changed bool, err error) {
	err = database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		repo := s.orders.WithTx(tx)
		o, err := repo.GetForUpdate(ctx, orderID)
		if err != nil {
			return err
		}
		if o.Status != StatusPaymentPending {
			return nil // resolved meanwhile
		}
		if _, err := s.payments.WithTx(tx).MarkFailed(ctx, paymentID, "not_charged_before_expiry"); err != nil {
			return err
		}
		if err := s.transition(ctx, repo, &o, StatusExpired); err != nil {
			return err
		}
		if _, err := s.inventory.ReleaseForOrder(ctx, tx, orderID, inventory.ReservationExpired); err != nil {
			return err
		}
		changed = true
		slog.InfoContext(ctx, "reconcile: charge never happened; order expired and stock released", "order_id", orderID)
		return nil
	})
	return changed, err
}
