// Package middleware contains HTTP middleware: functions that wrap a handler
// to run code before and/or after every request.
//
// A middleware has the shape:
//
//	func(next http.Handler) http.Handler
//
// and they are chained in the router: RequestID -> Logger -> Recoverer -> handler.
package middleware

import (
	"net/http"

	"inventory-order-engine/internal/requestid"
)

// RequestID makes sure every request has an ID.
//
// If the client (or a load balancer in front of us) already sent a valid
// X-Request-ID header we reuse it, so one ID can follow a request across
// systems. Otherwise we generate a new one. The ID is stored in the request
// context and echoed back in the response header.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestid.Header)
		if !requestid.IsValid(id) {
			id = requestid.New()
		}

		w.Header().Set(requestid.Header, id)
		ctx := requestid.NewContext(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
