package orders

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"inventory-order-engine/internal/validate"
)

// Idempotency: "doing it twice has the same effect as doing it once".
//
// Networks fail in the worst possible place: the server creates order #123,
// but the response is lost, so the client never learns about it and sends
// the request again. Without protection, the customer now has two orders
// and double-reserved stock. With an Idempotency-Key, the retry gets
// order #123 back.

// IdempotencyKeyHeader is the HTTP header clients use to send the key.
const IdempotencyKeyHeader = "Idempotency-Key"

// maxIdempotencyKeyLen matches the CHECK constraint in migration 000007.
const maxIdempotencyKeyLen = 255

var (
	// ErrIdempotencyKeyReused: the key was already used for a DIFFERENT
	// request. Replaying the old order would silently give the client
	// something it did not ask for, so we refuse instead.
	ErrIdempotencyKeyReused = errors.New("idempotency key was already used for a different request")

	// errDuplicateIdempotencyKey is internal: our INSERT lost the race to a
	// concurrent request with the same key. The service turns it into a replay.
	errDuplicateIdempotencyKey = errors.New("duplicate idempotency key")
)

// validateIdempotencyKey accepts 1-255 visible ASCII characters (a UUID is
// the typical choice). An empty key means "no idempotency requested".
func validateIdempotencyKey(key string) error {
	if key == "" {
		return nil
	}
	v := validate.Errors{}
	if len(key) > maxIdempotencyKeyLen {
		v.Add("Idempotency-Key", fmt.Sprintf("must be at most %d characters", maxIdempotencyKeyLen))
	}
	for _, c := range key {
		if c < '!' || c > '~' { // outside printable ASCII, or a space
			v.Add("Idempotency-Key", "must contain only visible ASCII characters (no spaces)")
			break
		}
	}
	return v.Err()
}

// fingerprint returns a SHA-256 hash that identifies WHAT was ordered.
//
// Lines are sorted by product id first, so the same basket listed in a
// different order ([mug, pen] vs [pen, mug]) gives the same fingerprint.
func fingerprint(items []ItemRequest) string {
	sorted := slices.Clone(items)
	slices.SortFunc(sorted, func(a, b ItemRequest) int { return cmp.Compare(a.ProductID, b.ProductID) })

	var b strings.Builder
	for _, it := range sorted {
		fmt.Fprintf(&b, "%d:%d;", it.ProductID, it.Quantity)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
