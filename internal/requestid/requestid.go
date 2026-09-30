// Package requestid stores and reads the per-request ID in a context.Context.
//
// Every HTTP request gets a unique ID. It is returned to the client in the
// X-Request-ID header, included in every log line and in every error body,
// so a client-reported error can be matched to the exact server logs.
//
// It lives in its own tiny package so that middleware, logging and HTTP
// helpers can all use it without importing each other (no import cycles).
package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// Header is the HTTP header used to send and receive request IDs.
const Header = "X-Request-ID"

// ctxKey is unexported so no other package can accidentally overwrite
// our value in the context by using the same key.
type ctxKey struct{}

// New generates a random 128-bit ID encoded as 32 hex characters.
func New() string {
	b := make([]byte, 16)
	// crypto/rand.Read never returns an error on supported platforms
	// (it panics instead), so there is nothing to handle here.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewContext returns a copy of ctx that carries the given request ID.
func NewContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the request ID stored in ctx, or "" if there is none.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// IsValid reports whether an ID sent by a client is safe to reuse.
// We only accept short IDs made of letters, digits, '-', '_' and '.', so a
// malicious client cannot inject newlines or huge strings into our logs.
func IsValid(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !isAlnum && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}
