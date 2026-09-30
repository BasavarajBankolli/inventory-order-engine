package orders_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"inventory-order-engine/internal/money"
	"inventory-order-engine/internal/orders"
	"inventory-order-engine/internal/payments"
)

// Integration tests for the Stage 10 expiry/reconciliation job.
// The fixture's service uses PaymentTimeout = 200ms and ReconcileAfter = 1s.

// expireReservations moves an order's reservations into the past so they
// look expired: expires_at = now() - ago.
func (f fixture) expireReservations(t *testing.T, orderID int64, ago string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE inventory_reservations
		SET created_at = now() - interval '1 hour', expires_at = now() - $2::interval
		WHERE order_id = $1`, orderID, ago); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) status(t *testing.T, orderID int64) orders.Status {
	t.Helper()
	var s orders.Status
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM orders WHERE id = $1`, orderID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (f fixture) paymentOf(t *testing.T, orderID int64) (status, reason string) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, COALESCE(failure_reason, '') FROM payments WHERE order_id = $1`, orderID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	return status, reason
}

func (f fixture) expire(t *testing.T) orders.ExpiryReport {
	t.Helper()
	rep, err := f.svc.ExpireOverdue(context.Background(), 100)
	if err != nil {
		t.Fatalf("ExpireOverdue() error = %v", err)
	}
	return rep
}

func TestExpire_ReservedOrder(t *testing.T) {
	f := setup(t)
	overdue, p := f.placeOrder(t, "EXP-1", 10, 3)
	fresh, err := f.svc.Create(context.Background(), f.bob, []orders.ItemRequest{{ProductID: p, Quantity: 2}})
	if err != nil {
		t.Fatal(err)
	}
	f.expireReservations(t, overdue.ID, "1 second")

	rep := f.expire(t)
	if rep.Expired != 1 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want 1 expired", rep)
	}
	if s := f.status(t, overdue.ID); s != orders.StatusExpired {
		t.Errorf("overdue order = %s, want EXPIRED", s)
	}
	if s := f.status(t, fresh.ID); s != orders.StatusReserved {
		t.Errorf("fresh order = %s, want still RESERVED", s)
	}
	// 10 - 3 - 2 = 5 available before; the overdue 3 came back.
	if st := f.stockOf(t, p); st != (stock{8, 2}) {
		t.Errorf("stock = %+v, want 8 available / 2 reserved", st)
	}
	if r := f.reservationStatuses(t, overdue.ID); r["EXPIRED"] != 1 {
		t.Errorf("reservations = %v, want EXPIRED", r)
	}
	f.assertInvariant(t)
}

func TestExpire_IsIdempotent(t *testing.T) {
	f := setup(t)
	o, p := f.placeOrder(t, "EXP-2", 10, 4)
	f.expireReservations(t, o.ID, "1 second")

	first := f.expire(t)
	second := f.expire(t)
	if first.Expired != 1 || second.Total() != 0 {
		t.Errorf("first = %+v, second = %+v; want 1 then nothing", first, second)
	}
	if st := f.stockOf(t, p); st != (stock{10, 0}) {
		t.Errorf("stock = %+v, want 10/0 (released exactly once)", st)
	}
}

// Five worker processes running the job at the same moment over the same
// ten overdue orders: every order is expired exactly once.
func TestExpire_ConcurrentWorkers(t *testing.T) {
	f := setup(t)
	p := f.productWithStock(t, "EXP-MANY", 100, "INR", "ACTIVE", 100)
	buyers := f.customers(t, 10)
	for _, b := range buyers {
		o, err := f.svc.Create(context.Background(), b, []orders.ItemRequest{{ProductID: p, Quantity: 3}})
		if err != nil {
			t.Fatal(err)
		}
		f.expireReservations(t, o.ID, "1 second")
	}

	var mu sync.Mutex
	var total orders.ExpiryReport
	race(5, func(int) {
		rep, err := f.svc.ExpireOverdue(context.Background(), 100)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		total.Expired += rep.Expired
		total.Failed += rep.Failed
		mu.Unlock()
	})

	if total.Expired != 10 || total.Failed != 0 {
		t.Errorf("combined report = %+v, want exactly 10 expired", total)
	}
	if st := f.stockOf(t, p); st != (stock{100, 0}) {
		t.Errorf("stock = %+v, want 100/0", st)
	}
	f.assertInvariant(t)
}

// The customer cancels at the same moment the worker expires the order.
// Exactly one of them wins; stock is released exactly once.
func TestExpire_RacesWithCancel(t *testing.T) {
	for i := 0; i < 10; i++ {
		f := setup(t)
		o, p := f.placeOrder(t, "EXP-RACE", 10, 4)
		f.expireReservations(t, o.ID, "1 second")

		race(2, func(i int) {
			if i == 0 {
				_, _ = f.svc.Cancel(context.Background(), f.alice, o.ID)
			} else {
				_, _ = f.svc.ExpireOverdue(context.Background(), 100)
			}
		})

		if s := f.status(t, o.ID); s != orders.StatusCancelled && s != orders.StatusExpired {
			t.Fatalf("status = %s, want CANCELLED or EXPIRED", s)
		}
		if st := f.stockOf(t, p); st != (stock{10, 0}) {
			t.Fatalf("stock = %+v, want 10/0", st)
		}
	}
}

// A PAYMENT_PENDING order is left alone during the grace period: a charge
// may still be in flight.
func TestReconcile_WaitsForGracePeriod(t *testing.T) {
	f := setup(t)
	o, p := f.placeOrder(t, "REC-GRACE", 10, 2)
	if _, err := f.svc.Pay(simulate(payments.OutcomeTimeout), f.alice, o.ID); !errors.Is(err, orders.ErrPaymentOutcomeUnknown) {
		t.Fatal(err)
	}
	f.expireReservations(t, o.ID, "100 milliseconds") // expired, but within the 1s grace

	if rep := f.expire(t); rep.Total() != 0 {
		t.Errorf("report = %+v, want nothing done inside the grace period", rep)
	}
	if s := f.status(t, o.ID); s != orders.StatusPaymentPending {
		t.Errorf("status = %s, want PAYMENT_PENDING", s)
	}
	if st := f.stockOf(t, p); st != (stock{8, 2}) {
		t.Errorf("stock = %+v, want still 8/2", st)
	}
}

// Payment timed out but the money WAS taken, and the customer never
// retried. The worker must find out and CONFIRM the order - not expire it.
func TestReconcile_ChargedTimeoutIsConfirmed(t *testing.T) {
	f := setup(t)
	o, p := f.placeOrder(t, "REC-PAID", 10, 2)
	if _, err := f.svc.Pay(simulate(payments.OutcomeTimeout), f.alice, o.ID); !errors.Is(err, orders.ErrPaymentOutcomeUnknown) {
		t.Fatal(err)
	}
	f.expireReservations(t, o.ID, "5 seconds") // past the grace period

	// Meanwhile the customer can no longer START a charge for this order.
	if _, err := f.svc.Pay(context.Background(), f.alice, o.ID); !errors.Is(err, orders.ErrReservationExpired) {
		t.Errorf("Pay after expiry = %v, want ErrReservationExpired", err)
	}

	rep := f.expire(t)
	if rep.Confirmed != 1 {
		t.Fatalf("report = %+v, want 1 confirmed", rep)
	}
	if s := f.status(t, o.ID); s != orders.StatusConfirmed {
		t.Errorf("status = %s, want CONFIRMED", s)
	}
	if st := f.stockOf(t, p); st != (stock{8, 0}) {
		t.Errorf("stock = %+v, want 8/0 (sold, not released)", st)
	}
	if n := f.charges(t); n != 1 {
		t.Errorf("charges = %d, want 1", n)
	}
	f.assertInvariant(t)
}

// Crash between "payment row created" and "provider called": the provider
// never saw the charge. The worker can safely expire the order.
func TestReconcile_NeverChargedIsExpired(t *testing.T) {
	f := setup(t)
	o, p := f.placeOrder(t, "REC-NOCHARGE", 10, 3)
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE orders SET status = 'PAYMENT_PENDING' WHERE id = $1;`, o.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO payments (order_id, amount, currency) SELECT id, total_amount, currency FROM orders WHERE id = $1`, o.ID); err != nil {
		t.Fatal(err)
	}
	f.expireReservations(t, o.ID, "5 seconds")

	rep := f.expire(t)
	if rep.Expired != 1 {
		t.Fatalf("report = %+v, want 1 expired", rep)
	}
	if s := f.status(t, o.ID); s != orders.StatusExpired {
		t.Errorf("status = %s, want EXPIRED", s)
	}
	if st, reason := f.paymentOf(t, o.ID); st != "FAILED" || reason != "not_charged_before_expiry" {
		t.Errorf("payment = %s/%s", st, reason)
	}
	if st := f.stockOf(t, p); st != (stock{10, 0}) {
		t.Errorf("stock = %+v, want 10/0", st)
	}
	f.assertInvariant(t)
}

// The provider declined, but we crashed before recording it. The worker
// learns about the decline and cancels the order.
func TestReconcile_DeclinedIsCancelled(t *testing.T) {
	f := setup(t)
	o, p := f.placeOrder(t, "REC-DECLINED", 10, 1)
	var paymentID int64
	if _, err := f.pool.Exec(context.Background(), `UPDATE orders SET status = 'PAYMENT_PENDING' WHERE id = $1`, o.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO payments (order_id, amount, currency) SELECT id, total_amount, currency FROM orders WHERE id = $1 RETURNING id`,
		o.ID).Scan(&paymentID); err != nil {
		t.Fatal(err)
	}
	pay := payments.Payment{ID: paymentID}
	if _, err := f.provider.Charge(simulate(payments.OutcomeFailure), payments.ChargeRequest{
		Amount: money.New(o.TotalAmount, o.Currency), IdempotencyKey: pay.IdempotencyKey(),
	}); err != nil {
		t.Fatal(err)
	}
	f.expireReservations(t, o.ID, "5 seconds")

	rep := f.expire(t)
	if rep.Cancelled != 1 {
		t.Fatalf("report = %+v, want 1 cancelled", rep)
	}
	if s := f.status(t, o.ID); s != orders.StatusCancelled {
		t.Errorf("status = %s, want CANCELLED", s)
	}
	if st := f.stockOf(t, p); st != (stock{10, 0}) {
		t.Errorf("stock = %+v, want 10/0", st)
	}
	f.assertInvariant(t)
}
