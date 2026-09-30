package users

import (
	"errors"
	"net/http"

	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/identity"
)

// Handler serves user endpoints. It is thin: read input, call one method,
// map the result to HTTP.
type Handler struct {
	repo *Repository
}

// NewHandler creates a users Handler.
func NewHandler(repo *Repository) *Handler {
	return &Handler{repo: repo}
}

// Me handles GET /api/v1/users/me: the profile of the logged-in user.
//
// The route is protected by auth middleware, so a Principal is always
// present here. We still load the user from the database because the token
// only carries the id and role, not the current name/email.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	p, ok := identity.FromContext(r.Context())
	if !ok {
		// Only reachable if someone forgets the middleware on this route.
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "authentication required")
		return
	}

	u, err := h.repo.GetByID(r.Context(), p.UserID)
	if errors.Is(err, ErrNotFound) {
		// Valid token, but the account no longer exists.
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "user no longer exists")
		return
	}
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, ToResponse(u))
}
