package orders_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-order-engine/internal/events"
	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/metrics"
	"inventory-order-engine/internal/orders"
	"inventory-order-engine/internal/payments"
	"inventory-order-engine/internal/products"
	"inventory-order-engine/internal/testutil"
	"inventory-order-engine/internal/validate"
)

// Integration tests against real PostgreSQL (skipped without TEST_DATABASE_URL).

type fixture struct {
	pool     *pgxpool.Pool
	svc      *orders.Service
	provider *payments.MockProvider
	alice    identity.Principal // customer
	bob      identity.Principal // customer
	admin    identity.Principal
}

func setup(t *testing.T) fixture {
	t.Helper()
	return setupWithMetrics(t, nil)
}

// setupWithMetrics is setup with a metrics recorder (m may be nil).
func setupWithMetrics(t *testing.T, m *metrics.Metrics) fixture {
	t.Helper()
	pool := testutil.NewMigratedPool(t)
	provider := payments.NewMockProvider(pool, payments.OutcomeSuccess)
	f := fixture{
		pool: pool,
		svc: orders.NewService(orders.Deps{
			Pool:           pool,
			Orders:         orders.NewRepository(pool),
			Products:       products.NewRepository(pool),
			Inventory:      inventory.NewService(pool, inventory.NewRepository(pool)),
			Payments:       payments.NewRepository(pool),
			Provider:       provider,
			Outbox:         events.NewOutbox(pool),
			ReservationTTL: 15 * time.Minute,
			PaymentTimeout: 200 * time.Millisecond,
			ReconcileAfter: time.Second,
			Metrics:        m,
		}),
		provider: provider,
	}
	f.alice = identity.Principal{UserID: f.user(t, "alice@x.com"), Role: identity.RoleCustomer}
	f.bob = identity.Principal{UserID: f.user(t, "bob@x.com"), Role: identity.RoleCustomer}
	f.admin = identity.Principal{UserID: f.user(t, "admin@x.com"), Role: identity.RoleAdmin}
	return f
}

func (f fixture) user(t *testing.T, email string) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO users (email, name, password_hash) VALUES ($1, 'U', 'x') RETURNING id`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// product inserts a product plus its inventory row with 100 units available.
func (f fixture) product(t *testing.T, sku string, price int64, currency, status string) int64 {
	t.Helper()
	return f.productWithStock(t, sku, price, currency, status, 100)
}

func (f fixture) productWithStock(t *testing.T, sku string, price int64, currency, status string, stock int) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO products (sku, name, price, currency, status) VALUES ($1, 'P', $2, $3, $4) RETURNING id`,
		sku, price, currency, status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO inventory (product_id, available_quantity) VALUES ($1, $2)`, id, stock); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f fixture) countOrders(t *testing.T) (orders, items int) {
	t.Helper()
	err := f.pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM orders), (SELECT count(*) FROM order_items)`).Scan(&orders, &items)
	if err != nil {
		t.Fatal(err)
	}
	return orders, items
}

func TestCreate_PersistsOrderItemsAndPriceSnapshot(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	kb := f.product(t, "KB", 249900, "INR", "ACTIVE")
	mug := f.product(t, "MUG", 29900, "INR", "ACTIVE")

	o, err := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: kb, Quantity: 1}, {ProductID: mug, Quantity: 4}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if o.Status != orders.StatusReserved || o.UserID != f.alice.UserID || o.TotalAmount != 249900+4*29900 || o.Currency != "INR" {
		t.Errorf("order = %+v", o)
	}

	// Price change AFTER ordering must not affect the stored order.
	if _, err := f.pool.Exec(ctx, `UPDATE products SET price = 1 WHERE id = $1`, kb); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Get(ctx, f.alice, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalAmount != 369500 || len(got.Items) != 2 || got.Items[0].UnitPrice != 249900 {
		t.Errorf("stored order changed after price update: %+v", got)
	}
}

func TestCreate_RejectsAndWritesNothing(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	inr := f.product(t, "INR-1", 100, "INR", "ACTIVE")
	usd := f.product(t, "USD-1", 100, "USD", "ACTIVE")
	inactive := f.product(t, "OFF-1", 100, "INR", "INACTIVE")
	archived := f.product(t, "OLD-1", 100, "INR", "ARCHIVED")

	cases := []struct {
		name  string
		items []orders.ItemRequest
		check func(error) bool
	}{
		{"missing product", []orders.ItemRequest{{ProductID: inr, Quantity: 1}, {ProductID: 424242, Quantity: 1}},
			func(err error) bool { var v validate.Errors; return errors.As(err, &v) }},
		{"archived product", []orders.ItemRequest{{ProductID: archived, Quantity: 1}},
			func(err error) bool { var v validate.Errors; return errors.As(err, &v) }},
		{"inactive product", []orders.ItemRequest{{ProductID: inr, Quantity: 1}, {ProductID: inactive, Quantity: 1}},
			func(err error) bool { return errors.Is(err, orders.ErrProductUnavailable) }},
		{"mixed currency", []orders.ItemRequest{{ProductID: inr, Quantity: 1}, {ProductID: usd, Quantity: 1}},
			func(err error) bool { var v validate.Errors; return errors.As(err, &v) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Create(ctx, f.alice, tc.items)
			if !tc.check(err) {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}

	if o, i := f.countOrders(t); o != 0 || i != 0 {
		t.Errorf("rejected orders left rows behind: %d orders, %d items", o, i)
	}
}

func TestOwnership(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.product(t, "P-1", 100, "INR", "ACTIVE")
	aliceOrder, _ := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 1}})
	_, _ = f.svc.Create(ctx, f.bob, []orders.ItemRequest{{ProductID: p, Quantity: 2}})

	// Bob cannot see or cancel Alice's order: it "does not exist" for him.
	if _, err := f.svc.Get(ctx, f.bob, aliceOrder.ID); !errors.Is(err, orders.ErrNotFound) {
		t.Errorf("bob Get(alice's order) = %v, want ErrNotFound", err)
	}
	if _, err := f.svc.Cancel(ctx, f.bob, aliceOrder.ID); !errors.Is(err, orders.ErrNotFound) {
		t.Errorf("bob Cancel(alice's order) = %v, want ErrNotFound", err)
	}

	// The admin can.
	if _, err := f.svc.Get(ctx, f.admin, aliceOrder.ID); err != nil {
		t.Errorf("admin Get() = %v", err)
	}

	// Lists are scoped: each customer sees 1, the admin sees 2.
	for _, c := range []struct {
		who  identity.Principal
		want int64
	}{{f.alice, 1}, {f.bob, 1}, {f.admin, 2}} {
		res, err := f.svc.List(ctx, c.who, orders.ListInput{})
		if err != nil || res.Total != c.want {
			t.Errorf("List(user %d) total = %d, %v; want %d", c.who.UserID, res.Total, err, c.want)
		}
	}
}

func TestCancel(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.product(t, "P-1", 100, "INR", "ACTIVE")
	o, _ := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 1}})

	cancelled, err := f.svc.Cancel(ctx, f.alice, o.ID)
	if err != nil || cancelled.Status != orders.StatusCancelled || len(cancelled.Items) != 1 {
		t.Fatalf("Cancel() = %+v, %v", cancelled, err)
	}

	// CANCELLED is final.
	if _, err := f.svc.Cancel(ctx, f.alice, o.ID); !errors.Is(err, orders.ErrInvalidTransition) {
		t.Errorf("second Cancel() = %v, want ErrInvalidTransition", err)
	}

	// An order in PAYMENT_PENDING cannot be cancelled (payment in flight).
	o2, _ := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 1}})
	if _, err := f.pool.Exec(ctx, `UPDATE orders SET status = 'PAYMENT_PENDING' WHERE id = $1`, o2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Cancel(ctx, f.alice, o2.ID); !errors.Is(err, orders.ErrInvalidTransition) {
		t.Errorf("Cancel(PAYMENT_PENDING) = %v, want ErrInvalidTransition", err)
	}
}

// 20 simultaneous cancel requests for one order: the row lock makes them
// run one after another, so exactly one succeeds and the rest see an order
// that is already CANCELLED.
func TestCancel_Concurrent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.product(t, "P-1", 100, "INR", "ACTIVE")
	o, _ := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 1}})

	const n = 20
	var (
		wg              sync.WaitGroup
		mu              sync.Mutex
		ok, invalid     int
		start           = make(chan struct{})
		unexpectedError error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.svc.Cancel(ctx, f.alice, o.ID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, orders.ErrInvalidTransition):
				invalid++
			default:
				unexpectedError = err
			}
		}()
	}
	close(start)
	wg.Wait()

	if unexpectedError != nil || ok != 1 || invalid != n-1 {
		t.Errorf("ok=%d invalid=%d err=%v; want 1, %d, nil", ok, invalid, unexpectedError, n-1)
	}
}

func TestList_FilterAndPaginate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.product(t, "P-1", 100, "INR", "ACTIVE")
	var ids []int64
	for i := 0; i < 5; i++ {
		o, err := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: p, Quantity: i + 1}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, o.ID)
	}
	if _, err := f.svc.Cancel(ctx, f.alice, ids[0]); err != nil {
		t.Fatal(err)
	}

	page, err := f.svc.List(ctx, f.alice, orders.ListInput{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Newest first: ids[4], ids[3], ids[2], ... -> offset 1, limit 2 = ids[3], ids[2]
	if page.Total != 5 || len(page.Items) != 2 || page.Items[0].ID != ids[3] || page.Items[1].ID != ids[2] {
		t.Errorf("page = %+v", page)
	}

	cancelled, _ := f.svc.List(ctx, f.alice, orders.ListInput{Status: orders.StatusCancelled})
	if cancelled.Total != 1 || cancelled.Items[0].ID != ids[0] {
		t.Errorf("cancelled filter = %+v", cancelled)
	}

	if _, err := f.svc.List(ctx, f.alice, orders.ListInput{Status: "BOGUS"}); err == nil {
		t.Error("List(status=BOGUS) error = nil, want validation error")
	}
}

func TestDatabase_Constraints(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.product(t, "P-1", 100, "INR", "ACTIVE")
	o, _ := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 1}})

	bad := map[string]string{
		"unknown status":         `UPDATE orders SET status = 'LOST' WHERE id = $1`,
		"zero total":             `UPDATE orders SET total_amount = 0 WHERE id = $1`,
		"line total mismatch":    `UPDATE order_items SET total_price = 1 WHERE order_id = $1`,
		"zero quantity":          `UPDATE order_items SET quantity = 0, total_price = 0 WHERE order_id = $1`,
		"duplicate product line": `INSERT INTO order_items (order_id, product_id, quantity, unit_price, total_price) SELECT $1, product_id, 1, 100, 100 FROM order_items WHERE order_id = $1`,
	}
	for name, sql := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := f.pool.Exec(ctx, sql, o.ID); err == nil {
				t.Error("statement succeeded, want constraint violation")
			}
		})
	}
}
