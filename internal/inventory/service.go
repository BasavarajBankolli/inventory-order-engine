package inventory

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

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

// ---------------------------------------------------------------------------
// Reservations. These run inside the CALLER's transaction (the orders
// service), so that the order and the stock change commit or roll back
// together. That is why they take a pgx.Tx instead of starting their own.
// ---------------------------------------------------------------------------

// ReserveForOrder reserves stock for every line of an order, or fails with
// ErrOutOfStock. On failure the caller must roll back, which also undoes
// the lines that were already reserved.
//
// LOCK ORDER: rows are locked in ascending product id. If order A locked
// product 1 then 2 while order B locked 2 then 1, each could end up waiting
// for the other forever (a deadlock). When every transaction locks in the
// same global order, that cycle cannot form.
func (s *Service) ReserveForOrder(ctx context.Context, tx pgx.Tx, orderID int64, lines []ReserveLine, ttl time.Duration) ([]Reservation, error) {
	sorted := slices.Clone(lines)
	slices.SortFunc(sorted, func(a, b ReserveLine) int { return cmp.Compare(a.ProductID, b.ProductID) })

	repo := s.repo.WithTx(tx)
	reservations := make([]Reservation, 0, len(sorted))
	for _, line := range sorted {
		inv, err := repo.LockRow(ctx, line.ProductID)
		if err != nil {
			return nil, err
		}
		if err := inv.Reserve(line.Quantity); err != nil {
			return nil, err // ErrOutOfStock
		}
		if _, err := repo.Save(ctx, inv); err != nil {
			return nil, err
		}
		res, err := repo.CreateReservation(ctx, orderID, line, ttl)
		if err != nil {
			return nil, err
		}
		reservations = append(reservations, res)
	}
	return reservations, nil
}

// ReleaseForOrder returns the stock of all ACTIVE reservations of an order
// to available and marks them RELEASED (order cancelled) or EXPIRED (time
// ran out). It returns how many reservations were released.
//
// It is idempotent: a second call finds no ACTIVE reservations and does
// nothing, so a stock can never be returned twice.
func (s *Service) ReleaseForOrder(ctx context.Context, tx pgx.Tx, orderID int64, to ReservationStatus) (int, error) {
	if to != ReservationReleased && to != ReservationExpired {
		return 0, fmt.Errorf("release: invalid target status %s", to)
	}
	return s.finishReservations(ctx, tx, orderID, to, (*Inventory).ReleaseReserved)
}

// ConfirmForOrder turns all ACTIVE reservations of a paid order into sales:
// reserved -= qty (available is untouched - those units were already taken
// out of it at reservation time) and the reservations become CONFIRMED.
// Idempotent in the same way as ReleaseForOrder.
func (s *Service) ConfirmForOrder(ctx context.Context, tx pgx.Tx, orderID int64) (int, error) {
	return s.finishReservations(ctx, tx, orderID, ReservationConfirmed, (*Inventory).ConfirmReserved)
}

// HasExpiredReservations reports whether the order's stock hold has run
// out. Payment refuses such orders: the stock may be about to go back on
// sale (Stage 10 worker), so it must not be sold to this order any more.
func (s *Service) HasExpiredReservations(ctx context.Context, tx pgx.Tx, orderID int64) (bool, error) {
	return s.repo.WithTx(tx).HasExpiredActive(ctx, orderID)
}

// finishReservations is the shared loop behind Release and Confirm: lock the
// order's ACTIVE reservations (sorted by product id = global lock order),
// apply the stock movement to each product row, and finish each reservation.
func (s *Service) finishReservations(ctx context.Context, tx pgx.Tx, orderID int64, to ReservationStatus,
	apply func(inv *Inventory, qty int) error) (int, error) {
	repo := s.repo.WithTx(tx)
	active, err := repo.ActiveReservationsForUpdate(ctx, orderID)
	if err != nil {
		return 0, err
	}
	for _, res := range active {
		inv, err := repo.LockRow(ctx, res.ProductID)
		if err != nil {
			return 0, err
		}
		if err := apply(&inv, res.Quantity); err != nil {
			return 0, err
		}
		if _, err := repo.Save(ctx, inv); err != nil {
			return 0, err
		}
		if err := repo.FinishReservation(ctx, res.ID, to); err != nil {
			return 0, err
		}
	}
	return len(active), nil
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
