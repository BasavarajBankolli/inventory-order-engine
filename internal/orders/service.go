package orders

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/products"
	"inventory-order-engine/internal/validate"
)

const (
	defaultLimit = 20
	maxLimit     = 100
)

// Service holds the order use cases and owns their transactions.
type Service struct {
	pool     *pgxpool.Pool
	orders   *Repository
	products *products.Repository
}

// NewService creates an orders Service.
func NewService(pool *pgxpool.Pool, orders *Repository, prods *products.Repository) *Service {
	return &Service{pool: pool, orders: orders, products: prods}
}

// Create places an order for the caller.
//
//	validate request shape                      (no DB)
//	BEGIN
//	  load the requested products               (current price + status)
//	  buildItems: check + price snapshot + total (pure Go)
//	  INSERT order (CREATED) + INSERT items
//	COMMIT
//
// Reading the products INSIDE the transaction means the price stored in the
// order is the price at the moment of ordering, and an archived/inactive
// product is rejected even if it was active when the client loaded the page.
//
// Stage 6 adds "reserve inventory" to this same transaction.
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
		found, err := s.products.WithTx(tx).GetByIDs(ctx, ids)
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

		created, err = s.orders.WithTx(tx).Create(ctx, Order{
			UserID:      caller.UserID,
			Status:      StatusCreated,
			TotalAmount: total,
			Currency:    currency,
			Items:       items,
		})
		return err
	})
	if err != nil {
		return Order{}, err
	}

	slog.InfoContext(ctx, "order created",
		"order_id", created.ID, "total_amount", created.TotalAmount,
		"currency", created.Currency, "items", len(created.Items))
	return created, nil
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

// Cancel moves an order to CANCELLED if the state machine allows it.
//
//	BEGIN
//	  SELECT order FOR UPDATE    two concurrent cancels (or cancel + payment)
//	                             cannot both act on the same old status
//	  TransitionTo(CANCELLED)    the state machine decides
//	  UPDATE status
//	COMMIT
//
// Stage 6 adds "release the reserved stock" to this same transaction.
func (s *Service) Cancel(ctx context.Context, caller identity.Principal, id int64) (Order, error) {
	var updated Order
	err := database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		repo := s.orders.WithTx(tx)

		o, err := repo.GetForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if !canAccess(caller, o) {
			return ErrNotFound
		}

		from := o.Status
		if err := o.TransitionTo(StatusCancelled); err != nil {
			return err
		}

		updated, err = repo.UpdateStatus(ctx, o.ID, from, o.Status)
		if err != nil {
			return err
		}
		updated.Items, err = repo.items(ctx, o.ID)
		return err
	})
	if err != nil {
		return Order{}, err
	}

	slog.InfoContext(ctx, "order cancelled", "order_id", updated.ID)
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
