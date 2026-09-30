// Package identity describes WHO is making the current request.
//
// The auth middleware verifies the JWT and stores a Principal in the
// request context. Any handler or service can then ask "who is calling?"
// with identity.FromContext(ctx).
//
// Like requestid, it is a tiny standalone package so that auth, users,
// orders, logging, ... can all import it without import cycles.
package identity

import "context"

// Role controls what a user may do.
type Role string

const (
	// RoleCustomer can browse products and manage their OWN orders.
	RoleCustomer Role = "CUSTOMER"
	// RoleAdmin can additionally manage products and inventory.
	RoleAdmin Role = "ADMIN"
)

// Valid reports whether r is one of the known roles.
func (r Role) Valid() bool {
	return r == RoleCustomer || r == RoleAdmin
}

// Principal is the authenticated caller.
type Principal struct {
	UserID int64
	Role   Role
}

type ctxKey struct{}

// NewContext returns a copy of ctx carrying p.
func NewContext(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the Principal stored in ctx. ok is false for
// unauthenticated requests (no auth middleware ran, or it rejected them).
func FromContext(ctx context.Context) (p Principal, ok bool) {
	p, ok = ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
