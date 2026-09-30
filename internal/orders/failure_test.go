package orders_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/orders"
)

// Failures IN THE MIDDLE of a transaction. Whatever happens, the database
// must be left exactly as before: no order without a reservation, no
// reserved stock without an order, no locks left behind.

// lockHolder is a transaction holding a product's inventory row lock, so
// an order for that product blocks on it.
type lockHolder struct {
	tx  pgx.Tx
	pid int // the holder's backend process id
}

func (f fixture) lockInventoryRow(t *testing.T, productID int64) lockHolder {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var h lockHolder
	h.tx = tx
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&h.pid); err != nil {
		t.Fatal(err)
	}
	// Always release the lock, even if the test fails half-way: otherwise
	// the pool's cleanup would wait forever for this connection (a hung
	// test run - which is exactly what happened while writing this test).
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })

	if _, err := tx.Exec(ctx, `SELECT 1 FROM inventory WHERE product_id = $1 FOR UPDATE`, productID); err != nil {
		t.Fatal(err)
	}
	return h
}

// blockedBy returns the pids of sessions waiting for h's locks.
//
// pg_blocking_pids(pid) lists who is blocking a session. Asking "whose
// blockers include OUR holder?" finds exactly our order transaction - and
// never a session of another test package running in parallel on the same
// test database (killing that would break an unrelated test).
func (f fixture) blockedBy(t *testing.T, h lockHolder) []int {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT pid FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, h.pid)
	if err != nil {
		t.Fatal(err)
	}
	pids, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatal(err)
	}
	return pids
}

// waitForBlockedSession returns the pid of the session blocked by h (our
// order transaction), or fails after a few seconds.
func (f fixture) waitForBlockedSession(t *testing.T, h lockHolder) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pids := f.blockedBy(t, h); len(pids) > 0 {
			return pids[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the order transaction never blocked on the lock")
	return 0
}

func (f fixture) assertNothingWritten(t *testing.T, productID int64, wantStock stock) {
	t.Helper()
	if o, i := f.countOrders(t); o != 0 || i != 0 {
		t.Errorf("%d orders / %d items left behind", o, i)
	}
	if n := f.count(t, `SELECT count(*) FROM inventory_reservations`); n != 0 {
		t.Errorf("%d reservations left behind", n)
	}
	if n := f.count(t, `SELECT count(*) FROM outbox_events`); n != 0 {
		t.Errorf("%d outbox events left behind", n)
	}
	if s := f.stockOf(t, productID); s != wantStock {
		t.Errorf("stock = %+v, want %+v", s, wantStock)
	}
}

// The database connection DIES while the order transaction is running
// (server crash, network cut, admin kills the session). PostgreSQL rolls
// the transaction back; the order row it had already inserted disappears.
func TestFailure_ConnectionKilledMidTransaction(t *testing.T) {
	f := setup(t)
	p := f.productWithStock(t, "KILL-1", 1000, "INR", "ACTIVE", 10)
	holder := f.lockInventoryRow(t, p)

	result := make(chan error, 1)
	go func() {
		_, err := f.svc.Create(context.Background(), f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 3}})
		result <- err
	}()

	// The order transaction has inserted its order row and is now waiting
	// for the inventory lock. Kill its connection.
	pid := f.waitForBlockedSession(t, holder)
	if _, err := f.pool.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}

	err := <-result
	if err == nil || errors.Is(err, inventory.ErrOutOfStock) {
		t.Fatalf("Create() error = %v, want a database error", err)
	}
	holder.tx.Rollback(context.Background())
	f.assertNothingWritten(t, p, stock{10, 0})

	// The pool threw the dead connection away: the next order just works.
	if _, err := f.svc.Create(context.Background(), f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 3}}); err != nil {
		t.Errorf("order after the failure: %v", err)
	}
}

// The client gives up (HTTP request cancelled / timed out) while the order
// transaction waits for a lock. The request's context is cancelled, pgx
// aborts the query, the transaction rolls back, and its locks are released.
func TestFailure_ClientCancelsWhileWaiting(t *testing.T) {
	f := setup(t)
	p := f.productWithStock(t, "CANCEL-1", 1000, "INR", "ACTIVE", 10)
	holder := f.lockInventoryRow(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := f.svc.Create(ctx, f.alice, []orders.ItemRequest{{ProductID: p, Quantity: 2}})
		result <- err
	}()
	f.waitForBlockedSession(t, holder)
	cancel()

	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Create() error = %v, want context.Canceled", err)
	}
	// The SERVER must stop waiting too (not just our Go code). The cancel
	// request is asynchronous, so allow it a moment to arrive.
	deadline := time.Now().Add(2 * time.Second)
	for len(f.blockedBy(t, holder)) > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if pids := f.blockedBy(t, holder); len(pids) != 0 {
		t.Errorf("server sessions %v are still waiting for the lock after the client gave up", pids)
	}
	holder.tx.Rollback(context.Background())
	f.assertNothingWritten(t, p, stock{10, 0})
}

// A statement fails half-way through a multi-step transaction (here: the
// data is inconsistent, so releasing stock hits the "never negative" rule).
// Everything the transaction already did - the status change - is undone.
func TestFailure_ErrorHalfwayRollsBackEarlierSteps(t *testing.T) {
	f := setup(t)
	o, p := f.placeOrder(t, "HALF-1", 10, 4) // stock 6 / 4 reserved

	// Corrupt the data behind the service's back: reserved says 0, but an
	// ACTIVE reservation of 4 exists.
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE inventory SET reserved_quantity = 0 WHERE product_id = $1`, p); err != nil {
		t.Fatal(err)
	}

	// Cancel: step 1 (RESERVED -> CANCELLED) succeeds inside the
	// transaction, step 2 (release) fails.
	_, err := f.svc.Cancel(context.Background(), f.alice, o.ID)
	if !errors.Is(err, inventory.ErrInconsistentReservation) {
		t.Fatalf("Cancel() error = %v, want ErrInconsistentReservation", err)
	}

	// Step 1 was rolled back too: the order is still RESERVED, no event.
	if s := f.status(t, o.ID); s != orders.StatusReserved {
		t.Errorf("order status = %s, want RESERVED (the status change must be rolled back)", s)
	}
	if evs := f.eventsFor(t, o.ID); types(evs) != "[OrderCreated InventoryReserved]" {
		t.Errorf("events = %s, want no OrderCancelled", types(evs))
	}
	if r := f.reservationStatuses(t, o.ID); r["ACTIVE"] != 1 {
		t.Errorf("reservations = %v, want still ACTIVE", r)
	}
}
