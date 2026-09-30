package orders

import (
	"errors"
	"fmt"
)

// Status is a state in the order state machine.
type Status string

const (
	StatusCreated        Status = "CREATED"         // order rows written
	StatusReserved       Status = "RESERVED"        // stock held for this order (Stage 6)
	StatusPaymentPending Status = "PAYMENT_PENDING" // payment in progress (Stage 9)
	StatusPaymentFailed  Status = "PAYMENT_FAILED"  // payment declined / timed out
	StatusConfirmed      Status = "CONFIRMED"       // paid; stock is sold
	StatusProcessing     Status = "PROCESSING"      // warehouse is packing it
	StatusShipped        Status = "SHIPPED"
	StatusDelivered      Status = "DELIVERED" // final: happy path
	StatusCancelled      Status = "CANCELLED" // final: cancelled by customer/admin/system
	StatusExpired        Status = "EXPIRED"   // final: reservation ran out (Stage 10)
)

// ErrInvalidTransition is returned when a status change is not allowed.
var ErrInvalidTransition = errors.New("invalid order status transition")

// transitions is THE state machine: for each status, the statuses it may
// move to. Anything not listed is forbidden. Keeping it as data (instead of
// if-statements spread over the code) means the whole lifecycle can be read
// and reviewed in one place.
//
//	CREATED ──► RESERVED ──► PAYMENT_PENDING ──► CONFIRMED ──► PROCESSING ──► SHIPPED ──► DELIVERED
//	   │           │  │            │   │
//	   │           │  └─► EXPIRED ◄┘   └─► PAYMENT_FAILED ──► CANCELLED
//	   └───────────┴──────────────────────────────────────────► CANCELLED
//
// PAYMENT_PENDING cannot go to CANCELLED on purpose: a payment is in
// flight, and cancelling now could race with a successful charge (the money
// is taken but the order is cancelled). Stage 9 deals with this.
var transitions = map[Status][]Status{
	StatusCreated:        {StatusReserved, StatusCancelled},
	StatusReserved:       {StatusPaymentPending, StatusCancelled, StatusExpired},
	StatusPaymentPending: {StatusConfirmed, StatusPaymentFailed, StatusExpired}, // no CANCELLED: see above
	StatusPaymentFailed:  {StatusCancelled},
	StatusConfirmed:      {StatusProcessing},
	StatusProcessing:     {StatusShipped},
	StatusShipped:        {StatusDelivered},
	// DELIVERED, CANCELLED and EXPIRED are final: no outgoing transitions.
}

// CanTransition reports whether an order may move from one status to another.
func CanTransition(from, to Status) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// IsFinal reports whether no further transitions are possible.
func (s Status) IsFinal() bool {
	return len(transitions[s]) == 0
}

// TransitionTo moves the order to a new status, or returns
// ErrInvalidTransition and leaves the order unchanged.
//
// This is the ONLY way code should change Order.Status. Nothing else in the
// codebase assigns the Status field of an existing order directly.
func (o *Order) TransitionTo(to Status) error {
	if !CanTransition(o.Status, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, o.Status, to)
	}
	o.Status = to
	return nil
}
