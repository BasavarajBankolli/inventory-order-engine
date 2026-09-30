// Command concurrency-demo fires many simultaneous orders at a RUNNING API
// and reports what happened. It is the live version of the "100 buyers,
// 1 item" test.
//
//	go run ./cmd/concurrency-demo                       # 100 buyers, stock 1
//	go run ./cmd/concurrency-demo -buyers 200 -stock 5
//
// It needs an ADMIN account (to create the demo product and its stock); see
// "Creating an admin" in the README. Buyer accounts are created on the fly.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

func main() {
	var (
		base      = flag.String("base", "http://localhost:8080", "API base URL")
		adminUser = flag.String("admin-email", "alice@example.com", "admin account email")
		adminPass = flag.String("admin-password", "super-secret-1", "admin account password (local dev only)")
		buyers    = flag.Int("buyers", 100, "number of concurrent buyers")
		stock     = flag.Int("stock", 1, "units in stock before the race")
		quantity  = flag.Int("quantity", 1, "units each buyer orders")
	)
	flag.Parse()

	if err := run(*base, *adminUser, *adminPass, *buyers, *stock, *quantity); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(base, adminEmail, adminPassword string, buyers, stock, quantity int) error {
	c := &client{
		base: base,
		http: &http.Client{
			Timeout: 30 * time.Second,
			// Keep enough idle connections so 100 parallel requests do not
			// each open (and leave behind) a new TCP connection.
			Transport: &http.Transport{MaxIdleConnsPerHost: buyers},
		},
	}

	fmt.Println("1. Logging in as admin", adminEmail)
	adminToken, err := c.login(adminEmail, adminPassword)
	if err != nil {
		return fmt.Errorf("admin login: %w (is the API running, and is this user an ADMIN?)", err)
	}

	sku := fmt.Sprintf("DEMO-%d", time.Now().Unix())
	fmt.Printf("2. Creating product %s with %d unit(s) in stock\n", sku, stock)
	var product struct {
		ID int64 `json:"id"`
	}
	if err := c.call("POST", "/api/v1/products", adminToken,
		map[string]any{"sku": sku, "name": "Concurrency demo item", "price": 99900, "currency": "INR"},
		http.StatusCreated, &product); err != nil {
		return fmt.Errorf("create product: %w", err)
	}
	if err := c.call("PATCH", fmt.Sprintf("/api/v1/products/%d/inventory", product.ID), adminToken,
		map[string]any{"adjustment": stock}, http.StatusOK, nil); err != nil {
		return fmt.Errorf("add stock: %w", err)
	}

	fmt.Printf("3. Preparing %d buyer accounts (not timed)\n", buyers)
	tokens, err := c.prepareBuyers(buyers)
	if err != nil {
		return err
	}

	fmt.Printf("4. GO: %d buyers x %d unit(s), all at the same instant\n", buyers, quantity)
	results := make([]result, buyers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = c.placeOrder(tokens[i], product.ID, quantity)
		}()
	}
	began := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(began)

	var inv struct {
		Available int `json:"available_quantity"`
		Reserved  int `json:"reserved_quantity"`
	}
	if err := c.call("GET", fmt.Sprintf("/api/v1/products/%d/inventory", product.ID), adminToken,
		nil, http.StatusOK, &inv); err != nil {
		return fmt.Errorf("read inventory: %w", err)
	}

	return report(results, elapsed, stock, quantity, inv.Available, inv.Reserved)
}

type result struct {
	status int
	code   string // API error code, "" on success
}

func report(results []result, elapsed time.Duration, stock, quantity, available, reserved int) error {
	counts := map[string]int{}
	success := 0
	for _, r := range results {
		key := fmt.Sprintf("%d %s", r.status, r.code)
		if r.status == http.StatusCreated {
			key = "201 CREATED"
			success++
		}
		counts[key]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Printf("\nResults (%d requests in %v)\n", len(results), elapsed.Round(time.Millisecond))
	for _, k := range keys {
		fmt.Printf("   %-25s %4d\n", k, counts[k])
	}
	fmt.Printf("\nFinal inventory: available=%d reserved=%d\n", available, reserved)

	expected := min(stock/quantity, len(results))
	ok := success == expected && available >= 0 && reserved == success*quantity &&
		available+reserved == stock && counts["201 CREATED"]+counts["409 OUT_OF_STOCK"] == len(results)

	if ok {
		fmt.Printf("\nPASS: exactly %d order(s) succeeded, the rest got OUT_OF_STOCK, stock never went negative.\n", expected)
		return nil
	}
	return fmt.Errorf("FAIL: expected %d successful order(s), got %d", expected, success)
}

// --- tiny HTTP client --------------------------------------------------------

type client struct {
	base string
	http *http.Client
}

var errStatus = errors.New("unexpected status")

func (c *client) call(method, path, token string, body any, wantStatus int, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("%w %d from %s %s: %s", errStatus, resp.StatusCode, method, path, bytes.TrimSpace(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *client) login(email, password string) (string, error) {
	var res struct {
		AccessToken string `json:"access_token"`
	}
	err := c.call("POST", "/api/v1/auth/login", "", map[string]string{"email": email, "password": password},
		http.StatusOK, &res)
	return res.AccessToken, err
}

// prepareBuyers registers (first run) or just logs in (later runs) the
// buyer accounts, 10 at a time.
func (c *client) prepareBuyers(n int) ([]string, error) {
	const password = "demo-buyer-password"
	tokens := make([]string, n)
	errs := make([]error, n)
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			email := fmt.Sprintf("demo-buyer-%03d@example.com", i)
			err := c.call("POST", "/api/v1/auth/register", "",
				map[string]string{"email": email, "name": "Demo Buyer", "password": password}, http.StatusCreated, nil)
			if err != nil && !errors.Is(err, errStatus) {
				errs[i] = err
				return
			}
			// A 409 (already registered on an earlier run) is fine: log in.
			tokens[i], errs[i] = c.login(email, password)
		}()
	}
	wg.Wait()
	return tokens, errors.Join(errs...)
}

func (c *client) placeOrder(token string, productID int64, quantity int) result {
	body := map[string]any{"items": []map[string]any{{"product_id": productID, "quantity": quantity}}}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}

	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", c.base+"/api/v1/orders", bytes.NewReader(b))
	if err != nil {
		return result{status: -1, code: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return result{status: -1, code: "NETWORK_ERROR"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
	}
	return result{status: resp.StatusCode, code: errBody.Error.Code}
}
