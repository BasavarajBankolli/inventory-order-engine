package tests

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"inventory-order-engine/internal/app"
	"inventory-order-engine/internal/cache"
	"inventory-order-engine/internal/config"
	"inventory-order-engine/internal/testutil"
)

// End-to-end tests for Stage 11 (need TEST_DATABASE_URL and TEST_REDIS_URL).

func newRedisAPI(t *testing.T, ratePerMinute int) *testAPI {
	t.Helper()
	rdb, prefix := testutil.NewRedis(t)
	return newTestAPIWith(t, func(cfg *config.Config, opts *app.Options) {
		cfg.RateLimitPerMinute = ratePerMinute
		opts.Redis = rdb
		opts.RedisKeyPrefix = prefix
	})
}

func TestRedisAPI_ProductCacheIsInvalidatedOnUpdate(t *testing.T) {
	api := newRedisAPI(t, 0) // rate limiting off for this test
	admin := api.loginAs("admin@example.com", true)
	id := api.createProduct(admin, "CACHED-1", 1000)
	path := "/api/v1/products/" + strconv.FormatInt(id, 10)

	var p productResponse
	api.do("GET", path, nil, "", &p) // miss -> cached
	api.do("GET", path, nil, "", &p) // hit

	expectStatus(t, api.do("PATCH", path, map[string]any{"price": 2500}, admin, nil), http.StatusOK)
	api.do("GET", path, nil, "", &p)
	if p.Price != 2500 {
		t.Errorf("price after PATCH = %d, want 2500 (stale cache served)", p.Price)
	}

	expectStatus(t, api.do("DELETE", path, nil, admin, nil), http.StatusNoContent)
	expectStatus(t, api.do("GET", path, nil, "", nil), http.StatusNotFound)
}

func TestRedisAPI_RateLimitPerIPOnPublicRoutes(t *testing.T) {
	api := newRedisAPI(t, 5)

	for i := 1; i <= 5; i++ {
		resp := api.do("GET", "/api/v1/products", nil, "", nil)
		expectStatus(t, resp, http.StatusOK)
		if got := resp.Header.Get("X-RateLimit-Remaining"); got != strconv.Itoa(5-i) {
			t.Errorf("request %d: remaining = %q, want %d", i, got, 5-i)
		}
	}

	var body errorResponse
	resp := api.do("GET", "/api/v1/products", nil, "", &body)
	expectStatus(t, resp, http.StatusTooManyRequests)
	if body.Error.Code != "RATE_LIMITED" || resp.Header.Get("Retry-After") == "" {
		t.Errorf("429 response: code=%q Retry-After=%q", body.Error.Code, resp.Header.Get("Retry-After"))
	}

	// Health checks are never rate limited.
	expectStatus(t, api.do("GET", "/health", nil, "", nil), http.StatusOK)
	expectStatus(t, api.do("GET", "/ready", nil, "", nil), http.StatusOK)
}

// Authenticated routes are limited per USER: one user hitting the limit
// does not block another user.
func TestRedisAPI_RateLimitPerUser(t *testing.T) {
	// Logging in uses the (per-IP) public budget, so give it enough room.
	api := newRedisAPI(t, 8)
	alice := api.loginAs("alice@example.com", false)
	bob := api.loginAs("bob@example.com", false)

	for i := 0; i < 8; i++ {
		expectStatus(t, api.do("GET", "/api/v1/users/me", nil, alice, nil), http.StatusOK)
	}
	expectStatus(t, api.do("GET", "/api/v1/users/me", nil, alice, nil), http.StatusTooManyRequests)
	expectStatus(t, api.do("GET", "/api/v1/users/me", nil, bob, nil), http.StatusOK)
}

// Redis down: the whole API still works, FAST; /ready reports "degraded".
//
// We use the PRODUCTION client (with its circuit breaker) pointed at a
// non-routable address, where connecting hangs instead of being refused -
// the situation that made requests take 65-98 s before the breaker existed.
func TestRedisAPI_WorksWhenRedisIsDown(t *testing.T) {
	rdb, err := cache.NewRedisClient("redis://10.255.255.1:6379/0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.Close() })

	api := newTestAPIWith(t, func(cfg *config.Config, opts *app.Options) {
		cfg.RateLimitPerMinute = 1 // would block the 2nd request - if Redis worked
		opts.Redis = rdb
	})

	var ready struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	expectStatus(t, api.do("GET", "/ready", nil, "", &ready), http.StatusOK)
	if ready.Status != "degraded" || ready.Checks["redis"] != "unavailable" || ready.Checks["postgres"] != "ok" {
		t.Errorf("/ready = %+v, want degraded with redis unavailable", ready)
	}

	admin := api.loginAs("admin@example.com", true)
	id := api.createProduct(admin, "NO-REDIS", 1000)

	start := time.Now()
	for i := 0; i < 10; i++ { // fail open: no 429 even though the limit is 1
		expectStatus(t, api.do("GET", "/api/v1/products/"+strconv.FormatInt(id, 10), nil, "", nil), http.StatusOK)
	}
	// The breaker opened during setup, so these requests never wait for Redis.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("10 requests took %v with Redis down; they must not wait for it", elapsed)
	}
}
