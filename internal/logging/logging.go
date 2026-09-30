// Package logging builds the application's structured (JSON) logger.
//
// Structured logs are key/value pairs instead of free text, e.g.
//
//	{"time":"...","level":"INFO","msg":"order created","request_id":"ab12","order_id":42}
//
// Tools like Loki, Elasticsearch or CloudWatch can then filter by
// request_id or order_id instead of grepping text.
package logging

import (
	"context"
	"io"
	"log/slog"

	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/requestid"
)

// New returns a JSON logger that writes to w at the given minimum level.
//
// The returned logger automatically adds "request_id" (and "user_id" once
// the caller is authenticated) to every log line written with a context
// that carries them (slog.InfoContext(ctx, ...), logger.ErrorContext(ctx, ...)).
func New(w io.Writer, level slog.Level) *slog.Logger {
	jsonHandler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(contextHandler{Handler: jsonHandler})
}

// contextHandler wraps another slog.Handler and copies well-known values
// from the context into each log record.
type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := requestid.FromContext(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	if p, ok := identity.FromContext(ctx); ok {
		r.AddAttrs(slog.Int64("user_id", p.UserID))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must be overridden so that loggers derived with
// logger.With(...) keep the request_id behaviour.
func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{Handler: h.Handler.WithGroup(name)}
}
