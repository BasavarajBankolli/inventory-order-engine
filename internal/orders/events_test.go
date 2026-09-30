package orders_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/orders"
	"inventory-order-engine/internal/payments"
)

// The orders module must write exactly the right outbox events, in the
// same transaction as each change (skipped without TEST_DATABASE_URL).

type storedEvent struct {
	Type    string
	Payload map[string]any
}

func (f fixture) eventsFor(t *testing.T, orderID int64) []storedEvent {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT event_type, payload FROM outbox_events
		WHERE aggregate_type = 'order' AND aggregate_id = $1 ORDER BY id`, orderID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []storedEvent
	for rows.Next() {
		var e storedEvent
		var raw []byte
		if err := rows.Scan(&e.Type, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &e.Payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func types(evs []storedEvent) string {
	s := make([]string, len(evs))
	for i, e := range evs {
		s[i] = e.Type
	}
	return fmt.Sprint(s)
}

func TestEvents_CreateEmitsCreatedAndReserved(t *testing.T) {
	f := setup(t)
	o, p := f.placeOrder(t, "EV-1", 10, 3)

	evs := f.eventsFor(t, o.ID)
	if types(evs) != "[OrderCreated InventoryReserved]" {
		t.Fatalf("events = %s", types(evs))
	}
	created, reserved := evs[0].Payload, evs[1].Payload
	if created["order_id"] != float64(o.ID) || created["total_amount"] != float64(150000) || created["currency"] != "INR" {
		t.Errorf("OrderCreated payload = %v", created)
	}
	items := reserved["items"].([]any)
	first := items[0].(map[string]any)
	if len(items) != 1 || first["product_id"] != float64(p) || first["quantity"] != float64(3) || reserved["expires_at"] == "" {
		t.Errorf("InventoryReserved payload = %v", reserved)
	}
}

// A failed transaction must not leave events behind: no event about an
// order that does not exist.
func TestEvents_FailedCreateEmitsNothing(t *testing.T) {
	f := setup(t)
	p := f.productWithStock(t, "EV-OOS", 100, "INR", "ACTIVE", 1)

	_, err := f.svc.Create(context.Background(), f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 5}})
	if !errors.Is(err, inventory.ErrOutOfStock) {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM outbox_events`); n != 0 {
		t.Errorf("%d events written for a rolled-back order", n)
	}
}

func TestEvents_CancelEmitsOrderCancelled(t *testing.T) {
	f := setup(t)
	o, _ := f.placeOrder(t, "EV-2", 10, 1)
	if _, err := f.svc.Cancel(context.Background(), f.alice, o.ID); err != nil {
		t.Fatal(err)
	}
	evs := f.eventsFor(t, o.ID)
	if types(evs) != "[OrderCreated InventoryReserved OrderCancelled]" || evs[2].Payload["reason"] != "customer_cancelled" {
		t.Errorf("events = %s, last payload %v", types(evs), evs[len(evs)-1].Payload)
	}
}

func TestEvents_PaymentOutcomes(t *testing.T) {
	f := setup(t)

	paid, _ := f.placeOrder(t, "EV-PAID", 10, 1)
	if _, err := f.svc.Pay(context.Background(), f.alice, paid.ID); err != nil {
		t.Fatal(err)
	}
	evs := f.eventsFor(t, paid.ID)
	if types(evs) != "[OrderCreated InventoryReserved OrderConfirmed]" || evs[2].Payload["provider_reference"] == "" {
		t.Errorf("paid: events = %s, last payload %v", types(evs), evs[len(evs)-1].Payload)
	}

	declined, _ := f.placeOrder(t, "EV-DECL", 10, 1)
	if _, err := f.svc.Pay(simulate(payments.OutcomeFailure), f.alice, declined.ID); !errors.Is(err, orders.ErrPaymentDeclined) {
		t.Fatal(err)
	}
	evs = f.eventsFor(t, declined.ID)
	if types(evs) != "[OrderCreated InventoryReserved OrderCancelled]" || evs[2].Payload["reason"] != "payment_declined" {
		t.Errorf("declined: events = %s", types(evs))
	}

	// A timeout changes nothing, so it emits nothing.
	pending, _ := f.placeOrder(t, "EV-TO", 10, 1)
	if _, err := f.svc.Pay(simulate(payments.OutcomeTimeout), f.alice, pending.ID); !errors.Is(err, orders.ErrPaymentOutcomeUnknown) {
		t.Fatal(err)
	}
	if evs := f.eventsFor(t, pending.ID); types(evs) != "[OrderCreated InventoryReserved]" {
		t.Errorf("timeout: events = %s, want no new event", types(evs))
	}
}

func TestEvents_ExpiryEmitsOrderExpired(t *testing.T) {
	f := setup(t)
	o, _ := f.placeOrder(t, "EV-EXP", 10, 2)
	f.expireReservations(t, o.ID, "1 second")
	f.expire(t)

	evs := f.eventsFor(t, o.ID)
	if types(evs) != "[OrderCreated InventoryReserved OrderExpired]" || evs[2].Payload["reason"] != "reservation_expired" {
		t.Errorf("events = %s", types(evs))
	}

	// Running the worker again must not emit a duplicate.
	f.expire(t)
	if n := len(f.eventsFor(t, o.ID)); n != 3 {
		t.Errorf("%d events after a second expiry run, want still 3", n)
	}
}
