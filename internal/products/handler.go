package products

import (
	"errors"
	"net/http"
	"strconv"

	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/validate"
)

// Handler exposes the products Service over HTTP.
type Handler struct {
	svc *Service
}

// NewHandler creates a products Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// List handles GET /api/v1/products?q=&status=&sort=&limit=&offset=
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit, okLimit := httpx.QueryInt(r, "limit")
	offset, okOffset := httpx.QueryInt(r, "offset")
	if !okLimit || !okOffset {
		v := validate.Errors{}
		v.Check(okLimit, "limit", "must be an integer")
		v.Check(okOffset, "offset", "must be an integer")
		httpx.WriteValidationError(w, r, v)
		return
	}

	res, err := h.svc.List(r.Context(), ListParams{
		Search: q.Get("q"),
		Status: Status(q.Get("status")),
		Sort:   q.Get("sort"),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, res)
}

// Get handles GET /api/v1/products/{id}
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathID(r, "id")
	if !ok {
		writeInvalidID(w, r)
		return
	}

	p, err := h.svc.Get(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, p)
}

type createRequest struct {
	SKU         string `json:"sku"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Price       int64  `json:"price"`
	Currency    string `json:"currency"`
	Status      Status `json:"status"`
}

// Create handles POST /api/v1/products (admin only).
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	p, err := h.svc.Create(r.Context(), CreateInput{
		SKU:         req.SKU,
		Name:        req.Name,
		Description: req.Description,
		Price:       req.Price,
		Currency:    req.Currency,
		Status:      req.Status,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Location tells the client where the new resource lives.
	w.Header().Set("Location", "/api/v1/products/"+strconv.FormatInt(p.ID, 10))
	httpx.WriteJSON(w, r, http.StatusCreated, p)
}

// updateRequest uses pointers so a field that is absent from the JSON stays
// nil, while "description": "" becomes a pointer to an empty string.
type updateRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Price       *int64  `json:"price"`
	Currency    *string `json:"currency"`
	Status      *Status `json:"status"`
}

// Update handles PATCH /api/v1/products/{id} (admin only).
// SKU is not in updateRequest, so sending it is rejected as an unknown
// field: SKUs are permanent identifiers.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathID(r, "id")
	if !ok {
		writeInvalidID(w, r)
		return
	}

	var req updateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	p, err := h.svc.Update(r.Context(), id, UpdateInput{
		Name:        req.Name,
		Description: req.Description,
		Price:       req.Price,
		Currency:    req.Currency,
		Status:      req.Status,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, p)
}

// Delete handles DELETE /api/v1/products/{id} (admin only): archives it.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathID(r, "id")
	if !ok {
		writeInvalidID(w, r)
		return
	}

	if err := h.svc.Archive(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	// 204 No Content: success, and there is nothing to send back.
	w.WriteHeader(http.StatusNoContent)
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var verr validate.Errors
	switch {
	case errors.As(err, &verr):
		httpx.WriteValidationError(w, r, verr)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "product not found")
	case errors.Is(err, ErrSKUTaken):
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeSKUTaken, "a product with this sku already exists")
	default:
		httpx.WriteInternalError(w, r, err)
	}
}

func writeInvalidID(w http.ResponseWriter, r *http.Request) {
	httpx.WriteValidationError(w, r, validate.Errors{"id": "must be a positive integer"})
}
