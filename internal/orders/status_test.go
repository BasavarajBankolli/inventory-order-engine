package orders

import (
	"errors"
	"testing"
)

var allStatuses = []Status{
	StatusCreated, StatusReserved, StatusPaymentPending, StatusPaymentFailed, StatusConfirmed,
	StatusProcessing, StatusShipped, StatusDelivered, StatusCancelled, StatusExpired,
}

// The complete list of legal transitions, written out independently of the
// transitions map. The test checks EVERY one of the 10 x 10 = 100 pairs, so
// adding or removing a transition by accident always breaks this test.
var wantAllowed = map[[2]Status]bool{
	{StatusCreated, StatusReserved}:             true,
	{StatusCreated, StatusCancelled}:            true,
	{StatusReserved, StatusPaymentPending}:      true,
	{StatusReserved, StatusCancelled}:           true,
	{StatusReserved, StatusExpired}:             true,
	{StatusPaymentPending, StatusConfirmed}:     true,
	{StatusPaymentPending, StatusPaymentFailed}: true,
	{StatusPaymentPending, StatusExpired}:       true,
	{StatusPaymentFailed, StatusCancelled}:      true,
	{StatusConfirmed, StatusProcessing}:         true,
	{StatusProcessing, StatusShipped}:           true,
	{StatusShipped, StatusDelivered}:            true,
}

func TestCanTransition_AllPairs(t *testing.T) {
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			want := wantAllowed[[2]Status{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s -> %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestTransitionTo(t *testing.T) {
	o := Order{Status: StatusCreated}

	// Happy path all the way to DELIVERED.
	for _, next := range []Status{StatusReserved, StatusPaymentPending, StatusConfirmed, StatusProcessing, StatusShipped, StatusDelivered} {
		if err := o.TransitionTo(next); err != nil {
			t.Fatalf("TransitionTo(%s) error = %v", next, err)
		}
	}

	// Invalid move: error, and the status must not change.
	err := o.TransitionTo(StatusCancelled)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("DELIVERED -> CANCELLED error = %v, want ErrInvalidTransition", err)
	}
	if o.Status != StatusDelivered {
		t.Errorf("status changed to %s after a rejected transition", o.Status)
	}
}

func TestTransitionTo_Examples(t *testing.T) {
	tests := []struct {
		from, to Status
		ok       bool
	}{
		{StatusCreated, StatusConfirmed, false},        // cannot skip reservation and payment
		{StatusPaymentPending, StatusCancelled, false}, // payment in flight
		{StatusPaymentFailed, StatusCancelled, true},   // failure path
		{StatusReserved, StatusExpired, true},          // reservation timed out
		{StatusCancelled, StatusCreated, false},        // cannot un-cancel
		{StatusShipped, StatusShipped, false},          // no self-loops
	}
	for _, tt := range tests {
		o := Order{Status: tt.from}
		err := o.TransitionTo(tt.to)
		if (err == nil) != tt.ok {
			t.Errorf("%s -> %s: error = %v, want ok=%v", tt.from, tt.to, err, tt.ok)
		}
	}
}

func TestIsFinal(t *testing.T) {
	final := map[Status]bool{StatusDelivered: true, StatusCancelled: true, StatusExpired: true}
	for _, s := range allStatuses {
		if s.IsFinal() != final[s] {
			t.Errorf("%s.IsFinal() = %v, want %v", s, s.IsFinal(), final[s])
		}
	}
}

func TestIsKnownStatus(t *testing.T) {
	for _, s := range allStatuses {
		if !isKnownStatus(s) {
			t.Errorf("isKnownStatus(%s) = false", s)
		}
	}
	if isKnownStatus("SHIPPING") {
		t.Error(`isKnownStatus("SHIPPING") = true`)
	}
}
