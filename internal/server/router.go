// Package server wires HTTP routes to handlers.
//
// This is the only place that knows about every module. Each module exposes
// its handler; the router decides which URL goes where and which middleware
// applies. Reading this file gives you the whole API surface at a glance.
package server

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"inventory-order-engine/internal/auth"
	"inventory-order-engine/internal/health"
	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/middleware"
	"inventory-order-engine/internal/orders"
	"inventory-order-engine/internal/products"
	"inventory-order-engine/internal/users"
)

// Deps lists everything the router needs. New modules add their handlers
// here in later stages.
type Deps struct {
	Logger    *slog.Logger
	Health    *health.Handler
	Auth      *auth.Handler
	Users     *users.Handler
	Products  *products.Handler
	Inventory *inventory.Handler
	Orders    *orders.Handler

	// RequireAuth is the middleware that rejects requests without a valid
	// access token.
	RequireAuth func(http.Handler) http.Handler
}

// NewRouter builds the complete HTTP handler for the API.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	// Order matters: RequestID runs first so the logger and the recoverer
	// can see the ID; Recoverer sits inside Logger so a panic is logged
	// as a 500 response.
	r.Use(middleware.RequestID)
	r.Use(middleware.Logger(d.Logger))
	r.Use(middleware.Recoverer(d.Logger))

	// Unknown URLs and wrong methods also get our JSON error format.
	r.NotFound(httpx.NotFound)
	r.MethodNotAllowed(httpx.MethodNotAllowed)

	// Operational endpoints live outside /api/v1: they are for
	// infrastructure (Docker, load balancers), not for API clients.
	r.Get("/health", d.Health.Health)
	r.Get("/ready", d.Health.Ready)

	// Versioned business API.
	r.Route("/api/v1", func(r chi.Router) {
		// Public: no token needed.
		r.Post("/auth/register", d.Auth.Register)
		r.Post("/auth/login", d.Auth.Login)
		r.Get("/products", d.Products.List)
		r.Get("/products/{id}", d.Products.Get)

		// Authenticated: every route in this group needs a valid token.
		r.Group(func(r chi.Router) {
			r.Use(d.RequireAuth)

			r.Get("/users/me", d.Users.Me)

			// Orders: customers see only their own, admins see all
			// (enforced in orders.Service, not here).
			r.Post("/orders", d.Orders.Create)
			r.Get("/orders", d.Orders.List)
			r.Get("/orders/{id}", d.Orders.Get)
			r.Post("/orders/{id}/cancel", d.Orders.Cancel)

			// Admin only: RequireRole runs after RequireAuth, so it can
			// read the caller's role from the context.
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRole(identity.RoleAdmin))

				r.Post("/products", d.Products.Create)
				r.Patch("/products/{id}", d.Products.Update)
				r.Delete("/products/{id}", d.Products.Delete)

				r.Get("/products/{id}/inventory", d.Inventory.Get)
				r.Patch("/products/{id}/inventory", d.Inventory.Update)
			})
		})
	})

	return r
}
