// Package health implements the liveness (/health) and readiness (/ready)
// endpoints used by Docker, Kubernetes and load balancers.
//
//	/health  "Is the process alive?"         Never touches dependencies.
//	/ready   "Can it serve real traffic?"   Checks PostgreSQL (and Redis later).
//
// Why two endpoints? If the database goes down, restarting the API will not
// fix it. The orchestrator should stop sending traffic (not ready) but keep
// the process running (still healthy) until the database comes back.
package health

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"inventory-order-engine/internal/httpx"
)

// CheckFunc checks one dependency and returns an error if it is unusable.
// Using a plain function (not an interface) keeps it easy to plug in
// anything: pool.Ping for PostgreSQL today, a Redis ping in a later stage.
type CheckFunc func(ctx context.Context) error

// Handler serves the health endpoints.
type Handler struct {
	checks  map[string]CheckFunc
	timeout time.Duration
}

// NewHandler creates a Handler. checks maps a dependency name
// (e.g. "postgres") to the function that checks it.
func NewHandler(checks map[string]CheckFunc) *Handler {
	return &Handler{checks: checks, timeout: 2 * time.Second}
}

type response struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// Health always returns 200 while the process can answer HTTP at all.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, r, http.StatusOK, response{Status: "ok"})
}

// Ready returns 200 only if every dependency check passes, otherwise 503.
func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	resp := response{Status: "ready", Checks: make(map[string]string, len(h.checks))}

	for name, check := range h.checks {
		// Each check gets its own deadline so a hanging dependency cannot
		// make the readiness probe itself hang.
		ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
		err := check(ctx)
		cancel()

		if err != nil {
			// The real error goes to the logs; the client only learns that
			// the dependency is unavailable (no internal details leaked).
			slog.WarnContext(r.Context(), "readiness check failed", "dependency", name, "error", err)
			resp.Checks[name] = "unavailable"
			resp.Status = "not_ready"
			status = http.StatusServiceUnavailable
			continue
		}
		resp.Checks[name] = "ok"
	}

	httpx.WriteJSON(w, r, status, resp)
}
