package testutil

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"inventory-order-engine/internal/requestid"
)

// newClient mirrors cache.NewRedisClient. testutil must not import the
// cache package: cache imports products, and products' own tests import
// testutil - that would be an import cycle.
func newClient(t *testing.T, url string) *redis.Client {
	t.Helper()
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	opts.DialTimeout = time.Second
	opts.ReadTimeout = 300 * time.Millisecond
	opts.WriteTimeout = 300 * time.Millisecond
	opts.MaxRetries = 1
	return redis.NewClient(opts)
}

// NewRedis connects to TEST_REDIS_URL (skipping the test if it is unset)
// and returns the client plus a unique key prefix for this test. All keys
// with that prefix are deleted when the test ends, so tests running in
// parallel never see each other's cache entries or rate-limit counters.
func NewRedis(t *testing.T) (*redis.Client, string) {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; skipping Redis test")
	}
	rdb := newClient(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping test redis: %v", err)
	}

	prefix := "test:" + requestid.New()[:12] + ":"
	t.Cleanup(func() {
		ctx := context.Background()
		iter := rdb.Scan(ctx, 0, prefix+"*", 100).Iterator()
		for iter.Next(ctx) {
			rdb.Del(ctx, iter.Val())
		}
		rdb.Close()
	})
	return rdb, prefix
}

// NewBrokenRedis returns a client pointing at a port where nothing listens,
// to simulate "Redis is down". Every command fails quickly.
func NewBrokenRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := newClient(t, "redis://127.0.0.1:1/0")
	t.Cleanup(func() { rdb.Close() })
	return rdb
}
