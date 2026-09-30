package tests

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// The project's headline guarantee, tested end-to-end over real HTTP:
// 100 different customers try to buy the last unit at the same moment.
//
// Expected: exactly one 201, exactly 99 x 409 OUT_OF_STOCK, no 5xx, and the
// product ends at 0 available / 1 reserved.
func TestAPI_100ConcurrentBuyersForTheLastUnit(t *testing.T) {
	api := newTestAPI(t)
	admin := api.loginAs("admin@example.com", true)
	product := api.createStockedProduct(admin, "LAST-UNIT", 99900, 1)

	const buyers = 100
	tokens := make([]string, buyers)
	for i := range tokens {
		tokens[i] = api.loginAs(fmt.Sprintf("buyer%03d@example.com", i), false)
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		statuses = map[int]int{} // HTTP status -> count
		codes    = map[string]int{}
		start    = make(chan struct{})
	)
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var body errorResponse
			resp := api.do("POST", "/api/v1/orders", orderBody([2]int64{product, 1}), tokens[i], &body)

			mu.Lock()
			defer mu.Unlock()
			statuses[resp.StatusCode]++
			if body.Error.Code != "" {
				codes[body.Error.Code]++
			}
		}()
	}
	close(start)
	wg.Wait()

	if statuses[http.StatusCreated] != 1 || statuses[http.StatusConflict] != 99 || len(statuses) != 2 {
		t.Errorf("HTTP statuses = %v, want exactly {201: 1, 409: 99}", statuses)
	}
	if codes["OUT_OF_STOCK"] != 99 {
		t.Errorf("error codes = %v, want 99 x OUT_OF_STOCK", codes)
	}
	if inv := api.stockOf(admin, product); inv.AvailableQuantity != 0 || inv.ReservedQuantity != 1 {
		t.Errorf("final inventory = %+v, want 0 available / 1 reserved", inv)
	}

	// Admins see every order: exactly one exists.
	var all orderListResponse
	expectStatus(t, api.do("GET", "/api/v1/orders", nil, admin, &all), http.StatusOK)
	if all.Total != 1 {
		t.Errorf("%d orders exist, want exactly 1", all.Total)
	}
}
