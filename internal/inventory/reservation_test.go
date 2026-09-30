package inventory

import (
	"errors"
	"testing"
)

// Unit tests for the reservation stock movements (no database).

func TestReserve(t *testing.T) {
	inv := Inventory{ProductID: 1, AvailableQuantity: 10}

	if err := inv.Reserve(3); err != nil {
		t.Fatal(err)
	}
	if inv.AvailableQuantity != 7 || inv.ReservedQuantity != 3 {
		t.Errorf("after Reserve(3): %+v, want 7/3", inv)
	}

	// Exactly all remaining stock is fine.
	if err := inv.Reserve(7); err != nil || inv.AvailableQuantity != 0 || inv.ReservedQuantity != 10 {
		t.Errorf("Reserve(7): %v, %+v", err, inv)
	}

	// One more is not, and nothing changes.
	if err := inv.Reserve(1); !errors.Is(err, ErrOutOfStock) {
		t.Errorf("Reserve(1) on empty: %v, want ErrOutOfStock", err)
	}
	if inv.AvailableQuantity != 0 || inv.ReservedQuantity != 10 {
		t.Errorf("failed Reserve changed the inventory: %+v", inv)
	}

	if err := inv.Reserve(0); err == nil {
		t.Error("Reserve(0) must fail")
	}
}

func TestReleaseReserved(t *testing.T) {
	inv := Inventory{ProductID: 1, AvailableQuantity: 7, ReservedQuantity: 3}

	if err := inv.ReleaseReserved(3); err != nil {
		t.Fatal(err)
	}
	if inv.AvailableQuantity != 10 || inv.ReservedQuantity != 0 {
		t.Errorf("after ReleaseReserved(3): %+v, want 10/0", inv)
	}

	// Releasing more than is reserved would create stock out of thin air.
	if err := inv.ReleaseReserved(1); !errors.Is(err, ErrInconsistentReservation) {
		t.Errorf("over-release: %v, want ErrInconsistentReservation", err)
	}
	if inv.AvailableQuantity != 10 || inv.ReservedQuantity != 0 {
		t.Errorf("failed release changed the inventory: %+v", inv)
	}
}

func TestConfirmReserved(t *testing.T) {
	inv := Inventory{ProductID: 1, AvailableQuantity: 7, ReservedQuantity: 3}

	if err := inv.ConfirmReserved(2); err != nil {
		t.Fatal(err)
	}
	// Sold units leave the system: available is untouched.
	if inv.AvailableQuantity != 7 || inv.ReservedQuantity != 1 {
		t.Errorf("after ConfirmReserved(2): %+v, want 7/1", inv)
	}
	if err := inv.ConfirmReserved(2); !errors.Is(err, ErrInconsistentReservation) {
		t.Errorf("over-confirm: %v, want ErrInconsistentReservation", err)
	}
}

// The total number of units (available + reserved) only changes when stock
// is sold (confirm) or adjusted by an admin; reserve/release just move it.
func TestReserveReleaseKeepsTotal(t *testing.T) {
	inv := Inventory{ProductID: 1, AvailableQuantity: 50}
	total := func() int { return inv.AvailableQuantity + inv.ReservedQuantity }

	for _, q := range []int{5, 10, 1} {
		if err := inv.Reserve(q); err != nil {
			t.Fatal(err)
		}
		if total() != 50 {
			t.Fatalf("total changed to %d after Reserve(%d)", total(), q)
		}
	}
	for _, q := range []int{10, 5} {
		if err := inv.ReleaseReserved(q); err != nil {
			t.Fatal(err)
		}
		if total() != 50 {
			t.Fatalf("total changed to %d after ReleaseReserved(%d)", total(), q)
		}
	}
}
