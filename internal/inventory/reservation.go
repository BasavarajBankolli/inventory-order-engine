package inventory

import (
	"errors"
	"fmt"
	"time"
)

// Stock movements for reservations. Every method either applies the whole
// change or returns an error and changes nothing.
//
//	                       available   reserved
//	Reserve(3)                -3          +3     order placed
//	ReleaseReserved(3)        +3          -3     order cancelled / reservation expired
//	ConfirmReserved(3)         0          -3     order paid: the units are sold
//
// ReleaseReserved and ConfirmReserved can only fail if the data is already
// inconsistent (reserved smaller than a reservation) - that is a bug, and we
// refuse to make it worse.

// ErrOutOfStock means fewer units are available than requested.
var ErrOutOfStock = errors.New("requested quantity is not available")

// ErrInconsistentReservation signals a bug: a reservation asks to release or
// confirm more units than the inventory row has reserved.
var ErrInconsistentReservation = errors.New("reservation exceeds reserved quantity")

// Reserve moves qty units from available to reserved.
func (inv *Inventory) Reserve(qty int) error {
	if qty <= 0 {
		return fmt.Errorf("reserve: quantity must be positive, got %d", qty)
	}
	if inv.AvailableQuantity < qty {
		return fmt.Errorf("%w: product %d has %d available, %d requested",
			ErrOutOfStock, inv.ProductID, inv.AvailableQuantity, qty)
	}
	inv.AvailableQuantity -= qty
	inv.ReservedQuantity += qty
	return nil
}

// ReleaseReserved moves qty reserved units back to available.
func (inv *Inventory) ReleaseReserved(qty int) error {
	if qty <= 0 || inv.ReservedQuantity < qty {
		return fmt.Errorf("%w: product %d has %d reserved, releasing %d",
			ErrInconsistentReservation, inv.ProductID, inv.ReservedQuantity, qty)
	}
	inv.ReservedQuantity -= qty
	inv.AvailableQuantity += qty
	return nil
}

// ConfirmReserved removes qty units from reserved for good: they are sold.
// Used when payment succeeds (Stage 9).
func (inv *Inventory) ConfirmReserved(qty int) error {
	if qty <= 0 || inv.ReservedQuantity < qty {
		return fmt.Errorf("%w: product %d has %d reserved, confirming %d",
			ErrInconsistentReservation, inv.ProductID, inv.ReservedQuantity, qty)
	}
	inv.ReservedQuantity -= qty
	return nil
}

// ReservationStatus is the state of one reservation.
//
//	ACTIVE ──► CONFIRMED   (paid)
//	   ├─────► RELEASED    (order cancelled)
//	   └─────► EXPIRED     (time ran out)
//
// Only ACTIVE reservations hold stock; the other three are final.
type ReservationStatus string

const (
	ReservationActive    ReservationStatus = "ACTIVE"
	ReservationConfirmed ReservationStatus = "CONFIRMED"
	ReservationReleased  ReservationStatus = "RELEASED"
	ReservationExpired   ReservationStatus = "EXPIRED"
)

// Reservation holds stock of one product for one order.
type Reservation struct {
	ID        int64             `json:"id"`
	OrderID   int64             `json:"order_id"`
	ProductID int64             `json:"product_id"`
	Quantity  int               `json:"quantity"`
	Status    ReservationStatus `json:"status"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// ReserveLine is one product/quantity pair to reserve for an order.
type ReserveLine struct {
	ProductID int64
	Quantity  int
}
