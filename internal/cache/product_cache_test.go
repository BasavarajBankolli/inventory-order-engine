package cache_test

import (
	"context"
	"testing"
	"time"

	"inventory-order-engine/internal/cache"
	"inventory-order-engine/internal/products"
	"inventory-order-engine/internal/testutil"
)

func TestProductCache_SetGetDelete(t *testing.T) {
	rdb, prefix := testutil.NewRedis(t)
	c := cache.NewProductCache(rdb, time.Minute, prefix)
	ctx := context.Background()

	if _, found, err := c.Get(ctx, 42); err != nil || found {
		t.Fatalf("empty cache: found=%v err=%v, want a clean miss", found, err)
	}

	want := products.Product{ID: 42, SKU: "MUG-01", Name: "Mug", Price: 29900, Currency: "INR", Status: products.StatusActive}
	if err := c.Set(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, found, err := c.Get(ctx, 42)
	if err != nil || !found || got.SKU != want.SKU || got.Price != want.Price {
		t.Fatalf("Get = %+v, %v, %v", got, found, err)
	}

	// Every entry has a TTL (the safety net against missed invalidations).
	ttl := rdb.TTL(ctx, prefix+"product:v1:42").Val()
	if ttl <= 0 || ttl > time.Minute {
		t.Errorf("TTL = %v, want (0, 1m]", ttl)
	}

	if err := c.Delete(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := c.Get(ctx, 42); found {
		t.Error("entry still cached after Delete")
	}
	if err := c.Delete(ctx, 42); err != nil {
		t.Errorf("deleting a missing key must not fail: %v", err)
	}
}

func TestProductCache_CorruptEntryIsAnError(t *testing.T) {
	rdb, prefix := testutil.NewRedis(t)
	c := cache.NewProductCache(rdb, time.Minute, prefix)
	rdb.Set(context.Background(), prefix+"product:v1:7", "{not json", time.Minute)

	if _, _, err := c.Get(context.Background(), 7); err == nil {
		t.Error("corrupt entry: err = nil, want a decode error (the service then falls back to the DB)")
	}
}

// Redis down: every call returns an error quickly instead of hanging.
func TestProductCache_RedisDown(t *testing.T) {
	c := cache.NewProductCache(testutil.NewBrokenRedis(t), time.Minute, "")
	ctx := context.Background()

	start := time.Now()
	if _, _, err := c.Get(ctx, 1); err == nil {
		t.Error("Get with Redis down: err = nil")
	}
	if err := c.Set(ctx, products.Product{ID: 1}); err == nil {
		t.Error("Set with Redis down: err = nil")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("calls took %v; the short client timeouts should fail fast", time.Since(start))
	}
}
