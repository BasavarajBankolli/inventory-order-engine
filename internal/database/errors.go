package database

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// uniqueViolation is PostgreSQL's SQLSTATE code for "duplicate key value
// violates unique constraint". Full list:
// https://www.postgresql.org/docs/current/errcodes-appendix.html
const uniqueViolation = "23505"

// IsUniqueViolation reports whether err was caused by the named UNIQUE
// constraint (e.g. "users_email_key").
//
// Checking the constraint name matters: a table can have several unique
// constraints and each one means something different to the caller.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == uniqueViolation &&
		pgErr.ConstraintName == constraint
}
