package inventory

import (
	"errors"
	"testing"

	"inventory-order-engine/internal/validate"
)

// Unit tests for the stock rules (no database).

func TestAdjust(t *testing.T) {
	tests := []struct {
		name      string
		start     int
		delta     int
		want      int
		wantError error
	}{
		{"restock", 10, 5, 15, nil},
		{"remove some", 10, -4, 6, nil},
		{"remove all", 10, -10, 0, nil},
		{"would go negative", 10, -11, 10, ErrInsufficientStock},
		{"from zero", 0, -1, 0, ErrInsufficientStock},
		{"exceeds max", MaxQuantity - 1, 2, MaxQuantity - 1, ErrQuantityTooLarge},
		{"exactly max", MaxQuantity - 1, 1, MaxQuantity, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := Inventory{AvailableQuantity: tt.start, ReservedQuantity: 3}
			err := inv.Adjust(tt.delta)

			if !errors.Is(err, tt.wantError) {
				t.Fatalf("Adjust(%d) error = %v, want %v", tt.delta, err, tt.wantError)
			}
			// On error nothing may change (no half-applied state).
			if inv.AvailableQuantity != tt.want {
				t.Errorf("available = %d, want %d", inv.AvailableQuantity, tt.want)
			}
			if inv.ReservedQuantity != 3 {
				t.Errorf("reserved changed to %d; Adjust must only touch available", inv.ReservedQuantity)
			}
		})
	}
}

func TestSetAvailable(t *testing.T) {
	inv := Inventory{AvailableQuantity: 7}

	if err := inv.SetAvailable(-1); !errors.Is(err, ErrNegativeQuantity) || inv.AvailableQuantity != 7 {
		t.Errorf("SetAvailable(-1) = %v, available %d", err, inv.AvailableQuantity)
	}
	if err := inv.SetAvailable(MaxQuantity + 1); !errors.Is(err, ErrQuantityTooLarge) || inv.AvailableQuantity != 7 {
		t.Errorf("SetAvailable(max+1) = %v, available %d", err, inv.AvailableQuantity)
	}
	if err := inv.SetAvailable(0); err != nil || inv.AvailableQuantity != 0 {
		t.Errorf("SetAvailable(0) = %v, available %d", err, inv.AvailableQuantity)
	}
}

func ptr[T any](v T) *T { return &v }

func TestValidateUpdate(t *testing.T) {
	tests := []struct {
		name      string
		in        UpdateInput
		wantField string // "" = valid
	}{
		{"adjustment", UpdateInput{Adjustment: ptr(5)}, ""},
		{"negative adjustment", UpdateInput{Adjustment: ptr(-5)}, ""},
		{"adjustment with version", UpdateInput{Adjustment: ptr(5), Version: ptr[int64](3)}, ""},
		{"absolute with version", UpdateInput{AvailableQuantity: ptr(100), Version: ptr[int64](3)}, ""},
		{"nothing", UpdateInput{}, "body"},
		{"both", UpdateInput{Adjustment: ptr(1), AvailableQuantity: ptr(1), Version: ptr[int64](1)}, "body"},
		{"zero adjustment", UpdateInput{Adjustment: ptr(0)}, "adjustment"},
		{"huge adjustment", UpdateInput{Adjustment: ptr(maxAdjustment + 1)}, "adjustment"},
		{"absolute without version", UpdateInput{AvailableQuantity: ptr(100)}, "version"},
		{"negative absolute", UpdateInput{AvailableQuantity: ptr(-1), Version: ptr[int64](1)}, "available_quantity"},
		{"version zero", UpdateInput{Adjustment: ptr(1), Version: ptr[int64](0)}, "version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUpdate(tt.in)
			if tt.wantField == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			var verr validate.Errors
			if !errors.As(err, &verr) || verr[tt.wantField] == "" {
				t.Errorf("error = %v, want a problem for %q", err, tt.wantField)
			}
		})
	}
}
