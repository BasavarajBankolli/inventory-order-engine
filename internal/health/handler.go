// Package health implements the liveness (/health) and readiness (/ready)
// endpoints used by Docker, Kubernetes and load balancers.
//
//	/health  "Is the process alive?"         Never touches dependencies.
//	/ready   "Can it serve real traffic?"   Checks PostgreSQL and Redis.
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
type CheckFunc func(ctx context.Context) error

// Check is one dependency to verify on /ready.
type Check struct {
	Name string
	Fn   CheckFunc

	// Required dependencies decide readiness: if PostgreSQL is down the API
	// cannot do anything useful, so /ready returns 503.
	//
	// Optional ones only degrade the service: without Redis the API still
	// works (no cache, no rate limiting), so /ready stays 200 with status
	// "degraded" - taking the API out of rotation would make things WORSE.
	Required bool
}

// Handler serves the health endpoints.
type Handler struct {
	checks  []Check
	timeout time.Duration
}

// NewHandler creates a Handler.
func NewHandler(checks ...Check) *Handler {
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

// Ready returns:
//
//	200 {"status":"ready"}     every dependency is fine
//	200 {"status":"degraded"}  an OPTIONAL dependency is down
//	503 {"status":"not_ready"} a REQUIRED dependency is down
func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	resp := response{Status: "ready", Checks: make(map[string]string, len(h.checks))}

	for _, c := range h.checks {
		// Each check gets its own deadline so a hanging dependency cannot
		// make the readiness probe itself hang.
		ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
		err := c.Fn(ctx)
		cancel()

		if err == nil {
			resp.Checks[c.Name] = "ok"
			continue
		}

		// The real error goes to the logs; the client only learns that the
		// dependency is unavailable (no internal details leaked).
		slog.WarnContext(r.Context(), "readiness check failed",
			"dependency", c.Name, "required", c.Required, "error", err)
		resp.Checks[c.Name] = "unavailable"
		if c.Required {
			resp.Status = "not_ready"
			status = http.StatusServiceUnavailable
		} else if resp.Status == "ready" {
			resp.Status = "degraded"
		}
	}

	httpx.WriteJSON(w, r, status, resp)
}
