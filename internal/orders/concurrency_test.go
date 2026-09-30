package orders_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/orders"
)

// Concurrency tests (Stage 7). Every test:
//  1. prepares data,
//  2. starts N goroutines that all wait on a closed-channel "starting gun",
//  3. fires them at the same instant,
//  4. checks the outcome AND the database afterwards.
//
// Run them repeatedly to trust them:
//   go test -count=20 -run Concurrency ./internal/orders/

// outcome counts results by kind. Any error that is not expected is kept so
// the test can print it.
type outcome struct {
	ok, outOfStock, other atomic.Int64
	mu                    sync.Mutex
	unexpected            []error
}

func (o *outcome) record(err error) {
	switch {
	case err == nil:
		o.ok.Add(1)
	case errors.Is(err, inventory.ErrOutOfStock):
		o.outOfStock.Add(1)
	default:
		o.other.Add(1)
		o.mu.Lock()
		o.unexpected = append(o.unexpected, err)
		o.mu.Unlock()
	}
}

// errs summarises unexpected errors: how many, and the first one.
func (o *outcome) errs() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.unexpected) == 0 {
		return "none"
	}
	return fmt.Sprintf("%d, first: %v", len(o.unexpected), o.unexpected[0])
}

// race runs fn(i) for i in [0, n) in n goroutines released at the same moment.
func race(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fn(i)
		}()
	}
	close(start) // bang
	wg.Wait()
}

// customers creates n distinct customer accounts.
func (f fixture) customers(t *testing.T, n int) []identity.Principal {
	t.Helper()
	out := make([]identity.Principal, n)
	for i := range out {
		out[i] = identity.Principal{UserID: f.user(t, fmt.Sprintf("buyer%03d@x.com", i)), Role: identity.RoleCustomer}
	}
	return out
}

func (f fixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// THE demo from the project brief: stock = 1, 100 buyers at the same instant.
// Exactly one order may succeed; the other 99 must get OUT_OF_STOCK; stock
// must end at 0 available / 1 reserved, never -1.
func TestConcurrency_100BuyersOneItem(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "LAST-ONE", 99900, "INR", "ACTIVE", 1)
	buyers := f.customers(t, 100)

	var res outcome
	race(len(buyers), func(i int) {
		_, err := f.svc.Create(ctx, buyers[i], []orders.ItemRequest{{ProductID: p, Quantity: 1}})
		res.record(err)
	})

	if res.ok.Load() != 1 || res.outOfStock.Load() != 99 || res.other.Load() != 0 {
		t.Fatalf("ok=%d out_of_stock=%d other=%d (unexpected errors: %s); want 1, 99, 0",
			res.ok.Load(), res.outOfStock.Load(), res.other.Load(), res.errs())
	}
	if s := f.stockOf(t, p); s != (stock{0, 1}) {
		t.Errorf("stock = %+v, want 0 available / 1 reserved", s)
	}
	if n := f.count(t, `SELECT count(*) FROM orders`); n != 1 {
		t.Errorf("%d orders in the database, want exactly 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM inventory_reservations WHERE status = 'ACTIVE'`); n != 1 {
		t.Errorf("%d active reservations, want exactly 1", n)
	}
	f.assertInvariant(t)
}

// Stock 10, 100 buyers of 1 unit each: exactly 10 succeed.
func TestConcurrency_100BuyersTenItems(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "TEN-LEFT", 1000, "INR", "ACTIVE", 10)
	buyers := f.customers(t, 100)

	var res outcome
	race(len(buyers), func(i int) {
		_, err := f.svc.Create(ctx, buyers[i], []orders.ItemRequest{{ProductID: p, Quantity: 1}})
		res.record(err)
	})

	if res.ok.Load() != 10 || res.outOfStock.Load() != 90 || res.other.Load() != 0 {
		t.Fatalf("ok=%d out_of_stock=%d other=%d (unexpected errors: %s); want 10, 90, 0",
			res.ok.Load(), res.outOfStock.Load(), res.other.Load(), res.errs())
	}
	if s := f.stockOf(t, p); s != (stock{0, 10}) {
		t.Errorf("stock = %+v, want 0/10", s)
	}
	f.assertInvariant(t)
}

// Varying quantities: stock 50, 40 buyers asking for 1..4 units. However
// the race turns out, the reserved total must equal the successful demand,
// and available + reserved must still be 50.
func TestConcurrency_MixedQuantities(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "MIXED", 1000, "INR", "ACTIVE", 50)
	buyers := f.customers(t, 40)

	var reservedByWinners atomic.Int64
	var res outcome
	race(len(buyers), func(i int) {
		qty := i%4 + 1
		_, err := f.svc.Create(ctx, buyers[i], []orders.ItemRequest{{ProductID: p, Quantity: qty}})
		res.record(err)
		if err == nil {
			reservedByWinners.Add(int64(qty))
		}
	})

	if res.other.Load() != 0 {
		t.Fatalf("unexpected errors: %s", res.errs())
	}
	s := f.stockOf(t, p)
	if s.available < 0 || s.available+s.reserved != 50 || int64(s.reserved) != reservedByWinners.Load() {
		t.Errorf("stock = %+v, winners reserved %d; want available >= 0, total 50, reserved == winners",
			s, reservedByWinners.Load())
	}
	f.assertInvariant(t)
}

// Half the orders list products as [A, B], the other half as [B, A]. If rows
// were locked in request order, two such transactions could each hold one
// lock and wait for the other: a deadlock (PostgreSQL error 40P01). Because
// ReserveForOrder locks in product-id order, every order must succeed.
func TestConcurrency_NoDeadlockWithOppositeItemOrder(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a := f.productWithStock(t, "DL-A", 100, "INR", "ACTIVE", 1000)
	b := f.productWithStock(t, "DL-B", 100, "INR", "ACTIVE", 1000)
	c := f.productWithStock(t, "DL-C", 100, "INR", "ACTIVE", 1000)
	buyers := f.customers(t, 60)

	var res outcome
	race(len(buyers), func(i int) {
		var items []orders.ItemRequest
		switch i % 3 {
		case 0:
			items = []orders.ItemRequest{{ProductID: a, Quantity: 1}, {ProductID: b, Quantity: 1}, {ProductID: c, Quantity: 1}}
		case 1:
			items = []orders.ItemRequest{{ProductID: c, Quantity: 1}, {ProductID: b, Quantity: 1}, {ProductID: a, Quantity: 1}}
		default:
			items = []orders.ItemRequest{{ProductID: b, Quantity: 1}, {ProductID: a, Quantity: 1}, {ProductID: c, Quantity: 1}}
		}
		_, err := f.svc.Create(ctx, buyers[i], items)
		res.record(err)
	})

	if res.ok.Load() != 60 {
		t.Fatalf("ok=%d, want all 60 (unexpected errors: %s)", res.ok.Load(), res.errs())
	}
	for _, p := range []int64{a, b, c} {
		if s := f.stockOf(t, p); s != (stock{940, 60}) {
			t.Errorf("product %d stock = %+v, want 940/60", p, s)
		}
	}
	f.assertInvariant(t)
}

// Creates and cancels running at the same time on a small stock. Units are
// only ever MOVED between available and reserved, so their sum must stay 20
// and the reservation invariant must hold at the end.
func TestConcurrency_CreateAndCancelStorm(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "STORM", 100, "INR", "ACTIVE", 20)
	buyers := f.customers(t, 80)

	var res outcome
	var cancelErrors atomic.Int64
	race(len(buyers), func(i int) {
		o, err := f.svc.Create(ctx, buyers[i], []orders.ItemRequest{{ProductID: p, Quantity: 1 + i%3}})
		res.record(err)
		if err == nil && i%2 == 0 {
			// Half of the winners change their mind immediately, freeing
			// stock that later buyers in the race may still grab.
			if _, err := f.svc.Cancel(ctx, buyers[i], o.ID); err != nil {
				cancelErrors.Add(1)
			}
		}
	})

	if res.other.Load() != 0 || cancelErrors.Load() != 0 {
		t.Fatalf("unexpected errors: create=%s cancel=%d", res.errs(), cancelErrors.Load())
	}
	s := f.stockOf(t, p)
	if s.available < 0 || s.reserved < 0 || s.available+s.reserved != 20 {
		t.Errorf("stock = %+v, want non-negative and summing to 20", s)
	}
	f.assertInvariant(t)
}

// Admin archives a product while orders for it are being placed. Every
// order must either succeed completely (placed before the archive) or be
// rejected; the archive itself must not fail.
func TestConcurrency_ArchiveDuringOrders(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.productWithStock(t, "ARCH", 100, "INR", "ACTIVE", 1000)
	buyers := f.customers(t, 30)

	var archiveErr error
	var res outcome
	var rejected atomic.Int64
	race(len(buyers)+1, func(i int) {
		if i == len(buyers) {
			_, archiveErr = f.pool.Exec(ctx, `UPDATE products SET status = 'ARCHIVED' WHERE id = $1`, p)
			return
		}
		_, err := f.svc.Create(ctx, buyers[i], []orders.ItemRequest{{ProductID: p, Quantity: 1}})
		if err != nil && !errors.Is(err, inventory.ErrOutOfStock) {
			rejected.Add(1) // "product does not exist" after the archive: expected
			return
		}
		res.record(err)
	})

	if archiveErr != nil {
		t.Fatalf("archive failed: %v", archiveErr)
	}
	if res.ok.Load()+rejected.Load() != 30 {
		t.Errorf("ok=%d rejected=%d, want them to add up to 30", res.ok.Load(), rejected.Load())
	}
	// Every successful order has its stock reserved - nothing half-done.
	if s := f.stockOf(t, p); int64(s.reserved) != res.ok.Load() || s.available+s.reserved != 1000 {
		t.Errorf("stock = %+v with %d successful orders", s, res.ok.Load())
	}
	f.assertInvariant(t)
}
