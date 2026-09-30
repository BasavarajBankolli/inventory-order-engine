package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"inventory-order-engine/internal/products"
)

// ProductCache stores products in Redis as JSON.
//
//	key:   <prefix>product:v1:<id>        e.g. "product:v1:42"
//	value: {"id":42,"sku":"MOUSE-01",...} (the same JSON the API returns)
//	TTL:   PRODUCT_CACHE_TTL (5 minutes by default)
//
// The "v1" in the key is a schema version: if the Product struct ever
// changes shape, bumping it to "v2" makes every old entry simply unused
// (and expire), instead of being decoded into the wrong shape.
//
// It implements products.Cache.
type ProductCache struct {
	rdb    *redis.Client
	ttl    time.Duration
	prefix string
}

// NewProductCache creates a ProductCache. prefix lets tests (and several
// apps sharing one Redis) keep their keys apart; production uses "".
func NewProductCache(rdb *redis.Client, ttl time.Duration, prefix string) *ProductCache {
	return &ProductCache{rdb: rdb, ttl: ttl, prefix: prefix}
}

func (c *ProductCache) key(id int64) string {
	return fmt.Sprintf("%sproduct:v1:%d", c.prefix, id)
}

// Get returns the cached product. found is false on a cache miss.
func (c *ProductCache) Get(ctx context.Context, id int64) (p products.Product, found bool, err error) {
	data, err := c.rdb.Get(ctx, c.key(id)).Bytes()
	if errors.Is(err, redis.Nil) { // redis.Nil = "no such key", i.e. a normal miss
		return products.Product{}, false, nil
	}
	if err != nil {
		return products.Product{}, false, fmt.Errorf("redis get: %w", err)
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return products.Product{}, false, fmt.Errorf("decode cached product: %w", err)
	}
	return p, true, nil
}

// Set stores a product with the configured TTL.
//
// The TTL is a safety net: even if an invalidation is ever missed, a
// stale entry disappears on its own after a few minutes.
func (c *ProductCache) Set(ctx context.Context, p products.Product) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, c.key(p.ID), data, c.ttl).Err()
}

// Delete removes a product from the cache (invalidation). Deleting a key
// that does not exist is not an error.
func (c *ProductCache) Delete(ctx context.Context, id int64) error {
	return c.rdb.Del(ctx, c.key(id)).Err()
}
