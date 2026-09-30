package orders_test

import (
	"context"
	"errors"
	"testing"

	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/orders"
)

// Integration tests for Stage 6: orders reserve stock, cancel releases it.

type stock struct{ available, reserved int }

func (f fixture) stockOf(t *testing.T, productID int64) stock {
	t.Helper()
	var s stock
	if err := f.pool.QueryRow(context.Background(),
		`SELECT available_quantity, reserved_quantity FROM inventory WHERE product_id = $1`, productID).
		Scan(&s.available, &s.reserved); err != nil {
		t.Fatal(err)
	}
	return s
}

// assertInvariant checks, for EVERY product, that reserved_quantity equals
// the sum of its ACTIVE reservations. If any code path forgot to update one
// side, this catches it.
func (f fixture) assertInvariant(t *testing.T) {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT i.product_id, i.reserved_quantity, COALESCE(SUM(r.quantity), 0)
		FROM inventory i
		LEFT JOIN inventory_reservations r ON r.product_id = i.product_id AND r.status = 'ACTIVE'
		GROUP BY i.product_id, i.reserved_quantity`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var productID int64
		var reserved, activeSum int
		if err := rows.Scan(&productID, &reserved, &activeSum); err != nil {
			t.Fatal(err)
		}
		if reserved != activeSum {
			t.Errorf("product %d: reserved_quantity = %d but ACTIVE reservations sum to %d", productID, reserved, activeSum)
		}
	}
}

func (f fixture) reservations(t *testing.T, orderID int64) map[int64]string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT product_id, status FROM inventory_reservations WHERE order_id = $1`, orderID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var pid int64
		var status string
		if err := rows.Scan(&pid, &status); err != nil {
			t.Fatal(err)
		}
		out[pid] = status
	}
	return out
}

func TestReserve_OnCreate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	kb := f.productWithStock(t, "KB", 1000, "INR", "ACTIVE", 10)
	mug := f.productWithStock(t, "MUG", 500, "INR", "ACTIVE", 5)

	o, err := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: kb, Quantity: 3}, {ProductID: mug, Quantity: 5}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if o.Status != orders.StatusReserved {
		t.Errorf("status = %s, want RESERVED", o.Status)
	}

	if s := f.stockOf(t, kb); s != (stock{7, 3}) {
		t.Errorf("keyboard stock = %+v, want 7 available / 3 reserved", s)
	}
	if s := f.stockOf(t, mug); s != (stock{0, 5}) {
		t.Errorf("mug stock = %+v, want 0 available / 5 reserved (exactly all of it)", s)
	}

	res := f.reservations(t, o.ID)
	if res[kb] != "ACTIVE" || res[mug] != "ACTIVE" || len(res) != 2 {
		t.Errorf("reservations = %v, want 2 ACTIVE", res)
	}

	// expires_at = transaction time + 15 minutes (computed by PostgreSQL).
	var minutes float64
	if err := f.pool.QueryRow(ctx,
		`SELECT EXTRACT(EPOCH FROM (expires_at - created_at)) / 60 FROM inventory_reservations WHERE order_id = $1 LIMIT 1`,
		o.ID).Scan(&minutes); err != nil {
		t.Fatal(err)
	}
	if minutes < 14.99 || minutes > 15.01 {
		t.Errorf("reservation lasts %.2f minutes, want 15", minutes)
	}
	f.assertInvariant(t)
}

// If ANY line is out of stock, the whole order fails and every line that
// was already reserved is rolled back: all-or-nothing.
func TestReserve_OutOfStockRollsBackEverything(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	plenty := f.productWithStock(t, "SKU-A", 100, "INR", "ACTIVE", 10)
	scarce := f.productWithStock(t, "SKU-B", 100, "INR", "ACTIVE", 1)

	// plenty (lower id) is reserved first, then scarce fails.
	_, err := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: plenty, Quantity: 2}, {ProductID: scarce, Quantity: 5}})
	if !errors.Is(err, inventory.ErrOutOfStock) {
		t.Fatalf("Create() error = %v, want ErrOutOfStock", err)
	}

	if s := f.stockOf(t, plenty); s != (stock{10, 0}) {
		t.Errorf("first line was not rolled back: %+v", s)
	}
	if s := f.stockOf(t, scarce); s != (stock{1, 0}) {
		t.Errorf("scarce stock changed: %+v", s)
	}
	if o, i := f.countOrders(t); o != 0 || i != 0 {
		t.Errorf("failed order left %d orders / %d items behind", o, i)
	}
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM inventory_reservations`).Scan(&n)
	if n != 0 {
		t.Errorf("failed order left %d reservations behind", n)
	}
}

func TestReserve_LastUnits(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "LAST", 100, "INR", "ACTIVE", 3)

	if _, err := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 3}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(ctx, f.bob, []orders.ItemRequest{{ProductID: p, Quantity: 1}}); !errors.Is(err, inventory.ErrOutOfStock) {
		t.Errorf("second order error = %v, want ErrOutOfStock", err)
	}
	if s := f.stockOf(t, p); s != (stock{0, 3}) {
		t.Errorf("stock = %+v, want 0/3", s)
	}
}

func TestCancel_ReleasesStockOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "SKU-P", 100, "INR", "ACTIVE", 10)

	o, err := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Cancel(ctx, f.alice, o.ID); err != nil {
		t.Fatal(err)
	}
	if s := f.stockOf(t, p); s != (stock{10, 0}) {
		t.Errorf("after cancel: %+v, want 10/0", s)
	}
	if res := f.reservations(t, o.ID); res[p] != "RELEASED" {
		t.Errorf("reservation = %v, want RELEASED", res)
	}

	// A second cancel is rejected by the state machine and must not
	// release the stock a second time.
	if _, err := f.svc.Cancel(ctx, f.alice, o.ID); !errors.Is(err, orders.ErrInvalidTransition) {
		t.Errorf("second cancel: %v", err)
	}
	if s := f.stockOf(t, p); s != (stock{10, 0}) {
		t.Errorf("after second cancel: %+v, want still 10/0", s)
	}
	f.assertInvariant(t)
}

// A product archived after the order was placed must not trap the stock.
func TestCancel_ReleasesArchivedProduct(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "SKU-P", 100, "INR", "ACTIVE", 5)
	o, err := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE products SET status = 'ARCHIVED' WHERE id = $1`, p); err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.Cancel(ctx, f.alice, o.ID); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if s := f.stockOf(t, p); s != (stock{5, 0}) {
		t.Errorf("after cancel: %+v, want 5/0", s)
	}
}

func TestReleaseForOrder_IsIdempotentAndValidatesTarget(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "SKU-P", 100, "INR", "ACTIVE", 5)
	o, err := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 2}})
	if err != nil {
		t.Fatal(err)
	}

	invSvc := inventory.NewService(f.pool, inventory.NewRepository(f.pool))
	release := func(to inventory.ReservationStatus) (int, error) {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		n, err := invSvc.ReleaseForOrder(ctx, tx, o.ID, to)
		if err == nil {
			err = tx.Commit(ctx)
		}
		return n, err
	}

	if _, err := release(inventory.ReservationConfirmed); err == nil {
		t.Error("ReleaseForOrder(CONFIRMED) must be rejected: confirming is not releasing")
	}
	if n, err := release(inventory.ReservationExpired); err != nil || n != 1 {
		t.Errorf("first release: n=%d err=%v, want 1, nil", n, err)
	}
	if n, err := release(inventory.ReservationExpired); err != nil || n != 0 {
		t.Errorf("second release: n=%d err=%v, want 0, nil (nothing left to release)", n, err)
	}
	if s := f.stockOf(t, p); s != (stock{5, 0}) {
		t.Errorf("stock = %+v, want 5/0 (released exactly once)", s)
	}
	f.assertInvariant(t)
}

// A mix of creates and cancels must always keep reserved_quantity equal to
// the sum of ACTIVE reservations.
func TestInvariant_AfterManyOperations(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a := f.productWithStock(t, "SKU-A", 100, "INR", "ACTIVE", 50)
	b := f.productWithStock(t, "SKU-B", 100, "INR", "ACTIVE", 50)

	var ids []int64
	for i := 1; i <= 8; i++ {
		o, err := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: a, Quantity: i}, {ProductID: b, Quantity: 9 - i}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, o.ID)
	}
	for _, id := range ids[:4] {
		if _, err := f.svc.Cancel(ctx, f.alice, id); err != nil {
			t.Fatal(err)
		}
	}
	f.assertInvariant(t)

	// Orders 5..8 still hold a = 5+6+7+8 = 26 and b = 4+3+2+1 = 10.
	if s := f.stockOf(t, a); s != (stock{24, 26}) {
		t.Errorf("A = %+v, want 24/26", s)
	}
	if s := f.stockOf(t, b); s != (stock{40, 10}) {
		t.Errorf("B = %+v, want 40/10", s)
	}
}
