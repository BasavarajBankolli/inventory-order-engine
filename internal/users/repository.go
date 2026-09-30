package users

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-order-engine/internal/database"
)

// Repository reads and writes users in PostgreSQL.
// All SQL for the users table lives here and nowhere else.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository creates a Repository using the shared connection pool.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// userColumns is the column list shared by every SELECT/RETURNING below, so
// scanUser always receives the columns in the same order.
const userColumns = `id, email, name, password_hash, role, created_at, updated_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	return u, err
}

// Create inserts a new user and returns it with the database-generated
// fields (id, timestamps) filled in.
//
// It returns ErrEmailTaken if the email already exists (case-insensitive).
// Note there is no "SELECT ... WHERE email = ?" check first: two concurrent
// requests could both pass such a check. The UNIQUE constraint is the only
// reliable guard, so we simply try the INSERT and interpret the error.
func (r *Repository) Create(ctx context.Context, nu NewUser) (User, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO users (email, name, password_hash, role)
		VALUES ($1, $2, $3, $4)
		RETURNING `+userColumns,
		nu.Email, nu.Name, nu.PasswordHash, nu.Role,
	)

	u, err := scanUser(row)
	if err != nil {
		if database.IsUniqueViolation(err, "users_email_key") {
			return User{}, ErrEmailTaken
		}
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	return u, nil
}

// GetByEmail finds a user by email (case-insensitive thanks to CITEXT).
func (r *Repository) GetByEmail(ctx context.Context, email string) (User, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE email = $1`, email)
	return oneUser(row)
}

// GetByID finds a user by primary key.
func (r *Repository) GetByID(ctx context.Context, id int64) (User, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id)
	return oneUser(row)
}

// oneUser converts pgx's "no rows" error into our own ErrNotFound, so
// callers never need to import pgx to understand the result.
func oneUser(row pgx.Row) (User, error) {
	u, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("select user: %w", err)
	}
	return u, nil
}
