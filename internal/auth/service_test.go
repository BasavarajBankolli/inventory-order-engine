package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/users"
	"inventory-order-engine/internal/validate"
)

// fakeStore is an in-memory UserStore. Because Service depends on the
// UserStore interface (not on *users.Repository), these tests run without
// a database. The repository itself is tested against real PostgreSQL in
// internal/users.
type fakeStore struct {
	mu     sync.Mutex
	byID   map[int64]users.User
	nextID int64
	err    error // if set, every call fails with it (simulates a DB outage)
}

func newFakeStore() *fakeStore {
	return &fakeStore{byID: map[int64]users.User{}, nextID: 1}
}

func (f *fakeStore) Create(_ context.Context, nu users.NewUser) (users.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return users.User{}, f.err
	}
	for _, u := range f.byID {
		if strings.EqualFold(u.Email, nu.Email) { // mimic CITEXT UNIQUE
			return users.User{}, users.ErrEmailTaken
		}
	}
	u := users.User{ID: f.nextID, Email: nu.Email, Name: nu.Name, PasswordHash: nu.PasswordHash, Role: nu.Role, CreatedAt: time.Now()}
	f.byID[u.ID] = u
	f.nextID++
	return u, nil
}

func (f *fakeStore) GetByEmail(_ context.Context, email string) (users.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return users.User{}, f.err
	}
	for _, u := range f.byID {
		if strings.EqualFold(u.Email, email) {
			return u, nil
		}
	}
	return users.User{}, users.ErrNotFound
}

func newTestService(t *testing.T, store UserStore) *Service {
	t.Helper()
	svc, err := NewService(store, NewTokenManager(testSecret, time.Hour), NewPasswordHasher(bcrypt.MinCost))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestRegister_Success(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(t, store)

	u, err := svc.Register(context.Background(), RegisterInput{
		Email: "  alice@example.com ", Name: " Alice ", Password: "correct-horse",
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if u.Email != "alice@example.com" || u.Name != "Alice" {
		t.Errorf("input not trimmed: %+v", u)
	}
	if u.Role != identity.RoleCustomer {
		t.Errorf("role = %s, want CUSTOMER (registration must never create admins)", u.Role)
	}
	if u.PasswordHash == "correct-horse" || !strings.HasPrefix(u.PasswordHash, "$2") {
		t.Errorf("password was not bcrypt-hashed: %q", u.PasswordHash)
	}
}

func TestRegister_Validation(t *testing.T) {
	svc := newTestService(t, newFakeStore())

	tests := []struct {
		name      string
		in        RegisterInput
		wantField string
	}{
		{"empty email", RegisterInput{Email: "", Name: "A", Password: "12345678"}, "email"},
		{"invalid email", RegisterInput{Email: "not-an-email", Name: "A", Password: "12345678"}, "email"},
		{"display-name email", RegisterInput{Email: "Bob <bob@x.com>", Name: "A", Password: "12345678"}, "email"},
		{"blank name", RegisterInput{Email: "a@x.com", Name: "   ", Password: "12345678"}, "name"},
		{"long name", RegisterInput{Email: "a@x.com", Name: strings.Repeat("n", 101), Password: "12345678"}, "name"},
		{"short password", RegisterInput{Email: "a@x.com", Name: "A", Password: "1234567"}, "password"},
		{"long password", RegisterInput{Email: "a@x.com", Name: "A", Password: strings.Repeat("p", 73)}, "password"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.Register(context.Background(), tt.in)

			var verr validate.Errors
			if !errors.As(err, &verr) {
				t.Fatalf("error = %v, want validate.Errors", err)
			}
			if _, ok := verr[tt.wantField]; !ok {
				t.Errorf("errors = %v, want a problem for field %q", verr, tt.wantField)
			}
		})
	}
}

func TestRegister_DuplicateEmailIsCaseInsensitive(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	ctx := context.Background()

	if _, err := svc.Register(ctx, RegisterInput{Email: "bob@example.com", Name: "Bob", Password: "password1"}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Register(ctx, RegisterInput{Email: "BOB@Example.com", Name: "Bob2", Password: "password2"})
	if !errors.Is(err, users.ErrEmailTaken) {
		t.Errorf("error = %v, want ErrEmailTaken", err)
	}
}

func TestLogin_Success(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	ctx := context.Background()
	registered, _ := svc.Register(ctx, RegisterInput{Email: "carol@example.com", Name: "Carol", Password: "s3cret-pass"})

	res, err := svc.Login(ctx, LoginInput{Email: "CAROL@example.com", Password: "s3cret-pass"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	p, err := svc.tokens.Verify(res.AccessToken)
	if err != nil {
		t.Fatalf("issued token does not verify: %v", err)
	}
	if p.UserID != registered.ID || p.Role != identity.RoleCustomer {
		t.Errorf("token principal = %+v, want user %d CUSTOMER", p, registered.ID)
	}
}

func TestLogin_WrongPasswordAndUnknownEmailLookIdentical(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	ctx := context.Background()
	_, _ = svc.Register(ctx, RegisterInput{Email: "dave@example.com", Name: "Dave", Password: "right-password"})

	_, errWrongPw := svc.Login(ctx, LoginInput{Email: "dave@example.com", Password: "wrong-password"})
	_, errNoUser := svc.Login(ctx, LoginInput{Email: "nobody@example.com", Password: "whatever1"})

	if !errors.Is(errWrongPw, ErrInvalidCredentials) || !errors.Is(errNoUser, ErrInvalidCredentials) {
		t.Errorf("errors = %v / %v, want ErrInvalidCredentials for both", errWrongPw, errNoUser)
	}
}

func TestLogin_MissingFields(t *testing.T) {
	svc := newTestService(t, newFakeStore())

	_, err := svc.Login(context.Background(), LoginInput{})
	var verr validate.Errors
	if !errors.As(err, &verr) || verr["email"] == "" || verr["password"] == "" {
		t.Errorf("error = %v, want validation errors for email and password", err)
	}
}

func TestService_DatabaseFailureIsPassedThrough(t *testing.T) {
	store := newFakeStore()
	store.err = errors.New("connection refused")
	svc := newTestService(t, store)
	ctx := context.Background()

	_, err := svc.Register(ctx, RegisterInput{Email: "e@x.com", Name: "E", Password: "password1"})
	if err == nil || errors.Is(err, users.ErrEmailTaken) {
		t.Errorf("Register() error = %v, want the underlying DB error", err)
	}

	// A DB outage during login must NOT look like "wrong password": the
	// handler has to answer 500, not 401.
	_, err = svc.Login(ctx, LoginInput{Email: "e@x.com", Password: "password1"})
	if err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Login() error = %v, want the underlying DB error", err)
	}
}
