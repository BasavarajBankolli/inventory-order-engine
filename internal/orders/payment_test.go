package orders_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"inventory-order-engine/internal/orders"
	"inventory-order-engine/internal/payments"
)

// Integration tests for Stage 9 (skipped without TEST_DATABASE_URL).

// charges returns how many times the mock provider actually took money.
func (f fixture) charges(t *testing.T) int {
	t.Helper()
	n, err := f.provider.Charges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func simulate(o payments.Outcome) context.Context {
	return payments.WithSimulatedOutcome(context.Background(), o)
}

// placeOrder creates a RESERVED order for alice: qty units of a new product
// that has `stock` units.
func (f fixture) placeOrder(t *testing.T, sku string, stockUnits, qty int) (orders.Order, int64) {
	t.Helper()
	p := f.productWithStock(t, sku, 50000, "INR", "ACTIVE", stockUnits)
	o, err := f.svc.Create(context.Background(), f.alice, []orders.ItemRequest{{ProductID: p, Quantity: qty}})
	if err != nil {
		t.Fatal(err)
	}
	return o, p
}

func (f fixture) reservationStatuses(t *testing.T, orderID int64) map[string]int {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT status, count(*) FROM inventory_reservations WHERE order_id = $1 GROUP BY status`, orderID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		out[s] = n
	}
	return out
}

func TestPay_Success(t *testing.T) {
	f := setup(t)
	o, p := f.placeOrder(t, "PAY-OK", 10, 3) // stock 7 available / 3 reserved

	res, err := f.svc.Pay(context.Background(), f.alice, o.ID)
	if err != nil {
		t.Fatalf("Pay() error = %v", err)
	}
	if res.Order.Status != orders.StatusConfirmed || res.Payment.Status != payments.StatusSucceeded ||
		res.Payment.ProviderReference == "" || res.Payment.Amount != 150000 {
		t.Errorf("result = %+v", res)
	}

	// Sold: reserved goes down, available is NOT given back.
	if s := f.stockOf(t, p); s != (stock{7, 0}) {
		t.Errorf("stock = %+v, want 7 available / 0 reserved (3 units sold)", s)
	}
	if r := f.reservationStatuses(t, o.ID); r["CONFIRMED"] != 1 || len(r) != 1 {
		t.Errorf("reservations = %v, want 1 CONFIRMED", r)
	}
	if f.charges(t) != 1 {
		t.Errorf("charges = %d, want 1", f.charges(t))
	}
	f.assertInvariant(t)
}

// Spec demo: payment failure -> reservation released -> inventory restored.
func TestPay_FailureReleasesStock(t *testing.T) {
	f := setup(t)
	o, p := f.placeOrder(t, "PAY-FAIL", 10, 4)
	if s := f.stockOf(t, p); s != (stock{6, 4}) {
		t.Fatalf("before payment: %+v", s)
	}

	res, err := f.svc.Pay(simulate(payments.OutcomeFailure), f.alice, o.ID)
	if !errors.Is(err, orders.ErrPaymentDeclined) {
		t.Fatalf("Pay() error = %v, want ErrPaymentDeclined", err)
	}
	if res.Order.Status != orders.StatusCancelled || res.Payment.Status != payments.StatusFailed ||
		res.Payment.FailureReason != "card_declined" {
		t.Errorf("result = %+v", res)
	}
	if s := f.stockOf(t, p); s != (stock{10, 0}) {
		t.Errorf("after decline: %+v, want 10/0 (stock restored)", s)
	}
	if r := f.reservationStatuses(t, o.ID); r["RELEASED"] != 1 {
		t.Errorf("reservations = %v, want RELEASED", r)
	}
	if f.charges(t) != 0 {
		t.Errorf("charges = %d, want 0", f.charges(t))
	}
	f.assertInvariant(t)
}

// Timeout: the provider took the money but we never heard back. Nothing may
// be released or cancelled. The retry must find out about the success via
// the provider idempotency key - and must not charge a second time.
func TestPay_TimeoutThenRetryChargesOnce(t *testing.T) {
	f := setup(t)
	o, p := f.placeOrder(t, "PAY-TO", 10, 2)

	res, err := f.svc.Pay(simulate(payments.OutcomeTimeout), f.alice, o.ID)
	if !errors.Is(err, orders.ErrPaymentOutcomeUnknown) {
		t.Fatalf("first Pay() error = %v, want ErrPaymentOutcomeUnknown", err)
	}
	if res.Order.Status != orders.StatusPaymentPending || res.Payment.Status != payments.StatusPending {
		t.Errorf("after timeout: order %s, payment %s; want PAYMENT_PENDING / PENDING", res.Order.Status, res.Payment.Status)
	}
	if s := f.stockOf(t, p); s != (stock{8, 2}) {
		t.Errorf("after timeout: %+v, want still 8/2 (outcome unknown: do not release)", s)
	}

	// A customer cannot cancel while the payment is unresolved.
	if _, err := f.svc.Cancel(context.Background(), f.alice, o.ID); !errors.Is(err, orders.ErrInvalidTransition) {
		t.Errorf("Cancel(PAYMENT_PENDING) = %v, want ErrInvalidTransition", err)
	}

	// Retry (default outcome = SUCCESS). The provider recognises the key.
	res, err = f.svc.Pay(context.Background(), f.alice, o.ID)
	if err != nil || res.Order.Status != orders.StatusConfirmed {
		t.Fatalf("retry: %+v, %v", res.Order.Status, err)
	}
	if f.charges(t) != 1 {
		t.Errorf("charges = %d, want exactly 1 (no double charge)", f.charges(t))
	}
	if s := f.stockOf(t, p); s != (stock{8, 0}) {
		t.Errorf("after confirm: %+v, want 8/0", s)
	}
	f.assertInvariant(t)
}

func TestPay_IsIdempotentAfterSuccess(t *testing.T) {
	f := setup(t)
	o, _ := f.placeOrder(t, "PAY-TWICE", 10, 1)

	first, err := f.svc.Pay(context.Background(), f.alice, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.Pay(context.Background(), f.alice, o.ID)
	if err != nil || second.Payment.ID != first.Payment.ID || second.Order.Status != orders.StatusConfirmed {
		t.Errorf("second Pay() = %+v, %v; want the same confirmed payment", second, err)
	}
	if f.charges(t) != 1 {
		t.Errorf("charges = %d, want 1", f.charges(t))
	}
}

// Ten simultaneous "Pay" clicks for one order: one payment row, one charge.
func TestPay_ConcurrentClicksChargeOnce(t *testing.T) {
	f := setup(t)
	o, p := f.placeOrder(t, "PAY-RACE", 10, 2)

	var mu sync.Mutex
	var failures []error
	race(10, func(int) {
		res, err := f.svc.Pay(context.Background(), f.alice, o.ID)
		mu.Lock()
		defer mu.Unlock()
		if err != nil || res.Order.Status != orders.StatusConfirmed {
			failures = append(failures, err)
		}
	})

	if len(failures) > 0 {
		t.Errorf("%d calls did not return a confirmed order, first: %v", len(failures), failures[0])
	}
	if f.charges(t) != 1 {
		t.Errorf("charges = %d, want 1", f.charges(t))
	}
	if n := f.count(t, `SELECT count(*) FROM payments WHERE order_id = $1`, o.ID); n != 1 {
		t.Errorf("%d payment rows, want 1", n)
	}
	if s := f.stockOf(t, p); s != (stock{8, 0}) {
		t.Errorf("stock = %+v, want 8/0 (confirmed exactly once)", s)
	}
	f.assertInvariant(t)
}

func TestPay_Rejections(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// Someone else's order does not exist for you.
	o1, _ := f.placeOrder(t, "PAY-R1", 10, 1)
	if _, err := f.svc.Pay(ctx, f.bob, o1.ID); !errors.Is(err, orders.ErrNotFound) {
		t.Errorf("bob pays alice's order: %v, want ErrNotFound", err)
	}

	// A cancelled order cannot be paid.
	if _, err := f.svc.Cancel(ctx, f.alice, o1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Pay(ctx, f.alice, o1.ID); !errors.Is(err, orders.ErrInvalidTransition) {
		t.Errorf("pay cancelled order: %v, want ErrInvalidTransition", err)
	}

	// An expired reservation cannot be paid, and nothing is charged.
	o2, _ := f.placeOrder(t, "PAY-R2", 10, 1)
	if _, err := f.pool.Exec(ctx,
		`UPDATE inventory_reservations SET created_at = now() - interval '1 hour', expires_at = now() - interval '1 minute' WHERE order_id = $1`,
		o2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Pay(ctx, f.alice, o2.ID); !errors.Is(err, orders.ErrReservationExpired) {
		t.Errorf("pay expired order: %v, want ErrReservationExpired", err)
	}
	if f.charges(t) != 0 {
		t.Errorf("charges = %d, want 0", f.charges(t))
	}
}

func TestPayments_DatabaseConstraints(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	o, _ := f.placeOrder(t, "PAY-DB", 10, 1)
	if _, err := f.svc.Pay(simulate(payments.OutcomeTimeout), f.alice, o.ID); err == nil {
		t.Fatal("expected a timeout")
	}

	bad := map[string]string{
		"second payment for the order": `INSERT INTO payments (order_id, amount, currency) VALUES ($1, 1, 'INR')`,
		"succeeded without reference":  `UPDATE payments SET status = 'SUCCEEDED' WHERE order_id = $1`,
		"unknown status":               `UPDATE payments SET status = 'REFUNDED' WHERE order_id = $1`,
		"zero amount":                  `UPDATE payments SET amount = 0 WHERE order_id = $1`,
	}
	for name, sql := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := f.pool.Exec(ctx, sql, o.ID); err == nil {
				t.Error("statement succeeded, want constraint violation")
			}
		})
	}
}
