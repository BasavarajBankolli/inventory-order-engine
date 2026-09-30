package inventory

import (
	"errors"
	"net/http"

	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/validate"
)

// Handler exposes inventory endpoints (admin only, see router.go).
type Handler struct {
	svc *Service
}

// NewHandler creates an inventory Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Get handles GET /api/v1/products/{id}/inventory
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	productID, ok := httpx.PathID(r, "id")
	if !ok {
		httpx.WriteValidationError(w, r, validate.Errors{"id": "must be a positive integer"})
		return
	}

	inv, err := h.svc.Get(r.Context(), productID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, inv)
}

type updateRequest struct {
	Adjustment        *int   `json:"adjustment"`
	AvailableQuantity *int   `json:"available_quantity"`
	Version           *int64 `json:"version"`
}

// Update handles PATCH /api/v1/products/{id}/inventory
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	productID, ok := httpx.PathID(r, "id")
	if !ok {
		httpx.WriteValidationError(w, r, validate.Errors{"id": "must be a positive integer"})
		return
	}

	var req updateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	inv, err := h.svc.Update(r.Context(), productID, UpdateInput{
		Adjustment:        req.Adjustment,
		AvailableQuantity: req.AvailableQuantity,
		Version:           req.Version,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, inv)
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var verr validate.Errors
	switch {
	case errors.As(err, &verr):
		httpx.WriteValidationError(w, r, verr)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "product not found")
	case errors.Is(err, ErrInsufficientStock):
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeInsufficientStock,
			"adjustment would make available stock negative")
	case errors.Is(err, ErrVersionConflict):
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeVersionConflict,
			"inventory was changed by someone else; GET it again and retry with the new version")
	case errors.Is(err, ErrQuantityTooLarge), errors.Is(err, ErrNegativeQuantity):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidation, err.Error())
	default:
		httpx.WriteInternalError(w, r, err)
	}
}
