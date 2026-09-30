package inventory

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/validate"
)

// maxAdjustment limits one relative change, to catch typos like 1000000 vs 100.
const maxAdjustment = 1_000_000

// Service holds the inventory use cases. It owns the transaction boundaries.
type Service struct {
	pool *pgxpool.Pool
	repo *Repository
}

// NewService creates an inventory Service.
func NewService(pool *pgxpool.Pool, repo *Repository) *Service {
	return &Service{pool: pool, repo: repo}
}

// UpdateInput describes one stock change. Exactly one of Adjustment or
// AvailableQuantity must be set.
//
//	{"adjustment": 25}                          relative: +25 units (restock)
//	{"adjustment": -2}                          relative: 2 damaged units
//	{"available_quantity": 100, "version": 7}   absolute: stock count result
//
// Version is optimistic locking: "apply this only if the inventory is still
// at the version I looked at". It is REQUIRED for absolute updates, because
// overwriting a number you read a while ago is exactly how updates get lost.
// It is optional for adjustments, which are safe on their own (see below).
type UpdateInput struct {
	Adjustment        *int
	AvailableQuantity *int
	Version           *int64
}

// Get returns the current stock of a product.
func (s *Service) Get(ctx context.Context, productID int64) (Inventory, error) {
	return s.repo.Get(ctx, productID)
}

// Update applies a stock change safely under concurrency:
//
//	BEGIN
//	  SELECT ... FOR UPDATE      lock the row; concurrent updates wait here
//	  check version (optional)   reject if the client's view is stale
//	  apply Adjust/SetAvailable  business rules in plain Go
//	  UPDATE ... version+1       save
//	COMMIT                       release the lock; the next waiter proceeds
func (s *Service) Update(ctx context.Context, productID int64, in UpdateInput) (Inventory, error) {
	if err := validateUpdate(in); err != nil {
		return Inventory{}, err
	}

	var saved Inventory
	err := database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		repo := s.repo.WithTx(tx)

		inv, err := repo.GetForUpdate(ctx, productID)
		if err != nil {
			return err
		}
		before := inv.AvailableQuantity

		if in.Version != nil && *in.Version != inv.Version {
			return ErrVersionConflict
		}

		if in.Adjustment != nil {
			err = inv.Adjust(*in.Adjustment)
		} else {
			err = inv.SetAvailable(*in.AvailableQuantity)
		}
		if err != nil {
			return err
		}

		saved, err = repo.Save(ctx, inv)
		if err != nil {
			return err
		}

		slog.InfoContext(ctx, "inventory updated",
			"product_id", productID,
			"available_before", before,
			"available_after", saved.AvailableQuantity,
			"version", saved.Version)
		return nil
	})
	return saved, err
}

func validateUpdate(in UpdateInput) error {
	v := validate.Errors{}

	switch {
	case in.Adjustment == nil && in.AvailableQuantity == nil:
		v.Add("body", "provide either adjustment or available_quantity")
	case in.Adjustment != nil && in.AvailableQuantity != nil:
		v.Add("body", "provide either adjustment or available_quantity, not both")
	case in.Adjustment != nil:
		a := *in.Adjustment
		v.Check(a != 0, "adjustment", "must not be zero")
		v.Check(a >= -maxAdjustment && a <= maxAdjustment, "adjustment",
			fmt.Sprintf("must be between %d and %d", -maxAdjustment, maxAdjustment))
	default: // absolute update
		q := *in.AvailableQuantity
		v.Check(q >= 0 && q <= MaxQuantity, "available_quantity", fmt.Sprintf("must be between 0 and %d", MaxQuantity))
		v.Check(in.Version != nil, "version", "is required when setting available_quantity (read it with GET first)")
	}

	if in.Version != nil {
		v.Check(*in.Version >= 1, "version", "must be a positive integer")
	}
	return v.Err()
}
