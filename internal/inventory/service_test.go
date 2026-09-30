package inventory_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/testutil"
	"inventory-order-engine/migrations"
)

// Integration tests against real PostgreSQL (skipped without TEST_DATABASE_URL).
// The concurrency tests are the important ones: they start many goroutines
// that hit the SAME inventory row at the same moment.

func ptr[T any](v T) *T { return &v }

// newProduct inserts a product and its inventory row with raw SQL and
// returns the product id. (Using SQL keeps this package's tests independent
// of the products package.)
func newProduct(t *testing.T, pool *pgxpool.Pool, sku string, available int) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO products (sku, name, price, currency) VALUES ($1, 'Test', 100, 'INR') RETURNING id`, sku).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO inventory (product_id, available_quantity) VALUES ($1, $2)`, id, available); err != nil {
		t.Fatal(err)
	}
	return id
}

func newService(t *testing.T) (*inventory.Service, *pgxpool.Pool) {
	pool := testutil.NewMigratedPool(t)
	return inventory.NewService(pool, inventory.NewRepository(pool)), pool
}

func TestService_AdjustAndSet(t *testing.T) {
	svc, pool := newService(t)
	ctx := context.Background()
	id := newProduct(t, pool, "P-1", 10)

	inv, err := svc.Update(ctx, id, inventory.UpdateInput{Adjustment: ptr(5)})
	if err != nil || inv.AvailableQuantity != 15 || inv.Version != 2 {
		t.Fatalf("adjust +5: %+v, %v; want available 15, version 2", inv, err)
	}

	inv, err = svc.Update(ctx, id, inventory.UpdateInput{AvailableQuantity: ptr(40), Version: ptr[int64](2)})
	if err != nil || inv.AvailableQuantity != 40 || inv.Version != 3 {
		t.Fatalf("set 40 @v2: %+v, %v; want available 40, version 3", inv, err)
	}

	// Stale version: the client read version 2, but it is now 3.
	_, err = svc.Update(ctx, id, inventory.UpdateInput{AvailableQuantity: ptr(1), Version: ptr[int64](2)})
	if !errors.Is(err, inventory.ErrVersionConflict) {
		t.Errorf("stale version error = %v, want ErrVersionConflict", err)
	}

	_, err = svc.Update(ctx, id, inventory.UpdateInput{Adjustment: ptr(-41)})
	if !errors.Is(err, inventory.ErrInsufficientStock) {
		t.Errorf("over-removal error = %v, want ErrInsufficientStock", err)
	}

	// Failed updates changed nothing.
	got, _ := svc.Get(ctx, id)
	if got.AvailableQuantity != 40 || got.Version != 3 {
		t.Errorf("after failures: %+v, want available 40, version 3", got)
	}
}

func TestService_NotFoundAndArchived(t *testing.T) {
	svc, pool := newService(t)
	ctx := context.Background()

	if _, err := svc.Get(ctx, 999999); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("missing product: %v, want ErrNotFound", err)
	}

	id := newProduct(t, pool, "P-GONE", 3)
	if _, err := pool.Exec(ctx, `UPDATE products SET status = 'ARCHIVED' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, id); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("archived Get: %v, want ErrNotFound", err)
	}
	if _, err := svc.Update(ctx, id, inventory.UpdateInput{Adjustment: ptr(1)}); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("archived Update: %v, want ErrNotFound", err)
	}
}

// runConcurrently starts n goroutines, releases them at the same instant
// and waits for all of them. It returns each goroutine's error.
func runConcurrently(n int, fn func(i int) error) []error {
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = fn(i)
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

// 50 concurrent "+1" restocks must give exactly +50. Without the row lock,
// two transactions could both read 0, both write 1, and one restock would
// be silently lost (the "lost update" problem).
func TestConcurrency_NoLostUpdates(t *testing.T) {
	svc, pool := newService(t)
	ctx := context.Background()
	id := newProduct(t, pool, "P-RACE", 0)

	const n = 50
	errs := runConcurrently(n, func(int) error {
		_, err := svc.Update(ctx, id, inventory.UpdateInput{Adjustment: ptr(1)})
		return err
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	inv, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inv.AvailableQuantity != n || inv.Version != n+1 {
		t.Errorf("available = %d, version = %d; want %d and %d", inv.AvailableQuantity, inv.Version, n, n+1)
	}
}

// Stock 5, 20 concurrent "-1": exactly 5 succeed, 15 get ErrInsufficientStock,
// and stock ends at 0 - never negative.
func TestConcurrency_NeverNegative(t *testing.T) {
	svc, pool := newService(t)
	ctx := context.Background()
	id := newProduct(t, pool, "P-SCARCE", 5)

	errs := runConcurrently(20, func(int) error {
		_, err := svc.Update(ctx, id, inventory.UpdateInput{Adjustment: ptr(-1)})
		return err
	})

	ok, insufficient := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, inventory.ErrInsufficientStock):
			insufficient++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	inv, _ := svc.Get(ctx, id)
	if ok != 5 || insufficient != 15 || inv.AvailableQuantity != 0 {
		t.Errorf("ok=%d insufficient=%d available=%d; want 5, 15, 0", ok, insufficient, inv.AvailableQuantity)
	}
}

// Two admins both read version 1 and both try to overwrite the count.
// Optimistic locking lets exactly one win; the other must re-read.
func TestConcurrency_OptimisticLocking(t *testing.T) {
	svc, pool := newService(t)
	ctx := context.Background()
	id := newProduct(t, pool, "P-COUNT", 10)

	errs := runConcurrently(10, func(i int) error {
		_, err := svc.Update(ctx, id, inventory.UpdateInput{AvailableQuantity: ptr(100 + i), Version: ptr[int64](1)})
		return err
	})

	wins, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, inventory.ErrVersionConflict):
			conflicts++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != 9 {
		t.Errorf("wins=%d conflicts=%d; want 1 and 9", wins, conflicts)
	}
}

// Even raw SQL that bypasses all Go code cannot store negative stock.
func TestDatabase_CheckConstraintBlocksNegativeStock(t *testing.T) {
	_, pool := newService(t)
	id := newProduct(t, pool, "P-CHECK", 1)

	_, err := pool.Exec(context.Background(), `UPDATE inventory SET available_quantity = -1 WHERE product_id = $1`, id)
	if err == nil {
		t.Fatal("UPDATE to -1 succeeded; the CHECK constraint is missing")
	}
	_, err = pool.Exec(context.Background(), `UPDATE inventory SET reserved_quantity = -1 WHERE product_id = $1`, id)
	if err == nil {
		t.Fatal("reserved -1 succeeded; the CHECK constraint is missing")
	}
}

// Products that existed before migration 000004 get an inventory row.
func TestMigration_BackfillsExistingProducts(t *testing.T) {
	pool := testutil.NewIsolatedPool(t)
	ctx := context.Background()

	// Apply migrations 1-3 only.
	upTo3 := fstest.MapFS{}
	for _, name := range []string{"000001_init.sql", "000002_create_users.sql", "000003_create_products.sql"} {
		data, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		upTo3[name] = &fstest.MapFile{Data: data}
	}
	if _, err := database.Migrate(ctx, pool, upTo3); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO products (sku, name, price, currency) VALUES ('OLD-1', 'A', 1, 'INR'), ('OLD-2', 'B', 1, 'INR')`); err != nil {
		t.Fatal(err)
	}

	// Now apply the rest, including 000004.
	if _, err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}

	var rows, zeroRows int
	err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE available_quantity = 0 AND reserved_quantity = 0) FROM inventory`).Scan(&rows, &zeroRows)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 2 || zeroRows != 2 {
		t.Errorf("inventory rows = %d (zero: %d), want 2 empty rows", rows, zeroRows)
	}
}
