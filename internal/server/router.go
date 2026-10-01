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
	"inventory-order-engine/internal/metrics"
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

	// CORSAllowedOrigins: browser origins allowed to call the API (the
	// frontend). Empty = CORS off.
	CORSAllowedOrigins []string

	// Metrics (Stage 14). nil = no metrics and no /metrics endpoint.
	Metrics *metrics.Metrics

	// MetricsToken, if set, is required as "Authorization: Bearer <token>"
	// on /metrics (for deployments where the endpoint is publicly reachable).
	MetricsToken string

	// Rate limiters (Stage 11). nil = no rate limiting (e.g. no Redis).
	RateLimitByIP   func(http.Handler) http.Handler // public routes
	RateLimitByUser func(http.Handler) http.Handler // authenticated routes
}

// orPassThrough returns mw, or a middleware that does nothing if mw is nil.
func orPassThrough(mw func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	if mw == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return mw
}

// NewRouter builds the complete HTTP handler for the API.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	// Order matters: RequestID runs first so the logger and the recoverer
	// can see the ID; Recoverer sits inside Logger so a panic is logged
	// as a 500 response.
	// CORS first: a browser preflight (OPTIONS) is answered before routing,
	// auth or rate limiting - it carries no token and must not be blocked.
	r.Use(middleware.CORS(d.CORSAllowedOrigins))
	r.Use(middleware.RequestID)
	r.Use(middleware.Logger(d.Logger, d.Metrics))
	r.Use(middleware.Recoverer(d.Logger))

	// Unknown URLs and wrong methods also get our JSON error format.
	r.NotFound(httpx.NotFound)
	r.MethodNotAllowed(httpx.MethodNotAllowed)

	// Operational endpoints live outside /api/v1: they are for
	// infrastructure (Docker, load balancers), not for API clients.
	r.Get("/health", d.Health.Health)
	r.Get("/ready", d.Health.Ready)
	if d.Metrics != nil {
		// Prometheus scrapes this. In production, expose it only to the
		// monitoring network (it reveals internal details), or - where it
		// is reachable from the internet - protect it with METRICS_TOKEN.
		r.With(middleware.StaticBearerToken(d.MetricsToken)).
			Method(http.MethodGet, "/metrics", d.Metrics.Handler())
	}

	// Versioned business API.
	// /health and /ready are NOT rate limited: infrastructure probes them
	// constantly and must always get an answer.
	r.Route("/api/v1", func(r chi.Router) {
		// Public: no token needed. Rate limited per client IP.
		r.Group(func(r chi.Router) {
			r.Use(orPassThrough(d.RateLimitByIP))

			r.Post("/auth/register", d.Auth.Register)
			r.Post("/auth/login", d.Auth.Login)
			r.Get("/products", d.Products.List)
			r.Get("/products/{id}", d.Products.Get)
		})

		// Authenticated: every route in this group needs a valid token.
		// Rate limited per USER (the limiter runs after RequireAuth, so it
		// knows who is calling).
		r.Group(func(r chi.Router) {
			r.Use(d.RequireAuth)
			r.Use(orPassThrough(d.RateLimitByUser))

			r.Get("/users/me", d.Users.Me)

			// Orders: customers see only their own, admins see all
			// (enforced in orders.Service, not here).
			r.Post("/orders", d.Orders.Create)
			r.Get("/orders", d.Orders.List)
			r.Get("/orders/{id}", d.Orders.Get)
			r.Post("/orders/{id}/cancel", d.Orders.Cancel)
			r.Post("/orders/{id}/pay", d.Orders.Pay)

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
