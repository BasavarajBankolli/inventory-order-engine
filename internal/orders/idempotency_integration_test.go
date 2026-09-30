package orders_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/orders"
)

// Integration tests for idempotency keys (skipped without TEST_DATABASE_URL).

func TestIdempotency_RetryReturnsSameOrder(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "IDEM-1", 1000, "INR", "ACTIVE", 10)
	items := []orders.ItemRequest{{ProductID: p, Quantity: 3}}

	first, replayed, err := f.svc.CreateWithKey(ctx, f.alice, items, "key-123")
	if err != nil || replayed {
		t.Fatalf("first request: replayed=%v err=%v", replayed, err)
	}

	// "Network timeout, client retries."
	second, replayed, err := f.svc.CreateWithKey(ctx, f.alice, items, "key-123")
	if err != nil || !replayed {
		t.Fatalf("retry: replayed=%v err=%v, want a replay", replayed, err)
	}
	if second.ID != first.ID || second.TotalAmount != first.TotalAmount || len(second.Items) != 1 {
		t.Errorf("retry returned %+v, want order %d", second, first.ID)
	}

	// Exactly one order exists and stock was reserved once, not twice.
	if n := f.count(t, `SELECT count(*) FROM orders`); n != 1 {
		t.Errorf("%d orders, want 1", n)
	}
	if s := f.stockOf(t, p); s != (stock{7, 3}) {
		t.Errorf("stock = %+v, want 7/3 (reserved once)", s)
	}
}

func TestIdempotency_SameKeyDifferentRequestIsRejected(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "IDEM-2", 1000, "INR", "ACTIVE", 10)

	if _, _, err := f.svc.CreateWithKey(ctx, f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 1}}, "k"); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.svc.CreateWithKey(ctx, f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 5}}, "k")
	if !errors.Is(err, orders.ErrIdempotencyKeyReused) {
		t.Errorf("error = %v, want ErrIdempotencyKeyReused", err)
	}
	if s := f.stockOf(t, p); s != (stock{9, 1}) {
		t.Errorf("stock = %+v, want 9/1 (second request must not reserve)", s)
	}
}

func TestIdempotency_KeysArePerUser(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "IDEM-3", 1000, "INR", "ACTIVE", 10)
	items := []orders.ItemRequest{{ProductID: p, Quantity: 1}}

	a, _, err := f.svc.CreateWithKey(ctx, f.alice, items, "shared-key")
	if err != nil {
		t.Fatal(err)
	}
	b, replayed, err := f.svc.CreateWithKey(ctx, f.bob, items, "shared-key")
	if err != nil || replayed {
		t.Fatalf("bob: replayed=%v err=%v; want his own new order", replayed, err)
	}
	if a.ID == b.ID || b.UserID != f.bob.UserID {
		t.Errorf("bob got alice's order: alice=%d bob=%d", a.ID, b.ID)
	}
}

func TestIdempotency_NoKeyMeansNoDeduplication(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "IDEM-4", 1000, "INR", "ACTIVE", 10)
	items := []orders.ItemRequest{{ProductID: p, Quantity: 1}}

	for i := 0; i < 2; i++ {
		if _, _, err := f.svc.CreateWithKey(ctx, f.alice, items, ""); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM orders`); n != 2 {
		t.Errorf("%d orders, want 2 (without a key, two requests are two orders)", n)
	}
}

// A request that FAILED stores nothing, so the client may retry the same
// key once the problem is gone.
func TestIdempotency_FailedAttemptCanBeRetried(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "IDEM-5", 1000, "INR", "ACTIVE", 0)
	items := []orders.ItemRequest{{ProductID: p, Quantity: 1}}

	if _, _, err := f.svc.CreateWithKey(ctx, f.alice, items, "k-retry"); !errors.Is(err, inventory.ErrOutOfStock) {
		t.Fatalf("first attempt: %v, want ErrOutOfStock", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE inventory SET available_quantity = 5 WHERE product_id = $1`, p); err != nil {
		t.Fatal(err)
	}
	o, replayed, err := f.svc.CreateWithKey(ctx, f.alice, items, "k-retry")
	if err != nil || replayed || o.Status != orders.StatusReserved {
		t.Errorf("retry after restock: %+v replayed=%v err=%v; want a NEW reserved order", o, replayed, err)
	}
}

// THE race: the client's HTTP library retries aggressively and 20 copies of
// the same request (same key) arrive at the same moment. All of them miss
// the fast-path lookup. The UNIQUE index must make exactly one INSERT win;
// the other 19 must roll back and return that same order.
func TestIdempotency_ConcurrentDuplicates(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "IDEM-RACE", 1000, "INR", "ACTIVE", 100)
	items := []orders.ItemRequest{{ProductID: p, Quantity: 2}}

	const n = 20
	var (
		mu       sync.Mutex
		orderIDs = map[int64]int{}
		created  atomic.Int64
		replays  atomic.Int64
		failures []error
	)
	race(n, func(int) {
		o, replayed, err := f.svc.CreateWithKey(ctx, f.alice, items, "same-key-for-all")
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failures = append(failures, err)
			return
		}
		orderIDs[o.ID]++
		if replayed {
			replays.Add(1)
		} else {
			created.Add(1)
		}
	})

	if len(failures) > 0 {
		t.Fatalf("%d requests failed, first: %v", len(failures), failures[0])
	}
	if len(orderIDs) != 1 || created.Load() != 1 || replays.Load() != n-1 {
		t.Errorf("order ids = %v, created = %d, replays = %d; want 1 id, 1 created, %d replays",
			orderIDs, created.Load(), replays.Load(), n-1)
	}
	if c := f.count(t, `SELECT count(*) FROM orders`); c != 1 {
		t.Errorf("%d orders in the database, want 1", c)
	}
	// Losers rolled back BEFORE touching stock: reserved exactly once.
	if s := f.stockOf(t, p); s != (stock{98, 2}) {
		t.Errorf("stock = %+v, want 98/2", s)
	}
	f.assertInvariant(t)
}

// Same race, but the product has exactly 1 unit. The first request takes
// it; the duplicates must NOT report OUT_OF_STOCK - they are the same
// purchase and must get the same successful order back.
func TestIdempotency_ConcurrentDuplicatesForLastUnit(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "IDEM-LAST", 1000, "INR", "ACTIVE", 1)
	items := []orders.ItemRequest{{ProductID: p, Quantity: 1}}

	var (
		mu       sync.Mutex
		orderIDs = map[int64]int{}
		failures []error
	)
	race(10, func(int) {
		o, _, err := f.svc.CreateWithKey(ctx, f.alice, items, "buy-the-last-one")
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failures = append(failures, err)
			return
		}
		orderIDs[o.ID]++
	})

	if len(failures) > 0 || len(orderIDs) != 1 {
		t.Errorf("order ids = %v, failures = %v; want all 10 to get the same order", orderIDs, failures)
	}
	if s := f.stockOf(t, p); s != (stock{0, 1}) {
		t.Errorf("stock = %+v, want 0/1", s)
	}
}

func TestIdempotency_DatabaseConstraints(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "IDEM-DB", 1000, "INR", "ACTIVE", 10)
	o, _, err := f.svc.CreateWithKey(ctx, f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 1}}, "db-key")
	if err != nil {
		t.Fatal(err)
	}

	bad := map[string]string{
		"key without hash": `UPDATE orders SET request_hash = NULL WHERE id = $1`,
		"empty key":        `UPDATE orders SET idempotency_key = '' WHERE id = $1`,
		"duplicate key": `INSERT INTO orders (user_id, total_amount, currency, idempotency_key, request_hash)
		                  SELECT user_id, 1, 'INR', idempotency_key, 'x' FROM orders WHERE id = $1`,
	}
	for name, sql := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := f.pool.Exec(ctx, sql, o.ID); err == nil {
				t.Error("statement succeeded, want constraint violation")
			}
		})
	}
}
