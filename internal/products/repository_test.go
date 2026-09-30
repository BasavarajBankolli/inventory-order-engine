package products

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"inventory-order-engine/internal/testutil"
)

// Integration tests against real PostgreSQL (skipped without TEST_DATABASE_URL).
// They live in package products (not products_test) so they can call the
// unexported normalizeList helper when building list parameters.

func newRepo(t *testing.T) *Repository {
	return NewRepository(testutil.NewMigratedPool(t))
}

func mustCreate(t *testing.T, r *Repository, sku, name string, price int64, status Status) Product {
	t.Helper()
	p, err := r.Create(context.Background(), CreateInput{SKU: sku, Name: name, Price: price, Currency: "INR", Status: status})
	if err != nil {
		t.Fatalf("Create(%s): %v", sku, err)
	}
	return p
}

func TestRepository_CreateGetDuplicate(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	p := mustCreate(t, r, "MUG-01", "Coffee Mug", 29900, StatusActive)
	if p.ID == 0 || p.Description != "" || p.Status != StatusActive {
		t.Errorf("created = %+v", p)
	}

	got, err := r.GetByID(ctx, p.ID)
	if err != nil || got.SKU != "MUG-01" || got.Price != 29900 {
		t.Errorf("GetByID() = %+v, %v", got, err)
	}

	_, err = r.Create(ctx, CreateInput{SKU: "MUG-01", Name: "Other", Price: 1, Currency: "INR", Status: StatusActive})
	if !errors.Is(err, ErrSKUTaken) {
		t.Errorf("duplicate SKU error = %v, want ErrSKUTaken", err)
	}

	if _, err := r.GetByID(ctx, 987654); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing product error = %v, want ErrNotFound", err)
	}
}

func TestRepository_UpdatePartial(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	p := mustCreate(t, r, "PEN-01", "Pen", 1500, StatusActive)

	// Give it a description first so we can check clearing it.
	desc := "blue ink"
	if _, err := r.Update(ctx, p.ID, UpdateInput{Description: &desc}); err != nil {
		t.Fatal(err)
	}

	newPrice := int64(1800)
	empty := ""
	updated, err := r.Update(ctx, p.ID, UpdateInput{Price: &newPrice, Description: &empty})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if updated.Price != 1800 {
		t.Errorf("price = %d, want 1800", updated.Price)
	}
	if updated.Description != "" {
		t.Errorf("description = %q, want it cleared", updated.Description)
	}
	if updated.Name != "Pen" || updated.Currency != "INR" {
		t.Errorf("fields that were not sent changed: %+v", updated)
	}
	if !updated.UpdatedAt.After(p.UpdatedAt) {
		t.Error("updated_at was not bumped")
	}
}

func TestRepository_ArchiveHidesProduct(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	p := mustCreate(t, r, "OLD-01", "Old thing", 100, StatusActive)

	if err := r.Archive(ctx, p.ID); err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	if _, err := r.GetByID(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID(archived) error = %v, want ErrNotFound", err)
	}
	name := "revived"
	if _, err := r.Update(ctx, p.ID, UpdateInput{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update(archived) error = %v, want ErrNotFound", err)
	}
	if err := r.Archive(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Archive() error = %v, want ErrNotFound", err)
	}

	// The SKU is still taken: the row still exists for order history.
	_, err := r.Create(ctx, CreateInput{SKU: "OLD-01", Name: "New", Price: 1, Currency: "INR", Status: StatusActive})
	if !errors.Is(err, ErrSKUTaken) {
		t.Errorf("re-using an archived SKU: error = %v, want ErrSKUTaken", err)
	}
}

func TestRepository_ListFilterSearchSortPaginate(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	mustCreate(t, r, "SHIRT-RED", "Red Shirt", 500, StatusActive)
	mustCreate(t, r, "SHIRT-BLUE", "Blue Shirt", 300, StatusActive)
	mustCreate(t, r, "HAT-01", "Sun Hat", 400, StatusActive)
	mustCreate(t, r, "SHIRT-OLD", "Old Shirt", 100, StatusInactive)
	archived := mustCreate(t, r, "SHIRT-GONE", "Gone Shirt", 200, StatusActive)
	if err := r.Archive(ctx, archived.ID); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, r, "SALE-50", "Discount 50% Off", 50, StatusActive)

	list := func(p ListParams) ([]string, int64) {
		t.Helper()
		items, total, err := r.List(ctx, normalizeList(p))
		if err != nil {
			t.Fatalf("List(%+v) error = %v", p, err)
		}
		skus := make([]string, len(items))
		for i, it := range items {
			skus[i] = it.SKU
		}
		return skus, total
	}

	// Default: ACTIVE only, newest first. Archived and inactive are hidden.
	skus, total := list(ListParams{})
	if fmt.Sprint(skus) != "[SALE-50 HAT-01 SHIRT-BLUE SHIRT-RED]" || total != 4 {
		t.Errorf("default list = %v (total %d)", skus, total)
	}

	// Search is case-insensitive and matches name or SKU.
	skus, total = list(ListParams{Search: "shirt", Sort: "price_asc"})
	if fmt.Sprint(skus) != "[SHIRT-BLUE SHIRT-RED]" || total != 2 {
		t.Errorf("search shirt = %v (total %d)", skus, total)
	}

	// "%" is matched literally, not as a wildcard.
	skus, _ = list(ListParams{Search: "50%"})
	if fmt.Sprint(skus) != "[SALE-50]" {
		t.Errorf("search 50%% = %v", skus)
	}
	skus, _ = list(ListParams{Search: "%"})
	if fmt.Sprint(skus) != "[SALE-50]" {
		t.Errorf("search %% alone = %v, want only the product containing %%", skus)
	}

	// Inactive filter.
	skus, _ = list(ListParams{Status: StatusInactive})
	if fmt.Sprint(skus) != "[SHIRT-OLD]" {
		t.Errorf("inactive list = %v", skus)
	}

	// Pagination: total stays the full count, items are one page.
	page1, total := list(ListParams{Sort: "price_desc", Limit: 2})
	page2, _ := list(ListParams{Sort: "price_desc", Limit: 2, Offset: 2})
	page3, _ := list(ListParams{Sort: "price_desc", Limit: 2, Offset: 4})
	if fmt.Sprint(page1) != "[SHIRT-RED HAT-01]" || fmt.Sprint(page2) != "[SHIRT-BLUE SALE-50]" || len(page3) != 0 || total != 4 {
		t.Errorf("pages = %v %v %v (total %d)", page1, page2, page3, total)
	}
}

func TestRepository_CheckConstraints(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()

	bad := map[string]string{
		"negative price": `INSERT INTO products (sku, name, price, currency) VALUES ('A1', 'A', -5, 'INR')`,
		"zero price":     `INSERT INTO products (sku, name, price, currency) VALUES ('A2', 'A', 0, 'INR')`,
		"huge price":     `INSERT INTO products (sku, name, price, currency) VALUES ('A3', 'A', 1000000001, 'INR')`,
		"bad currency":   `INSERT INTO products (sku, name, price, currency) VALUES ('A4', 'A', 5, 'rupees')`,
		"bad status":     `INSERT INTO products (sku, name, price, currency, status) VALUES ('A5', 'A', 5, 'INR', 'DRAFT')`,
		"lowercase sku":  `INSERT INTO products (sku, name, price, currency) VALUES ('abc', 'A', 5, 'INR')`,
		"blank name":     `INSERT INTO products (sku, name, price, currency) VALUES ('A6', ' ', 5, 'INR')`,
	}
	for name, sql := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, sql); err == nil {
				t.Errorf("insert succeeded, want constraint violation")
			}
		})
	}
}
