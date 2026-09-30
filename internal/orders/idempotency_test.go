package orders

import (
	"errors"
	"strings"
	"testing"

	"inventory-order-engine/internal/validate"
)

// Unit tests for the idempotency helpers (no database).

func TestFingerprint(t *testing.T) {
	a := fingerprint([]ItemRequest{{ProductID: 1, Quantity: 2}, {ProductID: 7, Quantity: 1}})

	// Same basket, different line order: same fingerprint.
	b := fingerprint([]ItemRequest{{ProductID: 7, Quantity: 1}, {ProductID: 1, Quantity: 2}})
	if a != b {
		t.Error("line order changed the fingerprint")
	}

	// Anything that changes WHAT is ordered changes the fingerprint.
	for name, items := range map[string][]ItemRequest{
		"quantity":   {{ProductID: 1, Quantity: 3}, {ProductID: 7, Quantity: 1}},
		"product":    {{ProductID: 1, Quantity: 2}, {ProductID: 8, Quantity: 1}},
		"extra line": {{ProductID: 1, Quantity: 2}, {ProductID: 7, Quantity: 1}, {ProductID: 9, Quantity: 1}},
		"ambiguity":  {{ProductID: 12, Quantity: 1}, {ProductID: 7, Quantity: 1}}, // "1:2" vs "12:1"
	} {
		if fingerprint(items) == a {
			t.Errorf("%s: fingerprint did not change", name)
		}
	}

	if len(a) != 64 {
		t.Errorf("fingerprint length = %d, want 64 hex chars (SHA-256)", len(a))
	}
}

func TestValidateIdempotencyKey(t *testing.T) {
	valid := []string{"", "3f0c7b8e-1d2a-4c55-9e1b-0a6f2d3c4b5a", "order-2026-09-30#1", strings.Repeat("k", 255)}
	for _, k := range valid {
		if err := validateIdempotencyKey(k); err != nil {
			t.Errorf("validateIdempotencyKey(%q) = %v, want nil", k, err)
		}
	}

	invalid := []string{strings.Repeat("k", 256), "has space", "tab\tkey", "new\nline", "émoji-✓"}
	for _, k := range invalid {
		var verr validate.Errors
		if err := validateIdempotencyKey(k); !errors.As(err, &verr) {
			t.Errorf("validateIdempotencyKey(%q) = %v, want a validation error", k, err)
		}
	}
}
