package orders

import (
	"errors"
	"net/http"
	"strconv"

	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/validate"
)

// Handler exposes the orders Service over HTTP. Every route is behind
// RequireAuth, so a Principal is always in the context.
type Handler struct {
	svc *Service
}

// NewHandler creates an orders Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

type createRequest struct {
	Items []struct {
		ProductID int64 `json:"product_id"`
		Quantity  int   `json:"quantity"`
	} `json:"items"`
}

// Create handles POST /api/v1/orders
//
//	{"items": [{"product_id": 2, "quantity": 3}, {"product_id": 5, "quantity": 1}]}
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFrom(w, r)
	if !ok {
		return
	}

	var req createRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}
	items := make([]ItemRequest, len(req.Items))
	for i, it := range req.Items {
		items[i] = ItemRequest{ProductID: it.ProductID, Quantity: it.Quantity}
	}

	o, err := h.svc.Create(r.Context(), caller, items)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/orders/"+strconv.FormatInt(o.ID, 10))
	httpx.WriteJSON(w, r, http.StatusCreated, o)
}

// List handles GET /api/v1/orders?status=&limit=&offset=
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFrom(w, r)
	if !ok {
		return
	}

	limit, okLimit := httpx.QueryInt(r, "limit")
	offset, okOffset := httpx.QueryInt(r, "offset")
	if !okLimit || !okOffset {
		v := validate.Errors{}
		v.Check(okLimit, "limit", "must be an integer")
		v.Check(okOffset, "offset", "must be an integer")
		httpx.WriteValidationError(w, r, v)
		return
	}

	res, err := h.svc.List(r.Context(), caller, ListInput{
		Status: Status(r.URL.Query().Get("status")),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, res)
}

// Get handles GET /api/v1/orders/{id}
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFrom(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathID(r, "id")
	if !ok {
		httpx.WriteValidationError(w, r, validate.Errors{"id": "must be a positive integer"})
		return
	}

	o, err := h.svc.Get(r.Context(), caller, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, o)
}

// Cancel handles POST /api/v1/orders/{id}/cancel
//
// It is a POST to an "action" URL rather than PATCH {"status":"CANCELLED"}:
// clients may only ask for this one specific transition, never set an
// arbitrary status.
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFrom(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathID(r, "id")
	if !ok {
		httpx.WriteValidationError(w, r, validate.Errors{"id": "must be a positive integer"})
		return
	}

	o, err := h.svc.Cancel(r.Context(), caller, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, o)
}

func callerFrom(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	p, ok := identity.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "authentication required")
	}
	return p, ok
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var verr validate.Errors
	switch {
	case errors.As(err, &verr):
		httpx.WriteValidationError(w, r, verr)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "order not found")
	case errors.Is(err, ErrProductUnavailable):
		// err's text is built by us ("...: product 7"), so it is safe to show.
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeProductUnavailable, err.Error())
	case errors.Is(err, ErrInvalidTransition):
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeInvalidTransition, err.Error())
	default:
		httpx.WriteInternalError(w, r, err)
	}
}
