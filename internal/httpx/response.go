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
// More codes (OUT_OF_STOCK, CONFLICT, ...) are added in later stages.
const (
	CodeValidation       = "VALIDATION_ERROR"
	CodeNotFound         = "NOT_FOUND"
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	CodeInternal         = "INTERNAL_ERROR"
)

// ErrorBody is the JSON shape of every error response.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes what went wrong.
type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteJSON writes v as a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
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

// NotFound is used for URLs that match no route.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusNotFound, CodeNotFound, "the requested resource was not found")
}

// MethodNotAllowed is used when the URL exists but not for this HTTP method.
func MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed for this resource")
}
