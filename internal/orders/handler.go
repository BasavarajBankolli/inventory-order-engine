package orders

import (
	"errors"
	"net/http"
	"strconv"

	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/payments"
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
//	Idempotency-Key: 3f0c7b8e-...        (optional, recommended)
//	{"items": [{"product_id": 2, "quantity": 3}, {"product_id": 5, "quantity": 1}]}
//
// 201 Created = a new order. 200 OK + "Idempotent-Replayed: true" = this key
// was already used for the same request; the body is the ORIGINAL order.
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

	key := r.Header.Get(IdempotencyKeyHeader)
	o, replayed, err := h.svc.CreateWithKey(r.Context(), caller, items, key)
	if err != nil {
		writeError(w, r, err)
		return
	}

	w.Header().Set("Location", "/api/v1/orders/"+strconv.FormatInt(o.ID, 10))
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		httpx.WriteJSON(w, r, http.StatusOK, o)
		return
	}
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

type payRequest struct {
	// Simulate asks the MOCK payment provider for a specific outcome:
	// "SUCCESS", "FAILURE" or "TIMEOUT". Testing/demo only; a real provider
	// would ignore it.
	Simulate string `json:"simulate"`
}

// Pay handles POST /api/v1/orders/{id}/pay
//
//	(empty body)                    use the mock's default outcome
//	{"simulate": "FAILURE"}         force a decline
//	{"simulate": "TIMEOUT"}         charge succeeds but the answer is lost
//
// 200 = paid (order CONFIRMED). 402 = declined (order CANCELLED, stock
// released). 504 = no answer from the provider (order still
// PAYMENT_PENDING; call this endpoint again - it will not charge twice).
func (h *Handler) Pay(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFrom(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathID(r, "id")
	if !ok {
		httpx.WriteValidationError(w, r, validate.Errors{"id": "must be a positive integer"})
		return
	}

	ctx := r.Context()
	if r.ContentLength != 0 { // the body is optional
		var req payRequest
		if err := httpx.DecodeJSON(w, r, &req); err != nil {
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidation, err.Error())
			return
		}
		if req.Simulate != "" {
			outcome, ok := payments.ParseOutcome(req.Simulate)
			if !ok {
				httpx.WriteValidationError(w, r, validate.Errors{"simulate": "must be SUCCESS, FAILURE or TIMEOUT"})
				return
			}
			ctx = payments.WithSimulatedOutcome(ctx, outcome)
		}
	}

	res, err := h.svc.Pay(ctx, caller, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, res)
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
	case errors.Is(err, inventory.ErrOutOfStock):
		// The message names the product and quantities; it contains only
		// numbers we computed, so it is safe to return.
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeOutOfStock, err.Error())
	case errors.Is(err, ErrIdempotencyKeyReused):
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeIdempotencyKeyReused,
			"this Idempotency-Key was already used for a different request; use a new key for a new order")
	case errors.Is(err, ErrPaymentDeclined):
		httpx.WriteError(w, r, http.StatusPaymentRequired, httpx.CodePaymentFailed,
			err.Error()+"; the order was cancelled and its stock released")
	case errors.Is(err, ErrPaymentOutcomeUnknown):
		httpx.WriteError(w, r, http.StatusGatewayTimeout, httpx.CodePaymentTimeout,
			"the payment provider did not answer in time; the order is still PAYMENT_PENDING. "+
				"Retry this request - you will not be charged twice")
	case errors.Is(err, ErrReservationExpired):
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeReservationExpired,
			"the stock reservation for this order has expired; please place a new order")
	case errors.Is(err, ErrInvalidTransition):
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeInvalidTransition, err.Error())
	default:
		httpx.WriteInternalError(w, r, err)
	}
}
