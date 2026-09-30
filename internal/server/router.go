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

	"inventory-order-engine/internal/health"
	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/middleware"
)

// Deps lists everything the router needs. New modules (auth, products,
// orders, ...) add their handlers here in later stages.
type Deps struct {
	Logger *slog.Logger
	Health *health.Handler
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

	// The versioned business API (/api/v1/...) is added from Stage 2 onwards.

	return r
}
