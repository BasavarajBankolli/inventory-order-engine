package payments_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"inventory-order-engine/internal/money"
	"inventory-order-engine/internal/payments"
	"inventory-order-engine/internal/testutil"
)

// The mock stores its charges in PostgreSQL (like a real provider keeps its
// own records), so these are integration tests (need TEST_DATABASE_URL).

func newMock(t *testing.T) *payments.MockProvider {
	t.Helper()
	return payments.NewMockProvider(testutil.NewMigratedPool(t), payments.OutcomeSuccess)
}

func charge(m *payments.MockProvider, ctx context.Context, key string) (payments.Result, error) {
	return m.Charge(ctx, payments.ChargeRequest{Amount: money.New(1000, "INR"), IdempotencyKey: key})
}

func charges(t *testing.T, m *payments.MockProvider) int {
	t.Helper()
	n, err := m.Charges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMock_SuccessAndFailure(t *testing.T) {
	m := newMock(t)

	res, err := charge(m, context.Background(), "k1")
	if err != nil || res.Status != payments.ResultSucceeded || res.Reference == "" {
		t.Fatalf("success: %+v, %v", res, err)
	}

	res, err = charge(m, payments.WithSimulatedOutcome(context.Background(), payments.OutcomeFailure), "k2")
	if err != nil || res.Status != payments.ResultDeclined || res.FailureReason != "card_declined" {
		t.Fatalf("failure: %+v, %v (a decline is a result, not an error)", res, err)
	}

	if n := charges(t, m); n != 1 {
		t.Errorf("charges = %d, want 1 (declines take no money)", n)
	}
}

// Provider-side idempotency: the same key never charges twice, whatever
// outcome the retry asks for.
func TestMock_IdempotentPerKey(t *testing.T) {
	m := newMock(t)
	first, _ := charge(m, context.Background(), "same")
	again, _ := charge(m, payments.WithSimulatedOutcome(context.Background(), payments.OutcomeFailure), "same")

	if again != first {
		t.Errorf("retry = %+v, want the original %+v", again, first)
	}
	if n := charges(t, m); n != 1 {
		t.Errorf("charges = %d, want 1", n)
	}
}

// Many simultaneous requests with one key: still exactly one charge.
func TestMock_ConcurrentSameKey(t *testing.T) {
	m := newMock(t)
	var wg sync.WaitGroup
	refs := make([]string, 20)
	for i := range refs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := charge(m, context.Background(), "hammered")
			if err != nil {
				t.Error(err)
			}
			refs[i] = res.Reference
		}()
	}
	wg.Wait()

	for _, r := range refs {
		if r != refs[0] {
			t.Fatalf("different references returned: %q vs %q", r, refs[0])
		}
	}
	if n := charges(t, m); n != 1 {
		t.Errorf("charges = %d, want 1", n)
	}
}

// TIMEOUT: the money is taken, but the caller only sees an error. A retry
// with the same key (or a Status lookup) reveals the success, no new charge.
func TestMock_TimeoutThenRetryAndStatus(t *testing.T) {
	m := newMock(t)

	ctx, cancel := context.WithTimeout(payments.WithSimulatedOutcome(context.Background(), payments.OutcomeTimeout), 50*time.Millisecond)
	defer cancel()
	_, err := charge(m, ctx, "pay-7")
	if !errors.Is(err, payments.ErrProviderUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	if n := charges(t, m); n != 1 {
		t.Fatalf("charges = %d, want 1 (TIMEOUT means the charge went through)", n)
	}

	status, err := m.Status(context.Background(), "pay-7")
	if err != nil || status.Status != payments.ResultSucceeded {
		t.Errorf("Status() = %+v, %v; want SUCCEEDED", status, err)
	}

	res, err := charge(m, context.Background(), "pay-7")
	if err != nil || res != status {
		t.Errorf("retry: %+v, %v; want the original success", res, err)
	}
	if n := charges(t, m); n != 1 {
		t.Errorf("charges after retry = %d, want still 1", n)
	}
}

func TestMock_StatusOfUnknownKey(t *testing.T) {
	m := newMock(t)
	if _, err := m.Status(context.Background(), "never-sent"); !errors.Is(err, payments.ErrChargeNotFound) {
		t.Errorf("Status(unknown) = %v, want ErrChargeNotFound", err)
	}
}

func TestParseOutcome(t *testing.T) {
	for in, want := range map[string]payments.Outcome{"success": payments.OutcomeSuccess, " FAILURE ": payments.OutcomeFailure, "Timeout": payments.OutcomeTimeout} {
		if got, ok := payments.ParseOutcome(in); !ok || got != want {
			t.Errorf("ParseOutcome(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := payments.ParseOutcome("maybe"); ok {
		t.Error(`ParseOutcome("maybe") accepted`)
	}
}
