package products

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/validate"
)

// Service holds the product business rules: normalising and validating
// input, and deciding what "delete" means (archive, not remove).
//
// It depends on the concrete repositories. Unlike auth, there is no fake
// store here: the rules are plain functions (validateCreate, ...) that are
// unit-tested directly, and the SQL is tested against real PostgreSQL.
type Service struct {
	pool      *pgxpool.Pool // to start transactions
	repo      *Repository
	inventory *inventory.Repository
	cache     Cache // may be nil: then every read goes to PostgreSQL
}

// Cache is a read-through cache for single products (Redis in production,
// see internal/cache). It is an interface so this package does not depend on
// Redis, and so the service works the same with no cache at all.
//
// The cache is NEVER the source of truth: every method may fail, and the
// service then simply uses PostgreSQL.
type Cache interface {
	Get(ctx context.Context, id int64) (p Product, found bool, err error)
	Set(ctx context.Context, p Product) error
	Delete(ctx context.Context, id int64) error
}

// NewService creates a products Service. cache may be nil.
func NewService(pool *pgxpool.Pool, repo *Repository, inv *inventory.Repository, cache Cache) *Service {
	return &Service{pool: pool, repo: repo, inventory: inv, cache: cache}
}

// CreateInput is the data needed to create a product.
type CreateInput struct {
	SKU         string
	Name        string
	Description string
	Price       int64
	Currency    string
	Status      Status // optional; defaults to ACTIVE
}

// UpdateInput is a partial update: nil means "leave unchanged".
// Pointers are how Go tells "not sent" (nil) apart from "sent as empty/zero".
type UpdateInput struct {
	Name        *string
	Description *string
	Price       *int64
	Currency    *string
	Status      *Status
}

// ListParams filters and paginates the product list.
type ListParams struct {
	Search string // matches name or SKU, case-insensitive
	Status Status // defaults to ACTIVE
	Sort   string // one of the keys in sortOrders; defaults to "newest"
	Limit  int    // page size, defaults to 20, max 100
	Offset int    // how many items to skip
}

// ListResult is one page of products plus the total number of matches.
type ListResult struct {
	Items  []Product `json:"items"`
	Total  int64     `json:"total"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
}

// Create validates and stores a new product together with its (empty)
// inventory row.
//
// Both INSERTs run in ONE transaction: if the inventory insert failed after
// the product insert succeeded, the product is rolled back too. There is
// never a product without an inventory row (which Stage 5 relies on when it
// locks inventory rows to place orders).
func (s *Service) Create(ctx context.Context, in CreateInput) (Product, error) {
	in = normalizeCreate(in)
	if err := validateCreate(in); err != nil {
		return Product{}, err
	}

	var p Product
	err := database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		p, err = s.repo.WithTx(tx).Create(ctx, in)
		if err != nil {
			return err // may be ErrSKUTaken
		}
		return s.inventory.WithTx(tx).CreateForProduct(ctx, p.ID)
	})
	if err != nil {
		return Product{}, err
	}

	slog.InfoContext(ctx, "product created", "product_id", p.ID, "sku", p.SKU)
	return p, nil
}

// Get returns one product. Archived products count as not found.
//
// CACHE-ASIDE ("lazy loading"):
//
//  1. look in the cache           -> hit: return it (no database query)
//  2. miss: read PostgreSQL        -> the source of truth
//  3. put the result in the cache  -> the next Get is a hit
//
// A broken cache (Redis down, bad data) is logged and treated as a miss, so
// a cache outage makes the API slower, never wrong or unavailable.
func (s *Service) Get(ctx context.Context, id int64) (Product, error) {
	if s.cache != nil {
		p, found, err := s.cache.Get(ctx, id)
		if err != nil {
			slog.WarnContext(ctx, "product cache read failed; using database", "product_id", id, "error", err)
		} else if found {
			slog.DebugContext(ctx, "product cache hit", "product_id", id)
			return p, nil
		}
	}

	p, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return Product{}, err // not-found results are not cached
	}

	if s.cache != nil {
		if err := s.cache.Set(ctx, p); err != nil {
			slog.WarnContext(ctx, "product cache write failed", "product_id", id, "error", err)
		}
	}
	return p, nil
}

// invalidate removes a product from the cache AFTER the database change
// committed. Deleting (rather than writing the new value) is the simplest
// correct choice: the next Get loads the fresh row.
//
// If the delete fails, the stale entry still disappears when its TTL runs
// out - that bounded staleness is the price of a cache, and why prices in
// ORDERS are always read from PostgreSQL, never from the cache.
func (s *Service) invalidate(ctx context.Context, id int64) {
	if s.cache == nil {
		return
	}
	if err := s.cache.Delete(ctx, id); err != nil {
		slog.WarnContext(ctx, "product cache invalidation failed; entry will expire via TTL",
			"product_id", id, "error", err)
	}
}

// List returns one page of products matching the filters.
func (s *Service) List(ctx context.Context, p ListParams) (ListResult, error) {
	p = normalizeList(p)
	if err := validateList(p); err != nil {
		return ListResult{}, err
	}

	items, total, err := s.repo.List(ctx, p)
	if err != nil {
		return ListResult{}, err
	}
	return ListResult{Items: items, Total: total, Limit: p.Limit, Offset: p.Offset}, nil
}

// Update applies a partial update to a non-archived product.
func (s *Service) Update(ctx context.Context, id int64, in UpdateInput) (Product, error) {
	in = normalizeUpdate(in)
	if err := validateUpdate(in); err != nil {
		return Product{}, err
	}

	p, err := s.repo.Update(ctx, id, in)
	if err != nil {
		return Product{}, err
	}
	s.invalidate(ctx, id)
	slog.InfoContext(ctx, "product updated", "product_id", p.ID)
	return p, nil
}

// Archive is our "delete": the row stays (orders may reference it) but the
// product disappears from the API and can no longer be changed or ordered.
func (s *Service) Archive(ctx context.Context, id int64) error {
	if err := s.repo.Archive(ctx, id); err != nil {
		return err
	}
	s.invalidate(ctx, id)
	slog.InfoContext(ctx, "product archived", "product_id", id)
	return nil
}

// ---------------------------------------------------------------------------
// Normalisation and validation. Pure functions: no database, easy to test.
// ---------------------------------------------------------------------------

func normalizeCreate(in CreateInput) CreateInput {
	in.SKU = strings.ToUpper(strings.TrimSpace(in.SKU))
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if in.Status == "" {
		in.Status = StatusActive
	}
	return in
}

func validateCreate(in CreateInput) error {
	v := validate.Errors{}
	v.Check(skuPattern.MatchString(in.SKU), "sku", "must be 2-64 characters: letters, digits, '-' or '_'")
	checkName(v, in.Name)
	checkDescription(v, in.Description)
	checkPrice(v, in.Price)
	checkCurrency(v, in.Currency)
	checkSettableStatus(v, in.Status)
	return v.Err()
}

func normalizeUpdate(in UpdateInput) UpdateInput {
	if in.Name != nil {
		s := strings.TrimSpace(*in.Name)
		in.Name = &s
	}
	if in.Description != nil {
		s := strings.TrimSpace(*in.Description)
		in.Description = &s
	}
	if in.Currency != nil {
		s := strings.ToUpper(strings.TrimSpace(*in.Currency))
		in.Currency = &s
	}
	return in
}

func validateUpdate(in UpdateInput) error {
	v := validate.Errors{}
	if in.Name == nil && in.Description == nil && in.Price == nil && in.Currency == nil && in.Status == nil {
		v.Add("body", "at least one field must be provided")
		return v.Err()
	}
	if in.Name != nil {
		checkName(v, *in.Name)
	}
	if in.Description != nil {
		checkDescription(v, *in.Description)
	}
	if in.Price != nil {
		checkPrice(v, *in.Price)
	}
	if in.Currency != nil {
		checkCurrency(v, *in.Currency)
	}
	if in.Status != nil {
		checkSettableStatus(v, *in.Status)
	}
	return v.Err()
}

func normalizeList(p ListParams) ListParams {
	p.Search = strings.TrimSpace(p.Search)
	if p.Status == "" {
		p.Status = StatusActive
	}
	if p.Sort == "" {
		p.Sort = "newest"
	}
	if p.Limit == 0 {
		p.Limit = defaultLimit
	}
	return p
}

func validateList(p ListParams) error {
	v := validate.Errors{}
	v.Check(utf8.RuneCountInString(p.Search) <= maxSearchLen, "q", fmt.Sprintf("must be at most %d characters", maxSearchLen))
	// Only ACTIVE/INACTIVE can be listed: archived products are "deleted".
	v.Check(p.Status == StatusActive || p.Status == StatusInactive, "status", "must be ACTIVE or INACTIVE")
	_, ok := sortOrders[p.Sort]
	v.Check(ok, "sort", "must be one of: newest, oldest, price_asc, price_desc, name")
	v.Check(p.Limit >= 1 && p.Limit <= maxLimit, "limit", fmt.Sprintf("must be between 1 and %d", maxLimit))
	v.Check(p.Offset >= 0, "offset", "must not be negative")
	return v.Err()
}

func checkName(v validate.Errors, name string) {
	v.Check(name != "", "name", "is required")
	v.Check(utf8.RuneCountInString(name) <= maxNameLen, "name", fmt.Sprintf("must be at most %d characters", maxNameLen))
}

func checkDescription(v validate.Errors, d string) {
	v.Check(utf8.RuneCountInString(d) <= maxDescriptionLen, "description", fmt.Sprintf("must be at most %d characters", maxDescriptionLen))
}

func checkPrice(v validate.Errors, price int64) {
	v.Check(price > 0 && price <= maxPrice, "price",
		fmt.Sprintf("must be between 1 and %d (minor units, e.g. 129900 = 1299.00)", maxPrice))
}

func checkCurrency(v validate.Errors, c string) {
	v.Check(currencyPattern.MatchString(c), "currency", "must be a 3-letter ISO code such as INR or USD")
}

// checkSettableStatus: clients may switch between ACTIVE and INACTIVE.
// ARCHIVED is only reachable through DELETE.
func checkSettableStatus(v validate.Errors, s Status) {
	v.Check(s == StatusActive || s == StatusInactive, "status", "must be ACTIVE or INACTIVE")
}
