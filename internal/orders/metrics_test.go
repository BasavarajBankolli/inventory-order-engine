package orders_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/metrics"
	"inventory-order-engine/internal/orders"
	"inventory-order-engine/internal/payments"
)

// Business metrics must count what really HAPPENED (committed), exactly
// once - not attempts, not rolled-back work, not idempotent replays.

func expectMetric(t *testing.T, m *metrics.Metrics, want float64, name string, labels ...string) {
	t.Helper()
	if got := m.Value(name, labels...); got != want {
		t.Errorf("%s%v = %v, want %v", name, labels, got, want)
	}
}

func TestMetrics_OrderCreatedAndReplay(t *testing.T) {
	m := metrics.New()
	f := setupWithMetrics(t, m)
	a := f.product(t, "M-A", 100, "INR", "ACTIVE")
	b := f.product(t, "M-B", 100, "INR", "ACTIVE")
	items := []orders.ItemRequest{{ProductID: a, Quantity: 1}, {ProductID: b, Quantity: 2}}

	if _, _, err := f.svc.CreateWithKey(context.Background(), f.alice, items, "key-1"); err != nil {
		t.Fatal(err)
	}
	// Same key again: the stored order is replayed, nothing new happens.
	if _, replayed, err := f.svc.CreateWithKey(context.Background(), f.alice, items, "key-1"); err != nil || !replayed {
		t.Fatalf("replay: replayed=%v err=%v", replayed, err)
	}

	expectMetric(t, m, 1, "orders_created_total")
	expectMetric(t, m, 2, "inventory_reservations_total", "event", "created") // one per order line
}

// Out of stock: the transaction rolls back, so nothing counts as created
// even though the first line was already reserved inside the transaction.
func TestMetrics_FailedOrdersByReason(t *testing.T) {
	m := metrics.New()
	f := setupWithMetrics(t, m)
	ok := f.product(t, "M-OK", 100, "INR", "ACTIVE")
	few := f.productWithStock(t, "M-FEW", 100, "INR", "ACTIVE", 1)
	inactive := f.product(t, "M-OFF", 100, "INR", "INACTIVE")
	ctx := context.Background()

	_, err := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: ok, Quantity: 1}, {ProductID: few, Quantity: 5}})
	if !errors.Is(err, inventory.ErrOutOfStock) {
		t.Fatalf("err = %v, want out of stock", err)
	}
	if _, err := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: inactive, Quantity: 1}}); err == nil {
		t.Fatal("inactive product: expected an error")
	}
	if _, err := f.svc.Create(ctx, f.alice, nil); err == nil {
		t.Fatal("no items: expected a validation error")
	}

	expectMetric(t, m, 1, "orders_failed_total", "reason", "out_of_stock")
	expectMetric(t, m, 1, "orders_failed_total", "reason", "product_unavailable")
	expectMetric(t, m, 1, "orders_failed_total", "reason", "validation")
	expectMetric(t, m, 0, "orders_created_total")
	expectMetric(t, m, 0, "inventory_reservations_total", "event", "created")
}

func TestMetrics_PaymentSuccessCountedOnce(t *testing.T) {
	m := metrics.New()
	f := setupWithMetrics(t, m)
	o, _ := f.placeOrder(t, "M-PAY", 10, 2)

	if _, err := f.svc.Pay(context.Background(), f.alice, o.ID); err != nil {
		t.Fatal(err)
	}
	// Paying an already-paid order again (double click, client retry)
	// must not count a second success.
	_, _ = f.svc.Pay(context.Background(), f.alice, o.ID)

	expectMetric(t, m, 1, "payments_success_total")
	expectMetric(t, m, 1, "inventory_reservations_total", "event", "confirmed")
}

// Many concurrent Pay calls for one order: one charge, one success metric.
func TestMetrics_ConcurrentPaysCountOnce(t *testing.T) {
	m := metrics.New()
	f := setupWithMetrics(t, m)
	o, _ := f.placeOrder(t, "M-RACE", 10, 1)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = f.svc.Pay(context.Background(), f.alice, o.ID)
		}()
	}
	close(start)
	wg.Wait()

	expectMetric(t, m, 1, "payments_success_total")
	expectMetric(t, m, 1, "inventory_reservations_total", "event", "confirmed")
}

func TestMetrics_PaymentDeclined(t *testing.T) {
	m := metrics.New()
	f := setupWithMetrics(t, m)
	o, _ := f.placeOrder(t, "M-DECL", 10, 1)

	if _, err := f.svc.Pay(simulate(payments.OutcomeFailure), f.alice, o.ID); !errors.Is(err, orders.ErrPaymentDeclined) {
		t.Fatalf("err = %v", err)
	}
	expectMetric(t, m, 1, "payments_failed_total", "reason", "declined")
	expectMetric(t, m, 1, "inventory_reservations_total", "event", "released")
	expectMetric(t, m, 0, "payments_success_total")
}

// Timeout, then a retry that finds the charge succeeded: one "timeout"
// failure (the provider did not answer in time) and one success.
func TestMetrics_PaymentTimeoutThenRetry(t *testing.T) {
	m := metrics.New()
	f := setupWithMetrics(t, m)
	o, _ := f.placeOrder(t, "M-TO", 10, 1)

	if _, err := f.svc.Pay(simulate(payments.OutcomeTimeout), f.alice, o.ID); !errors.Is(err, orders.ErrPaymentOutcomeUnknown) {
		t.Fatalf("err = %v", err)
	}
	expectMetric(t, m, 1, "payments_failed_total", "reason", "timeout")
	expectMetric(t, m, 0, "payments_success_total")

	if _, err := f.svc.Pay(context.Background(), f.alice, o.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	expectMetric(t, m, 1, "payments_success_total")
	expectMetric(t, m, 1, "inventory_reservations_total", "event", "confirmed")
}

func TestMetrics_CancelAndExpire(t *testing.T) {
	m := metrics.New()
	f := setupWithMetrics(t, m)

	cancelled, _ := f.placeOrder(t, "M-CAN", 10, 1)
	if _, err := f.svc.Cancel(context.Background(), f.alice, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	// Cancelling again changes nothing and must not count again.
	_, _ = f.svc.Cancel(context.Background(), f.alice, cancelled.ID)
	expectMetric(t, m, 1, "inventory_reservations_total", "event", "released")

	overdue, _ := f.placeOrder(t, "M-EXP", 10, 1)
	f.expireReservations(t, overdue.ID, "1 minute")
	f.expire(t)
	f.expire(t) // second run finds nothing to do
	expectMetric(t, m, 1, "inventory_reservations_total", "event", "expired")
}
