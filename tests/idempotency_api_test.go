package tests

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
)

func TestIdempotency_API(t *testing.T) {
	api := newTestAPI(t)
	admin := api.loginAs("admin@example.com", true)
	alice := api.loginAs("alice@example.com", false)
	p := api.createStockedProduct(admin, "IDEM-API", 1000, 10)
	body := orderBody([2]int64{p, 2})
	key := map[string]string{"Idempotency-Key": "checkout-7f3a"}

	// First request: a new order.
	var first orderResponse
	resp := api.doWithHeaders("POST", "/api/v1/orders", body, alice, key, &first)
	expectStatus(t, resp, http.StatusCreated)
	if resp.Header.Get("Idempotent-Replayed") != "" {
		t.Error("first request must not be marked as replayed")
	}

	// Retry after a "timeout": the SAME order, status 200, replay header.
	var retry orderResponse
	resp = api.doWithHeaders("POST", "/api/v1/orders", body, alice, key, &retry)
	expectStatus(t, resp, http.StatusOK)
	if resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Error("retry is missing Idempotent-Replayed: true")
	}
	if retry.ID != first.ID || retry.Status != first.Status || len(retry.Items) != 1 {
		t.Errorf("retry = %+v, want the original order %d", retry, first.ID)
	}
	if resp.Header.Get("Location") != "/api/v1/orders/"+strconv.FormatInt(first.ID, 10) {
		t.Errorf("Location = %q", resp.Header.Get("Location"))
	}

	// Same key, different basket: 409.
	var reused errorResponse
	resp = api.doWithHeaders("POST", "/api/v1/orders", orderBody([2]int64{p, 5}), alice, key, &reused)
	expectStatus(t, resp, http.StatusConflict)
	if reused.Error.Code != "IDEMPOTENCY_KEY_REUSED" {
		t.Errorf("code = %q, want IDEMPOTENCY_KEY_REUSED", reused.Error.Code)
	}

	// Invalid key: 400.
	var bad errorResponse
	resp = api.doWithHeaders("POST", "/api/v1/orders", body, alice, map[string]string{"Idempotency-Key": "has spaces"}, &bad)
	expectStatus(t, resp, http.StatusBadRequest)
	if bad.Error.Fields["Idempotency-Key"] == "" {
		t.Errorf("fields = %v, want an Idempotency-Key problem", bad.Error.Fields)
	}

	// Stock was reserved exactly once.
	if inv := api.stockOf(admin, p); inv.AvailableQuantity != 8 || inv.ReservedQuantity != 2 {
		t.Errorf("inventory = %+v, want 8/2", inv)
	}
}

// 25 identical requests with the same key fired at once over HTTP: exactly
// one 201, the rest 200 replays, all with the same order id.
func TestIdempotency_API_ConcurrentRetries(t *testing.T) {
	api := newTestAPI(t)
	admin := api.loginAs("admin@example.com", true)
	alice := api.loginAs("alice@example.com", false)
	p := api.createStockedProduct(admin, "IDEM-STORM", 1000, 10)
	key := map[string]string{"Idempotency-Key": "double-click-9"}

	const n = 25
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		statuses = map[int]int{}
		ids      = map[int64]bool{}
		start    = make(chan struct{})
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var o orderResponse
			resp := api.doWithHeaders("POST", "/api/v1/orders", orderBody([2]int64{p, 1}), alice, key, &o)
			mu.Lock()
			defer mu.Unlock()
			statuses[resp.StatusCode]++
			ids[o.ID] = true
		}()
	}
	close(start)
	wg.Wait()

	if statuses[http.StatusCreated] != 1 || statuses[http.StatusOK] != n-1 || len(ids) != 1 {
		t.Errorf("statuses = %v, distinct order ids = %d; want {201:1, 200:%d} and 1 id", statuses, len(ids), n-1)
	}
	if inv := api.stockOf(admin, p); inv.ReservedQuantity != 1 {
		t.Errorf("reserved = %d, want 1", inv.ReservedQuantity)
	}
}
