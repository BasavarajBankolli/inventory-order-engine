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

// NewContext returns a copy of ctx carrying p. If an outer layer prepared
// a Slot (see WithSlot), the principal is also recorded there.
func NewContext(ctx context.Context, p Principal) context.Context {
	if s, ok := ctx.Value(slotKey{}).(*slot); ok {
		s.p, s.set = p, true
	}
	return context.WithValue(ctx, ctxKey{}, p)
}

// --- Slot: letting an OUTER middleware learn who the caller was ---------------
//
// The request logger runs before authentication, and contexts only flow
// inwards: the logger cannot see values added by RequireAuth. So the logger
// puts an empty slot into the context; RequireAuth's NewContext fills it;
// after the request, the logger reads the slot to add user_id to the
// request log line.

type slotKey struct{}

type slot struct {
	p   Principal
	set bool
}

// WithSlot returns a context with an empty slot for the caller's identity.
func WithSlot(ctx context.Context) context.Context {
	return context.WithValue(ctx, slotKey{}, &slot{})
}

// FromSlot returns the principal recorded in ctx's slot, if any.
func FromSlot(ctx context.Context) (Principal, bool) {
	s, ok := ctx.Value(slotKey{}).(*slot)
	if !ok || !s.set {
		return Principal{}, false
	}
	return s.p, true
}

// FromContext returns the Principal stored in ctx. ok is false for
// unauthenticated requests (no auth middleware ran, or it rejected them).
func FromContext(ctx context.Context) (p Principal, ok bool) {
	p, ok = ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
