package orders

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/products"
	"inventory-order-engine/internal/validate"
)

const (
	defaultLimit = 20
	maxLimit     = 100
)

// Service holds the order use cases and owns their transactions.
type Service struct {
	pool      *pgxpool.Pool
	orders    *Repository
	products  *products.Repository
	inventory *inventory.Service

	// reservationTTL is how long reserved stock is held for an unpaid order.
	reservationTTL time.Duration
}

// NewService creates an orders Service.
func NewService(pool *pgxpool.Pool, orders *Repository, prods *products.Repository,
	inv *inventory.Service, reservationTTL time.Duration) *Service {
	return &Service{pool: pool, orders: orders, products: prods, inventory: inv, reservationTTL: reservationTTL}
}

// Create places an order for the caller and reserves its stock.
//
//	validate request shape                       (no DB)
//	BEGIN
//	  load the requested products                (current price + status)
//	  buildItems: check + price snapshot + total  (pure Go)
//	  INSERT order (CREATED) + INSERT items
//	  reserve stock: lock inventory rows (by product id),
//	    available -= qty, reserved += qty,
//	    INSERT reservations (ACTIVE, expires_at)  -> ErrOutOfStock = ROLLBACK
//	  CREATED -> RESERVED                        (state machine)
//	COMMIT
//
// Everything is one transaction: either the order exists AND its stock is
// reserved, or nothing happened at all. There is never an order without a
// reservation, nor a reservation without an order.
//
// Reading the products INSIDE the transaction means the price stored in the
// order is the price at the moment of ordering, and an archived/inactive
// product is rejected even if it was active when the client loaded the page.
func (s *Service) Create(ctx context.Context, caller identity.Principal, requested []ItemRequest) (Order, error) {
	if err := validateItems(requested); err != nil {
		return Order{}, err
	}

	var created Order
	err := database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		ids := make([]int64, len(requested))
		for i, it := range requested {
			ids[i] = it.ProductID
		}
		// FOR SHARE: the products cannot be archived or re-priced until we
		// commit, so the snapshot we store is still true at commit time.
		found, err := s.products.WithTx(tx).GetByIDsForShare(ctx, ids)
		if err != nil {
			return err
		}
		byID := make(map[int64]products.Product, len(found))
		for _, p := range found {
			byID[p.ID] = p
		}

		items, total, currency, err := buildItems(requested, byID)
		if err != nil {
			return err
		}

		repo := s.orders.WithTx(tx)
		created, err = repo.Create(ctx, Order{
			UserID:      caller.UserID,
			Status:      StatusCreated,
			TotalAmount: total,
			Currency:    currency,
			Items:       items,
		})
		if err != nil {
			return err
		}

		lines := make([]inventory.ReserveLine, len(items))
		for i, it := range items {
			lines[i] = inventory.ReserveLine{ProductID: it.ProductID, Quantity: it.Quantity}
		}
		if _, err := s.inventory.ReserveForOrder(ctx, tx, created.ID, lines, s.reservationTTL); err != nil {
			return err // ErrOutOfStock -> the order insert above is rolled back too
		}

		return s.transition(ctx, repo, &created, StatusReserved)
	})
	if err != nil {
		return Order{}, err
	}

	slog.InfoContext(ctx, "order created and stock reserved",
		"order_id", created.ID, "total_amount", created.TotalAmount,
		"currency", created.Currency, "items", len(created.Items))
	return created, nil
}

// transition applies a state machine move to o and saves it. Items are kept.
func (s *Service) transition(ctx context.Context, repo *Repository, o *Order, to Status) error {
	from := o.Status
	if err := o.TransitionTo(to); err != nil {
		return err
	}
	saved, err := repo.UpdateStatus(ctx, o.ID, from, to)
	if err != nil {
		return err
	}
	saved.Items = o.Items
	*o = saved
	return nil
}

// Get returns one order with its items.
//
// Customers can only see their own orders. For someone else's order we
// return ErrNotFound, not a "forbidden" error: a 403 would confirm that
// order #123 exists, which leaks information (e.g. how many orders the shop
// gets). Admins can see every order.
func (s *Service) Get(ctx context.Context, caller identity.Principal, id int64) (Order, error) {
	o, err := s.orders.GetByID(ctx, id)
	if err != nil {
		return Order{}, err
	}
	if !canAccess(caller, o) {
		return Order{}, ErrNotFound
	}
	return o, nil
}

// ListInput filters the order list.
type ListInput struct {
	Status Status
	Limit  int
	Offset int
}

// ListResult is one page of orders.
type ListResult struct {
	Items  []Order `json:"items"`
	Total  int64   `json:"total"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
}

// List returns the caller's orders (customers) or everyone's (admins).
func (s *Service) List(ctx context.Context, caller identity.Principal, in ListInput) (ListResult, error) {
	if in.Limit == 0 {
		in.Limit = defaultLimit
	}
	v := validate.Errors{}
	v.Check(in.Status == "" || isKnownStatus(in.Status), "status", "is not a valid order status")
	v.Check(in.Limit >= 1 && in.Limit <= maxLimit, "limit", fmt.Sprintf("must be between 1 and %d", maxLimit))
	v.Check(in.Offset >= 0, "offset", "must not be negative")
	if err := v.Err(); err != nil {
		return ListResult{}, err
	}

	p := ListParams{UserID: caller.UserID, Status: in.Status, Limit: in.Limit, Offset: in.Offset}
	if caller.Role == identity.RoleAdmin {
		p.UserID = 0 // all users
	}

	list, total, err := s.orders.List(ctx, p)
	if err != nil {
		return ListResult{}, err
	}
	return ListResult{Items: list, Total: total, Limit: in.Limit, Offset: in.Offset}, nil
}

// Cancel moves an order to CANCELLED and returns its reserved stock.
//
//	BEGIN
//	  SELECT order FOR UPDATE    two concurrent cancels (or cancel + expiry)
//	                             cannot both act on the same old status
//	  TransitionTo(CANCELLED)    the state machine decides
//	  UPDATE status
//	  release ACTIVE reservations: reserved -= qty, available += qty,
//	    reservation -> RELEASED
//	COMMIT
//
// Lock order is always: order row first, then inventory rows by product id
// (the same order Create uses), so cancel and create cannot deadlock.
func (s *Service) Cancel(ctx context.Context, caller identity.Principal, id int64) (Order, error) {
	var (
		updated  Order
		released int
	)
	err := database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		repo := s.orders.WithTx(tx)

		o, err := repo.GetForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if !canAccess(caller, o) {
			return ErrNotFound
		}

		if err := s.transition(ctx, repo, &o, StatusCancelled); err != nil {
			return err
		}

		// Orders created before Stage 6 have no reservations; releasing
		// then simply does nothing.
		released, err = s.inventory.ReleaseForOrder(ctx, tx, o.ID, inventory.ReservationReleased)
		if err != nil {
			return err
		}

		updated = o
		updated.Items, err = repo.items(ctx, o.ID)
		return err
	})
	if err != nil {
		return Order{}, err
	}

	slog.InfoContext(ctx, "order cancelled", "order_id", updated.ID, "reservations_released", released)
	return updated, nil
}

func canAccess(caller identity.Principal, o Order) bool {
	return caller.Role == identity.RoleAdmin || o.UserID == caller.UserID
}

func isKnownStatus(s Status) bool {
	switch s {
	case StatusCreated, StatusReserved, StatusPaymentPending, StatusPaymentFailed, StatusConfirmed,
		StatusProcessing, StatusShipped, StatusDelivered, StatusCancelled, StatusExpired:
		return true
	}
	return false
}
