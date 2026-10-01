package ratelimit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/metrics"
	"inventory-order-engine/internal/testutil"
)

// Tests against a real Redis (TEST_REDIS_URL), plus fail-open tests
// against a Redis that is down.

// fixedClock pins the limiter's clock to one moment in the middle of a window.
func fixedClock(l *Limiter, t time.Time) { l.now = func() time.Time { return t } }

func TestAllow_CountsUpToTheLimit(t *testing.T) {
	rdb, prefix := testutil.NewRedis(t)
	l := NewLimiter(rdb, 3, time.Minute, prefix)
	fixedClock(l, time.Date(2026, 9, 30, 12, 3, 20, 0, time.UTC))
	ctx := context.Background()

	for i, wantRemaining := range []int{2, 1, 0} {
		d, err := l.Allow(ctx, "user:1")
		if err != nil || !d.Allowed || d.Remaining != wantRemaining {
			t.Fatalf("request %d: %+v, %v", i+1, d, err)
		}
	}

	d, _ := l.Allow(ctx, "user:1")
	if d.Allowed || d.Remaining != 0 {
		t.Errorf("4th request: %+v, want denied", d)
	}
	// 12:03:20 -> window ends at 12:04:00 -> retry after 40s.
	if d.RetryAfter != 40*time.Second {
		t.Errorf("RetryAfter = %v, want 40s", d.RetryAfter)
	}

	// Other subjects have their own budget.
	if d, _ := l.Allow(ctx, "user:2"); !d.Allowed {
		t.Error("user:2 was limited by user:1's requests")
	}
}

func TestAllow_NewWindowResetsTheCount(t *testing.T) {
	rdb, prefix := testutil.NewRedis(t)
	l := NewLimiter(rdb, 1, time.Minute, prefix)
	ctx := context.Background()

	fixedClock(l, time.Date(2026, 9, 30, 12, 3, 59, 0, time.UTC))
	l.Allow(ctx, "ip:1.2.3.4")
	if d, _ := l.Allow(ctx, "ip:1.2.3.4"); d.Allowed {
		t.Fatal("2nd request in the same window was allowed")
	}

	fixedClock(l, time.Date(2026, 9, 30, 12, 4, 0, 0, time.UTC)) // next minute
	if d, _ := l.Allow(ctx, "ip:1.2.3.4"); !d.Allowed {
		t.Error("first request of a new window was denied")
	}
}

func TestAllow_CounterKeysExpire(t *testing.T) {
	rdb, prefix := testutil.NewRedis(t)
	l := NewLimiter(rdb, 10, time.Minute, prefix)
	l.Allow(context.Background(), "user:9")

	keys, _ := rdb.Keys(context.Background(), prefix+"rl:user:9:*").Result()
	if len(keys) != 1 {
		t.Fatalf("keys = %v", keys)
	}
	if ttl := rdb.TTL(context.Background(), keys[0]).Val(); ttl <= 0 || ttl > 61*time.Second {
		t.Errorf("TTL = %v, want about one window (old counters must clean themselves up)", ttl)
	}
}

// 200 requests at the same instant with a limit of 100: Redis INCR is
// atomic, so exactly 100 are allowed - no lost increments.
func TestAllow_ConcurrentRequestsAreCountedExactly(t *testing.T) {
	rdb, prefix := testutil.NewRedis(t)
	l := NewLimiter(rdb, 100, time.Minute, prefix)
	fixedClock(l, time.Date(2026, 9, 30, 12, 0, 30, 0, time.UTC))

	var allowed, failed atomic.Int64
	var firstErr atomic.Value
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d, err := l.Allow(context.Background(), "user:hot")
			switch {
			case err != nil:
				failed.Add(1)
				firstErr.CompareAndSwap(nil, err.Error())
			case d.Allowed:
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if failed.Load() > 0 {
		t.Fatalf("%d calls failed (first: %v) - the counter result is meaningless", failed.Load(), firstErr.Load())
	}
	if allowed.Load() != 100 {
		t.Errorf("allowed = %d, want exactly 100", allowed.Load())
	}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestMiddleware_Returns429WithHeaders(t *testing.T) {
	rdb, prefix := testutil.NewRedis(t)
	m := metrics.New()
	h := Middleware(NewLimiter(rdb, 2, time.Minute, prefix), ByIP(0), m)(okHandler())

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.9:51000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := send(); rec.Code != 200 || rec.Header().Get("X-RateLimit-Remaining") != "1" {
		t.Fatalf("1st: %d remaining=%s", rec.Code, rec.Header().Get("X-RateLimit-Remaining"))
	}
	send()
	rec := send()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd: status %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" || rec.Header().Get("X-RateLimit-Limit") != "2" {
		t.Errorf("429 headers = %v", rec.Header())
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != httpx.CodeRateLimited {
		t.Errorf("body = %s", rec.Body.String())
	}
	if got := m.Value("http_rate_limited_total"); got != 1 {
		t.Errorf("http_rate_limited_total = %v, want 1 (only the rejected request)", got)
	}
}

// Redis down: requests are ALLOWED (fail open), not rejected.
func TestMiddleware_FailsOpenWhenRedisIsDown(t *testing.T) {
	h := Middleware(NewLimiter(testutil.NewBrokenRedis(t), 1, time.Minute, ""), ByIP(0), nil)(okHandler())

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200 (fail open)", i+1, rec.Code)
		}
	}
}

func TestKeyFuncs(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.4:40000"
	req.Header.Set("X-Forwarded-For", "1.1.1.1") // must be ignored

	if got := ByIP(0)(req); got != "ip:198.51.100.4" {
		t.Errorf("ByIP = %q (X-Forwarded-For must not be trusted)", got)
	}
	if got := ByUser(0)(req); got != "ip:198.51.100.4" {
		t.Errorf("ByUser without a user = %q, want the IP fallback", got)
	}
	authed := req.WithContext(identity.NewContext(req.Context(), identity.Principal{UserID: 42}))
	if got := ByUser(1)(authed); got != "user:42" {
		t.Errorf("ByUser = %q, want user:42", got)
	}
}

// Behind N trusted proxies (e.g. Render's load balancer, N = 1), the
// client is the entry N places from the RIGHT of X-Forwarded-For. Anything
// further left was written by the client and can be forged.
func TestClientIP_BehindProxies(t *testing.T) {
	const lb = "10.0.0.5:41000" // every request arrives from the load balancer
	cases := []struct {
		name string
		hops int
		xff  []string
		want string
	}{
		{"no proxy configured: header ignored", 0, []string{"203.0.113.7"}, "10.0.0.5"},
		{"one proxy", 1, []string{"203.0.113.7"}, "203.0.113.7"},
		{"forged entry is skipped", 1, []string{"6.6.6.6, 203.0.113.7"}, "203.0.113.7"},
		{"two proxies", 2, []string{"203.0.113.7, 172.16.0.9"}, "203.0.113.7"},
		{"several header lines count as one list", 1, []string{"6.6.6.6", "203.0.113.7"}, "203.0.113.7"},
		{"header missing: peer address", 1, nil, "10.0.0.5"},
		{"fewer entries than proxies: peer address", 2, []string{"203.0.113.7"}, "10.0.0.5"},
		{"garbage: peer address", 1, []string{"not-an-ip"}, "10.0.0.5"},
		{"IPv6 client", 1, []string{"2001:db8::1"}, "2001:db8::1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = lb
			for _, v := range c.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if got := ClientIP(req, c.hops); got != c.want {
				t.Errorf("ClientIP = %q, want %q", got, c.want)
			}
		})
	}
}

// Behind a proxy, two visitors get two separate budgets (without
// TRUSTED_PROXY_HOPS they would share the load balancer's single budget).
func TestMiddleware_PerClientBehindProxy(t *testing.T) {
	rdb, prefix := testutil.NewRedis(t)
	h := Middleware(NewLimiter(rdb, 1, time.Minute, prefix), ByIP(1), nil)(okHandler())

	send := func(client string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.5:41000"
		req.Header.Set("X-Forwarded-For", client)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if send("203.0.113.1") != 200 || send("203.0.113.2") != 200 {
		t.Fatal("first request of each client must pass")
	}
	if code := send("203.0.113.1"); code != http.StatusTooManyRequests {
		t.Errorf("second request of client 1: %d, want 429", code)
	}
}
