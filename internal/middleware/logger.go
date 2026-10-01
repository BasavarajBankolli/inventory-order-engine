package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/metrics"
)

// statusRecorder wraps http.ResponseWriter to remember the status code and
// number of bytes written, because the standard ResponseWriter does not
// let us read them back after the handler finishes.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	// If a handler calls Write without WriteHeader, Go sends 200 OK.
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Logger writes one structured log line per request after it completes and
// records the request in the HTTP metrics (m may be nil: no metrics).
//
// We log the path but NOT the query string, headers or body: those can
// contain tokens, passwords or personal data.
func Logger(logger *slog.Logger, m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}

			// Empty slot that RequireAuth fills, so this OUTER middleware
			// can log who the caller was (see identity.WithSlot).
			r = r.WithContext(identity.WithSlot(r.Context()))

			next.ServeHTTP(rec, r)

			if rec.status == 0 { // handler wrote nothing at all
				rec.status = http.StatusOK
			}
			elapsed := time.Since(start)
			route := routePattern(r)

			m.ObserveHTTP(r.Method, route, rec.status, elapsed.Seconds())

			level := slog.LevelInfo
			if rec.status >= 500 {
				level = slog.LevelError
			}
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("route", route),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
				slog.String("remote_addr", r.RemoteAddr),
			}
			if p, ok := identity.FromSlot(r.Context()); ok {
				attrs = append(attrs, slog.Int64("user_id", p.UserID))
			}
			logger.LogAttrs(r.Context(), level, "http request", attrs...)
		})
	}
}

// routePattern returns the matched chi route, e.g. "/api/v1/orders/{id}",
// never the raw path "/api/v1/orders/123". Metrics labelled by raw path
// would create a new time series for every order id. Requests that matched
// no route are grouped as "unmatched" (a scanner probing random URLs must
// not create thousands of series either).
func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if p := rctx.RoutePattern(); p != "" {
			return p
		}
	}
	return "unmatched"
}
