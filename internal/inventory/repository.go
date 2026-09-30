package inventory

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"inventory-order-engine/internal/database"
)

// Repository contains all SQL for the inventory table.
type Repository struct {
	db database.DBTX
}

// NewRepository creates a Repository that runs queries on db (normally the
// connection pool).
func NewRepository(db database.DBTX) *Repository {
	return &Repository{db: db}
}

// WithTx returns a copy of the repository whose queries run inside tx.
//
//	database.WithTx(ctx, pool, func(tx pgx.Tx) error {
//	    inv, err := repo.WithTx(tx).GetForUpdate(ctx, productID)
//	    ...
//	})
func (r *Repository) WithTx(tx pgx.Tx) *Repository {
	return &Repository{db: tx}
}

const inventoryColumns = `i.product_id, i.available_quantity, i.reserved_quantity, i.version, i.updated_at`

func scanInventory(row pgx.Row) (Inventory, error) {
	var inv Inventory
	err := row.Scan(&inv.ProductID, &inv.AvailableQuantity, &inv.ReservedQuantity, &inv.Version, &inv.UpdatedAt)
	return inv, err
}

// CreateForProduct inserts the empty inventory row for a new product.
// It must run in the same transaction that inserts the product.
func (r *Repository) CreateForProduct(ctx context.Context, productID int64) error {
	if _, err := r.db.Exec(ctx, `INSERT INTO inventory (product_id) VALUES ($1)`, productID); err != nil {
		return fmt.Errorf("insert inventory: %w", err)
	}
	return nil
}

// Get reads the inventory of a non-archived product WITHOUT locking it.
// Fine for displaying; never use it to decide on a change (see GetForUpdate).
func (r *Repository) Get(ctx context.Context, productID int64) (Inventory, error) {
	row := r.db.QueryRow(ctx, `
		SELECT `+inventoryColumns+`
		FROM inventory i
		JOIN products p ON p.id = i.product_id
		WHERE i.product_id = $1 AND p.status <> 'ARCHIVED'`, productID)
	return oneInventory(row)
}

// GetForUpdate reads the inventory row AND locks it until the surrounding
// transaction ends. It must be called on a repository created with WithTx.
//
// FOR UPDATE is a pessimistic row lock: any other transaction that tries to
// lock (or update) the same row WAITS here until we COMMIT or ROLLBACK, and
// then sees our new values. That turns concurrent "read, calculate, write"
// sequences into a queue, so no update is ever lost.
//
// "FOR UPDATE OF i" locks only the inventory row, not the joined product.
func (r *Repository) GetForUpdate(ctx context.Context, productID int64) (Inventory, error) {
	row := r.db.QueryRow(ctx, `
		SELECT `+inventoryColumns+`
		FROM inventory i
		JOIN products p ON p.id = i.product_id
		WHERE i.product_id = $1 AND p.status <> 'ARCHIVED'
		FOR UPDATE OF i`, productID)
	return oneInventory(row)
}

// Save writes the quantities back and increments the version.
//
// "AND version = $4" is a second safety net: if the row changed since it
// was read (which a FOR UPDATE lock already prevents), no row matches and
// we report a conflict instead of silently overwriting someone's change.
func (r *Repository) Save(ctx context.Context, inv Inventory) (Inventory, error) {
	row := r.db.QueryRow(ctx, `
		UPDATE inventory i SET
			available_quantity = $2,
			reserved_quantity  = $3,
			version            = i.version + 1,
			updated_at         = now()
		WHERE i.product_id = $1 AND i.version = $4
		RETURNING `+inventoryColumns,
		inv.ProductID, inv.AvailableQuantity, inv.ReservedQuantity, inv.Version,
	)
	saved, err := scanInventory(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Inventory{}, ErrVersionConflict
	}
	if err != nil {
		return Inventory{}, fmt.Errorf("update inventory: %w", err)
	}
	return saved, nil
}

func oneInventory(row pgx.Row) (Inventory, error) {
	inv, err := scanInventory(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Inventory{}, ErrNotFound
	}
	if err != nil {
		return Inventory{}, fmt.Errorf("select inventory: %w", err)
	}
	return inv, nil
}
