package inventory

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SQL for reservations. These methods are only meaningful inside a
// transaction (Repository.WithTx), together with inventory row locks.

// LockRow locks one inventory row for the rest of the transaction.
//
// Unlike GetForUpdate it ignores the product's status: an order for a
// product that was archived AFTER the order was placed must still be able
// to release or confirm its reservation.
func (r *Repository) LockRow(ctx context.Context, productID int64) (Inventory, error) {
	row := r.db.QueryRow(ctx, `
		SELECT `+inventoryColumns+`
		FROM inventory i
		WHERE i.product_id = $1
		FOR UPDATE`, productID)
	return oneInventory(row)
}

// CreateReservation inserts an ACTIVE reservation that expires ttl after the
// transaction's start time.
//
// The expiry is computed by PostgreSQL (now() + interval), not by Go, so the
// database clock is the only clock that matters: the expiry worker compares
// expires_at with the same database now().
func (r *Repository) CreateReservation(ctx context.Context, orderID int64, line ReserveLine, ttl time.Duration) (Reservation, error) {
	row := r.db.QueryRow(ctx, `
		INSERT INTO inventory_reservations (order_id, product_id, quantity, status, expires_at)
		VALUES ($1, $2, $3, 'ACTIVE', now() + make_interval(secs => $4))
		RETURNING id, order_id, product_id, quantity, status, expires_at`,
		orderID, line.ProductID, line.Quantity, ttl.Seconds(),
	)
	var res Reservation
	if err := row.Scan(&res.ID, &res.OrderID, &res.ProductID, &res.Quantity, &res.Status, &res.ExpiresAt); err != nil {
		return Reservation{}, fmt.Errorf("insert reservation: %w", err)
	}
	return res, nil
}

// ActiveReservationsForUpdate returns the order's ACTIVE reservations,
// locked, ordered by product id (the global lock order; see service.go).
func (r *Repository) ActiveReservationsForUpdate(ctx context.Context, orderID int64) ([]Reservation, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, order_id, product_id, quantity, status, expires_at
		FROM inventory_reservations
		WHERE order_id = $1 AND status = 'ACTIVE'
		ORDER BY product_id
		FOR UPDATE`, orderID)
	if err != nil {
		return nil, fmt.Errorf("select reservations: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Reservation, error) {
		var res Reservation
		err := row.Scan(&res.ID, &res.OrderID, &res.ProductID, &res.Quantity, &res.Status, &res.ExpiresAt)
		return res, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan reservations: %w", err)
	}
	return list, nil
}

// FinishReservation moves one ACTIVE reservation to a final status.
// "AND status = 'ACTIVE'" guarantees a reservation is finished at most once.
func (r *Repository) FinishReservation(ctx context.Context, id int64, to ReservationStatus) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE inventory_reservations SET status = $2, updated_at = now()
		WHERE id = $1 AND status = 'ACTIVE'`, id, to)
	if err != nil {
		return fmt.Errorf("update reservation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("reservation %d is no longer ACTIVE", id)
	}
	return nil
}
