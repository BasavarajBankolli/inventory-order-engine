// Package httpx contains small helpers shared by all HTTP handlers so that
// every endpoint returns JSON in exactly the same shape.
//
// Success:  any JSON body chosen by the handler.
// Failure:  {"error": {"code": "...", "message": "...", "request_id": "..."}}
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"inventory-order-engine/internal/requestid"
)

// Machine-readable error codes. Clients should branch on the code, never on
// the human-readable message (messages may change, codes may not).
// More codes (OUT_OF_STOCK, ...) are added in later stages.
const (
	CodeValidation           = "VALIDATION_ERROR"
	CodeUnauthenticated      = "UNAUTHENTICATED"
	CodeInvalidCredentials   = "INVALID_CREDENTIALS"
	CodeForbidden            = "FORBIDDEN"
	CodeNotFound             = "NOT_FOUND"
	CodeMethodNotAllowed     = "METHOD_NOT_ALLOWED"
	CodeEmailTaken           = "EMAIL_ALREADY_EXISTS"
	CodeSKUTaken             = "SKU_ALREADY_EXISTS"
	CodeInsufficientStock    = "INSUFFICIENT_STOCK"
	CodeVersionConflict      = "VERSION_CONFLICT"
	CodeProductUnavailable   = "PRODUCT_UNAVAILABLE"
	CodeOutOfStock           = "OUT_OF_STOCK"
	CodeIdempotencyKeyReused = "IDEMPOTENCY_KEY_REUSED"
	CodePaymentFailed        = "PAYMENT_FAILED"
	CodePaymentTimeout       = "PAYMENT_TIMEOUT"
	CodeReservationExpired   = "RESERVATION_EXPIRED"
	CodeInvalidTransition    = "INVALID_STATE_TRANSITION"
	CodeInternal             = "INTERNAL_ERROR"
)

// ErrorBody is the JSON shape of every error response.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes what went wrong.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Fields lists per-field problems for VALIDATION_ERROR responses,
	// e.g. {"email": "must be a valid email address"}.
	Fields    map[string]string `json:"fields,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
}

// WriteJSON writes v as a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	// By default Go escapes <, > and & as < etc. (useful when JSON is
	// embedded in HTML). This is a JSON API, so keep the text readable.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// The status line is already sent, so we cannot change the response.
		// The best we can do is record the problem.
		slog.ErrorContext(r.Context(), "failed to write JSON response", "error", err)
	}
}

// WriteError writes a consistent JSON error response.
//
// message must be safe to show to clients: never pass raw database or
// internal errors here. Log those instead and send a generic message.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	WriteJSON(w, r, status, ErrorBody{Error: ErrorDetail{
		Code:      code,
		Message:   message,
		RequestID: requestid.FromContext(r.Context()),
	}})
}

// WriteValidationError writes a 400 listing every invalid field.
func WriteValidationError(w http.ResponseWriter, r *http.Request, fields map[string]string) {
	WriteJSON(w, r, http.StatusBadRequest, ErrorBody{Error: ErrorDetail{
		Code:      CodeValidation,
		Message:   "one or more fields are invalid",
		Fields:    fields,
		RequestID: requestid.FromContext(r.Context()),
	}})
}

// WriteInternalError logs the real error (with request_id, via the context)
// and sends the client a generic 500. The client gets the request_id so the
// error can be found in the logs, but never sees internal details such as
// SQL text, table names or stack traces.
func WriteInternalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "internal error",
		"method", r.Method, "path", r.URL.Path, "error", err)
	WriteError(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
}

// NotFound is used for URLs that match no route.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusNotFound, CodeNotFound, "the requested resource was not found")
}

// MethodNotAllowed is used when the URL exists but not for this HTTP method.
func MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed for this resource")
}
