package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"inventory-order-engine/internal/database"
	"inventory-order-engine/internal/requestid"
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

// MockProvider is a fake payment provider whose "servers" are the
// mock_provider_charges table (see migration 000009), so the API and the
// worker process see the same charges.
//
// Like a real provider it is IDEMPOTENT per IdempotencyKey: the first
// request for a key decides the result; later requests with the same key
// get that stored result back and are NOT charged again.
type MockProvider struct {
	db             database.DBTX
	defaultOutcome Outcome
}

// NewMockProvider creates a mock that uses defaultOutcome unless a request
// context carries a simulated outcome. db is the connection pool: the
// "provider" is an external system, so it never joins our transactions.
func NewMockProvider(db database.DBTX, defaultOutcome Outcome) *MockProvider {
	return &MockProvider{db: db, defaultOutcome: defaultOutcome}
}

// Charge implements Provider.
func (m *MockProvider) Charge(ctx context.Context, req ChargeRequest) (Result, error) {
	outcome := m.defaultOutcome
	if o, ok := ctx.Value(outcomeKey{}).(Outcome); ok {
		outcome = o
	}

	res := Result{Status: ResultSucceeded, Reference: "mock_ch_" + requestid.New()[:16]}
	if outcome == OutcomeFailure {
		res = Result{Status: ResultDeclined, FailureReason: "card_declined"}
	}

	// INSERT ... ON CONFLICT DO NOTHING is the provider's idempotency: only
	// the first request for a key stores a result. If the key already
	// exists, nothing is inserted (no row returned) and we answer with the
	// stored result instead - a retry is never charged twice.
	tag, err := m.db.Exec(ctx, `
		INSERT INTO mock_provider_charges (idempotency_key, status, reference, failure_reason, amount, currency)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		req.IdempotencyKey, res.Status, res.Reference, res.FailureReason, req.Amount.Amount, req.Amount.Currency)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrProviderUnavailable, err)
	}
	if tag.RowsAffected() == 0 {
		return m.Status(ctx, req.IdempotencyKey)
	}

	if outcome == OutcomeTimeout {
		// The charge went through, but we "lose" the response: block until
		// the caller gives up.
		<-ctx.Done()
		return Result{}, fmt.Errorf("%w: %w", ErrProviderUnavailable, ctx.Err())
	}
	return res, nil
}

// Status implements Provider: what happened to the charge with this key?
func (m *MockProvider) Status(ctx context.Context, idempotencyKey string) (Result, error) {
	var res Result
	err := m.db.QueryRow(ctx, `
		SELECT status, COALESCE(reference, ''), COALESCE(failure_reason, '')
		FROM mock_provider_charges WHERE idempotency_key = $1`, idempotencyKey).
		Scan(&res.Status, &res.Reference, &res.FailureReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrChargeNotFound
	}
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrProviderUnavailable, err)
	}
	return res, nil
}

// Charges returns how many times money was actually taken (for tests and
// demos): the number of SUCCEEDED charges.
func (m *MockProvider) Charges(ctx context.Context) (int, error) {
	var n int
	err := m.db.QueryRow(ctx, `SELECT count(*) FROM mock_provider_charges WHERE status = 'SUCCEEDED'`).Scan(&n)
	return n, err
}
