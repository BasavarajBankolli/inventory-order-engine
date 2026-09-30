package payments

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Outcome tells the mock how to behave for a charge.
type Outcome string

const (
	// OutcomeSuccess: the charge succeeds.
	OutcomeSuccess Outcome = "SUCCESS"
	// OutcomeFailure: the card is declined.
	OutcomeFailure Outcome = "FAILURE"
	// OutcomeTimeout: the charge SUCCEEDS at the provider, but the answer
	// never reaches us - we wait until our context deadline expires. This
	// is the nastiest real-world case: money taken, caller does not know.
	OutcomeTimeout Outcome = "TIMEOUT"
)

// ParseOutcome converts user input ("success", "FAILURE", ...) to an Outcome.
func ParseOutcome(s string) (Outcome, bool) {
	switch o := Outcome(strings.ToUpper(strings.TrimSpace(s))); o {
	case OutcomeSuccess, OutcomeFailure, OutcomeTimeout:
		return o, true
	}
	return "", false
}

type outcomeKey struct{}

// WithSimulatedOutcome returns a context that asks the MOCK provider for a
// specific outcome. It is a testing/demo hook: a real provider ignores it.
func WithSimulatedOutcome(ctx context.Context, o Outcome) context.Context {
	return context.WithValue(ctx, outcomeKey{}, o)
}

// MockProvider is an in-memory fake payment provider.
//
// Like a real provider it is IDEMPOTENT per IdempotencyKey: the first
// request for a key decides the result, later requests with the same key
// get that stored result back and are NOT charged again. Its memory is lost
// when the process restarts - fine for a mock.
type MockProvider struct {
	defaultOutcome Outcome

	mu        sync.Mutex
	processed map[string]Result // idempotency key -> result
	charges   int               // how many times money was actually taken
}

// NewMockProvider creates a mock that uses defaultOutcome unless a request
// context carries a simulated outcome.
func NewMockProvider(defaultOutcome Outcome) *MockProvider {
	return &MockProvider{defaultOutcome: defaultOutcome, processed: map[string]Result{}}
}

// Charge implements Provider.
func (m *MockProvider) Charge(ctx context.Context, req ChargeRequest) (Result, error) {
	outcome := m.defaultOutcome
	if o, ok := ctx.Value(outcomeKey{}).(Outcome); ok {
		outcome = o
	}

	m.mu.Lock()
	// Provider-side idempotency: a retry gets the original answer.
	if res, seen := m.processed[req.IdempotencyKey]; seen {
		m.mu.Unlock()
		return res, nil
	}

	var res Result
	switch outcome {
	case OutcomeFailure:
		res = Result{Status: ResultDeclined, FailureReason: "card_declined"}
	default: // SUCCESS and TIMEOUT both take the money
		m.charges++
		res = Result{
			Status:    ResultSucceeded,
			Reference: fmt.Sprintf("mock_ch_%d_%d", time.Now().UnixNano(), m.charges),
		}
	}
	m.processed[req.IdempotencyKey] = res
	m.mu.Unlock()

	if outcome == OutcomeTimeout {
		// The charge went through, but we "lose" the response: block until
		// the caller gives up.
		<-ctx.Done()
		return Result{}, fmt.Errorf("%w: %w", ErrProviderUnavailable, ctx.Err())
	}
	return res, nil
}

// Charges returns how many times money was actually taken (for tests).
func (m *MockProvider) Charges() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.charges
}
