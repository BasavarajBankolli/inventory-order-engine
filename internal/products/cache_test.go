package products_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"inventory-order-engine/internal/cache"
	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/products"
	"inventory-order-engine/internal/testutil"
)

// Cache-aside behaviour of products.Service with real PostgreSQL + Redis.

func newCachedService(t *testing.T, rdb *redis.Client, prefix string) (*products.Service, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.NewMigratedPool(t)
	svc := products.NewService(pool, products.NewRepository(pool), inventory.NewRepository(pool),
		cache.NewProductCache(rdb, time.Minute, prefix))
	return svc, pool
}

func create(t *testing.T, svc *products.Service) products.Product {
	t.Helper()
	p, err := svc.Create(context.Background(), products.CreateInput{SKU: "CACHE-1", Name: "Cached", Price: 1000, Currency: "INR"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCache_ReadThroughAndInvalidation(t *testing.T) {
	rdb, prefix := testutil.NewRedis(t)
	svc, pool := newCachedService(t, rdb, prefix)
	ctx := context.Background()
	p := create(t, svc)

	// 1st Get: miss -> PostgreSQL -> stored in Redis.
	if _, err := svc.Get(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if rdb.Exists(ctx, prefix+"product:v1:"+strconv.FormatInt(p.ID, 10)).Val() != 1 {
		t.Fatal("product was not written to the cache")
	}

	// Prove the 2nd Get comes from Redis: change the row BEHIND the
	// service's back. The cached (now stale) value is still returned.
	if _, err := pool.Exec(ctx, `UPDATE products SET name = 'changed in SQL' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(ctx, p.ID)
	if got.Name != "Cached" {
		t.Fatalf("name = %q, want the cached value (served without a DB query)", got.Name)
	}

	// Updating through the service invalidates the entry: fresh data.
	newName := "Updated via API"
	if _, err := svc.Update(ctx, p.ID, products.UpdateInput{Name: &newName}); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.Get(ctx, p.ID)
	if got.Name != "Updated via API" {
		t.Errorf("after Update: name = %q, want the new value", got.Name)
	}

	// Archiving invalidates too: the product must disappear at once.
	if err := svc.Archive(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, p.ID); !errors.Is(err, products.ErrNotFound) {
		t.Errorf("after Archive: %v, want ErrNotFound (not a stale cached product)", err)
	}
}

func TestCache_NotFoundIsNotCached(t *testing.T) {
	rdb, prefix := testutil.NewRedis(t)
	svc, _ := newCachedService(t, rdb, prefix)

	if _, err := svc.Get(context.Background(), 999999); !errors.Is(err, products.ErrNotFound) {
		t.Fatal(err)
	}
	if n := rdb.Exists(context.Background(), prefix+"product:v1:999999").Val(); n != 0 {
		t.Error("a not-found result was cached")
	}
}

// Redis down: reads, writes and invalidations all still work, straight from
// PostgreSQL. The cache is an optimisation, never a dependency.
func TestCache_RedisDownFallsBackToDatabase(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	svc := products.NewService(pool, products.NewRepository(pool), inventory.NewRepository(pool),
		cache.NewProductCache(testutil.NewBrokenRedis(t), time.Minute, ""))
	ctx := context.Background()
	p := create(t, svc)

	got, err := svc.Get(ctx, p.ID)
	if err != nil || got.ID != p.ID {
		t.Fatalf("Get with Redis down = %+v, %v", got, err)
	}
	name := "still editable"
	if _, err := svc.Update(ctx, p.ID, products.UpdateInput{Name: &name}); err != nil {
		t.Errorf("Update with Redis down: %v", err)
	}
	if got, _ := svc.Get(ctx, p.ID); got.Name != name {
		t.Errorf("name = %q, want %q", got.Name, name)
	}
}
