package orders

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"inventory-order-engine/internal/database"
)

// Repository contains all SQL for the orders and order_items tables.
type Repository struct {
	db database.DBTX
}

// NewRepository creates an orders Repository.
func NewRepository(db database.DBTX) *Repository {
	return &Repository{db: db}
}

// WithTx returns a copy of the repository whose queries run inside tx.
func (r *Repository) WithTx(tx pgx.Tx) *Repository {
	return &Repository{db: tx}
}

const orderColumns = `id, user_id, status, total_amount, currency, created_at, updated_at`

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	err := row.Scan(&o.ID, &o.UserID, &o.Status, &o.TotalAmount, &o.Currency, &o.CreatedAt, &o.UpdatedAt)
	return o, err
}

// Create inserts the order and all its items. It must run inside a
// transaction (WithTx) so that an order never exists without its items.
//
// If another order of the same user already has this idempotency key,
// PostgreSQL's unique index rejects the INSERT and we return
// errDuplicateIdempotencyKey. If that other order's transaction is still
// running, PostgreSQL first WAITS for it to finish: only if it commits is
// our INSERT a duplicate; if it rolls back, our INSERT succeeds.
func (r *Repository) Create(ctx context.Context, o Order) (Order, error) {
	// NULLIF('', '') = NULL: orders without a key store NULL, which the
	// partial unique index ignores.
	row := r.db.QueryRow(ctx, `
		INSERT INTO orders (user_id, status, total_amount, currency, idempotency_key, request_hash)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''))
		RETURNING `+orderColumns,
		o.UserID, o.Status, o.TotalAmount, o.Currency, o.idempotencyKey, o.requestHash,
	)
	created, err := scanOrder(row)
	if database.IsUniqueViolation(err, "orders_user_idempotency_key_idx") {
		return Order{}, errDuplicateIdempotencyKey
	}
	if err != nil {
		return Order{}, fmt.Errorf("insert order: %w", err)
	}
	created.idempotencyKey, created.requestHash = o.idempotencyKey, o.requestHash

	// One INSERT per item keeps the code obvious. An order has at most 50
	// items, so the extra round trips do not matter here.
	for _, it := range o.Items {
		if _, err := r.db.Exec(ctx, `
			INSERT INTO order_items (order_id, product_id, quantity, unit_price, total_price)
			VALUES ($1, $2, $3, $4, $5)`,
			created.ID, it.ProductID, it.Quantity, it.UnitPrice, it.TotalPrice,
		); err != nil {
			return Order{}, fmt.Errorf("insert order item: %w", err)
		}
	}

	created.Items = o.Items
	return created, nil
}

// GetByID returns an order with its items.
func (r *Repository) GetByID(ctx context.Context, id int64) (Order, error) {
	o, err := oneOrder(r.db.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE id = $1`, id))
	if err != nil {
		return Order{}, err
	}
	o.Items, err = r.items(ctx, id)
	return o, err
}

// GetByIdempotencyKey finds the user's order that was created with key,
// including its items and the stored request fingerprint.
func (r *Repository) GetByIdempotencyKey(ctx context.Context, userID int64, key string) (Order, error) {
	var hash string
	row := r.db.QueryRow(ctx, `
		SELECT `+orderColumns+`, request_hash
		FROM orders
		WHERE user_id = $1 AND idempotency_key = $2`, userID, key)
	var o Order
	err := row.Scan(&o.ID, &o.UserID, &o.Status, &o.TotalAmount, &o.Currency, &o.CreatedAt, &o.UpdatedAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, fmt.Errorf("select order by idempotency key: %w", err)
	}
	o.idempotencyKey, o.requestHash = key, hash

	o.Items, err = r.items(ctx, o.ID)
	return o, err
}

// GetForUpdate reads an order (without items) and locks its row until the
// transaction ends, so two concurrent status changes cannot interleave.
func (r *Repository) GetForUpdate(ctx context.Context, id int64) (Order, error) {
	return oneOrder(r.db.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE id = $1 FOR UPDATE`, id))
}

// UpdateStatus saves a status change that the caller already validated with
// Order.TransitionTo.
//
// "AND status = $3" (the status we expect to replace) is a guard: if the
// row's status is no longer what the caller saw, nothing is updated and we
// return ErrInvalidTransition instead of overwriting someone else's change.
func (r *Repository) UpdateStatus(ctx context.Context, id int64, from, to Status) (Order, error) {
	row := r.db.QueryRow(ctx, `
		UPDATE orders SET status = $2, updated_at = now()
		WHERE id = $1 AND status = $3
		RETURNING `+orderColumns, id, to, from)
	o, err := scanOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, fmt.Errorf("%w: order %d is no longer %s", ErrInvalidTransition, id, from)
	}
	if err != nil {
		return Order{}, fmt.Errorf("update order status: %w", err)
	}
	return o, nil
}

// ListParams filters and paginates orders.
type ListParams struct {
	UserID int64  // 0 = all users (admins only; enforced by the service)
	Status Status // "" = any status
	Limit  int
	Offset int
}

// List returns one page of orders (without items) and the total count.
func (r *Repository) List(ctx context.Context, p ListParams) ([]Order, int64, error) {
	// "$1 = 0 OR user_id = $1" lets one fixed query serve both "my orders"
	// and "all orders"; the same trick is used for the optional status.
	const where = `($1 = 0 OR user_id = $1) AND ($2 = '' OR status = $2)`

	var total int64
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM orders WHERE `+where, p.UserID, p.Status).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count orders: %w", err)
	}

	rows, err := r.db.Query(ctx, `
		SELECT `+orderColumns+` FROM orders
		WHERE `+where+`
		ORDER BY id DESC
		LIMIT $3 OFFSET $4`, p.UserID, p.Status, p.Limit, p.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list orders: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Order, error) { return scanOrder(row) })
	if err != nil {
		return nil, 0, fmt.Errorf("scan orders: %w", err)
	}
	return list, total, nil
}

func (r *Repository) items(ctx context.Context, orderID int64) ([]Item, error) {
	rows, err := r.db.Query(ctx, `
		SELECT product_id, quantity, unit_price, total_price
		FROM order_items WHERE order_id = $1 ORDER BY id`, orderID)
	if err != nil {
		return nil, fmt.Errorf("select order items: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Item, error) {
		var it Item
		err := row.Scan(&it.ProductID, &it.Quantity, &it.UnitPrice, &it.TotalPrice)
		return it, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan order items: %w", err)
	}
	return items, nil
}

func oneOrder(row pgx.Row) (Order, error) {
	o, err := scanOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, fmt.Errorf("select order: %w", err)
	}
	return o, nil
}
