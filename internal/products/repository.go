package products

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"inventory-order-engine/internal/database"
)

// Repository contains all SQL for the products table.
type Repository struct {
	db database.DBTX
}

// NewRepository creates a products Repository that runs queries on db
// (normally the connection pool).
func NewRepository(db database.DBTX) *Repository {
	return &Repository{db: db}
}

// WithTx returns a copy of the repository whose queries run inside tx.
func (r *Repository) WithTx(tx pgx.Tx) *Repository {
	return &Repository{db: tx}
}

const productColumns = `id, sku, name, description, price, currency, status, created_at, updated_at`

func scanProduct(row pgx.Row) (Product, error) {
	var p Product
	err := row.Scan(&p.ID, &p.SKU, &p.Name, &p.Description, &p.Price, &p.Currency, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// sortOrders maps the public ?sort= values to ORDER BY clauses.
//
// ORDER BY cannot use $1 placeholders, so user input must NEVER be pasted
// into it. We only ever insert one of these fixed strings (a whitelist),
// which makes SQL injection through ?sort= impossible.
// Every order ends with "id" so rows with equal prices/names always come
// back in the same order; otherwise pages could repeat or skip rows.
var sortOrders = map[string]string{
	"newest":     "id DESC",
	"oldest":     "id ASC",
	"price_asc":  "price ASC, id ASC",
	"price_desc": "price DESC, id DESC",
	"name":       "name ASC, id ASC",
}

// Create inserts a product. Returns ErrSKUTaken if the SKU exists.
func (r *Repository) Create(ctx context.Context, in CreateInput) (Product, error) {
	row := r.db.QueryRow(ctx, `
		INSERT INTO products (sku, name, description, price, currency, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+productColumns,
		in.SKU, in.Name, in.Description, in.Price, in.Currency, in.Status,
	)
	p, err := scanProduct(row)
	if database.IsUniqueViolation(err, "products_sku_key") {
		return Product{}, ErrSKUTaken
	}
	if err != nil {
		return Product{}, fmt.Errorf("insert product: %w", err)
	}
	return p, nil
}

// GetByID returns a non-archived product.
func (r *Repository) GetByID(ctx context.Context, id int64) (Product, error) {
	row := r.db.QueryRow(ctx, `
		SELECT `+productColumns+`
		FROM products
		WHERE id = $1 AND status <> 'ARCHIVED'`, id)
	return oneProduct(row)
}

// List returns one page of products and the total number of matching rows.
//
// The WHERE clause is built from pieces, but every user-supplied VALUE goes
// through a $n placeholder. Only fixed SQL text is concatenated.
func (r *Repository) List(ctx context.Context, p ListParams) ([]Product, int64, error) {
	var (
		where = []string{"status = $1"}
		args  = []any{p.Status}
	)
	if p.Search != "" {
		args = append(args, "%"+escapeLike(p.Search)+"%")
		n := len(args)
		where = append(where, fmt.Sprintf("(name ILIKE $%d OR sku ILIKE $%d)", n, n))
	}
	whereSQL := strings.Join(where, " AND ")

	// Query 1: total count, so clients can show "page 2 of 7".
	var total int64
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM products WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count products: %w", err)
	}

	// Query 2: the requested page.
	orderBy := sortOrders[p.Sort] // validated by the service; see sortOrders
	args = append(args, p.Limit, p.Offset)
	query := fmt.Sprintf(`SELECT %s FROM products WHERE %s ORDER BY %s LIMIT $%d OFFSET $%d`,
		productColumns, whereSQL, orderBy, len(args)-1, len(args))

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list products: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Product, error) {
		return scanProduct(row)
	})
	if err != nil {
		return nil, 0, fmt.Errorf("scan products: %w", err)
	}
	return items, total, nil
}

// Update applies a partial update.
//
// COALESCE($2, name) means "use $2 if it is not NULL, otherwise keep the
// current value". A nil Go pointer is sent as SQL NULL, so fields the client
// did not send stay unchanged, and ONE fixed SQL statement handles every
// combination of fields.
func (r *Repository) Update(ctx context.Context, id int64, in UpdateInput) (Product, error) {
	row := r.db.QueryRow(ctx, `
		UPDATE products SET
			name        = COALESCE($2, name),
			description = COALESCE($3, description),
			price       = COALESCE($4, price),
			currency    = COALESCE($5, currency),
			status      = COALESCE($6, status),
			updated_at  = now()
		WHERE id = $1 AND status <> 'ARCHIVED'
		RETURNING `+productColumns,
		id, in.Name, in.Description, in.Price, in.Currency, in.Status,
	)
	return oneProduct(row)
}

// Archive soft-deletes a product. Archiving twice returns ErrNotFound the
// second time, because an archived product no longer "exists" for the API.
func (r *Repository) Archive(ctx context.Context, id int64) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE products SET status = 'ARCHIVED', updated_at = now()
		WHERE id = $1 AND status <> 'ARCHIVED'`, id)
	if err != nil {
		return fmt.Errorf("archive product: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func oneProduct(row pgx.Row) (Product, error) {
	p, err := scanProduct(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Product{}, ErrNotFound
	}
	if err != nil {
		return Product{}, fmt.Errorf("select product: %w", err)
	}
	return p, nil
}

// escapeLike makes user input match literally inside a LIKE/ILIKE pattern.
// Without it, searching for "50%" or "a_b" would treat % and _ as
// wildcards. Backslash is PostgreSQL's default LIKE escape character.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
