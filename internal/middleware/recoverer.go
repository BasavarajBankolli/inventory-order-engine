package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"inventory-order-engine/internal/httpx"
)

// Recoverer catches a panic in any handler, logs it with a stack trace and
// returns a JSON 500 instead of dropping the connection.
//
// A panic is always a bug. We still want one buggy request to fail cleanly
// without leaking internals (the stack trace goes to logs, never to clients).
func Recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is Go's way of deliberately aborting a
				// response; re-panic so net/http handles it as intended.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}

				logger.ErrorContext(r.Context(), "panic recovered",
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())),
				)
				httpx.WriteError(w, r, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
			}()

			next.ServeHTTP(w, r)
		})
	}
}
