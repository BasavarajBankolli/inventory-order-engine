package payments

import (
	"context"
	"errors"
	"testing"
	"time"

	"inventory-order-engine/internal/money"
)

func charge(t *testing.T, m *MockProvider, ctx context.Context, key string) (Result, error) {
	t.Helper()
	return m.Charge(ctx, ChargeRequest{Amount: money.New(1000, "INR"), IdempotencyKey: key})
}

func TestMock_SuccessAndFailure(t *testing.T) {
	m := NewMockProvider(OutcomeSuccess)

	res, err := charge(t, m, context.Background(), "k1")
	if err != nil || res.Status != ResultSucceeded || res.Reference == "" {
		t.Fatalf("success: %+v, %v", res, err)
	}

	res, err = charge(t, m, WithSimulatedOutcome(context.Background(), OutcomeFailure), "k2")
	if err != nil || res.Status != ResultDeclined || res.FailureReason != "card_declined" {
		t.Fatalf("failure: %+v, %v (a decline is a result, not an error)", res, err)
	}

	if m.Charges() != 1 {
		t.Errorf("charges = %d, want 1 (declines take no money)", m.Charges())
	}
}

// Provider-side idempotency: the same key never charges twice, whatever
// outcome the retry asks for.
func TestMock_IdempotentPerKey(t *testing.T) {
	m := NewMockProvider(OutcomeSuccess)
	first, _ := charge(t, m, context.Background(), "same")
	again, _ := charge(t, m, WithSimulatedOutcome(context.Background(), OutcomeFailure), "same")

	if again != first {
		t.Errorf("retry = %+v, want the original %+v", again, first)
	}
	if m.Charges() != 1 {
		t.Errorf("charges = %d, want 1", m.Charges())
	}
}

// TIMEOUT: the money is taken, but the caller only sees an error. A retry
// with the same key must reveal the success without charging again.
func TestMock_TimeoutThenRetry(t *testing.T) {
	m := NewMockProvider(OutcomeSuccess)

	ctx, cancel := context.WithTimeout(WithSimulatedOutcome(context.Background(), OutcomeTimeout), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := charge(t, m, ctx, "pay-7")

	if !errors.Is(err, ErrProviderUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Error("returned before the deadline")
	}
	if m.Charges() != 1 {
		t.Fatalf("charges = %d, want 1 (TIMEOUT means the charge went through)", m.Charges())
	}

	res, err := charge(t, m, context.Background(), "pay-7")
	if err != nil || res.Status != ResultSucceeded {
		t.Errorf("retry: %+v, %v; want the original success", res, err)
	}
	if m.Charges() != 1 {
		t.Errorf("charges after retry = %d, want still 1", m.Charges())
	}
}

func TestParseOutcome(t *testing.T) {
	for in, want := range map[string]Outcome{"success": OutcomeSuccess, " FAILURE ": OutcomeFailure, "Timeout": OutcomeTimeout} {
		if got, ok := ParseOutcome(in); !ok || got != want {
			t.Errorf("ParseOutcome(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := ParseOutcome("maybe"); ok {
		t.Error(`ParseOutcome("maybe") accepted`)
	}
}
