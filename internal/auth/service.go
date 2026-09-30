package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/users"
	"inventory-order-engine/internal/validate"
)

// ErrInvalidCredentials is returned by Login for BOTH "no such email" and
// "wrong password". Returning different errors would let an attacker find
// out which emails have accounts (user enumeration).
var ErrInvalidCredentials = errors.New("invalid email or password")

// Password length limits. bcrypt only uses the first 72 bytes of input, so
// longer passwords are rejected instead of being silently truncated.
const (
	minPasswordLen = 8
	maxPasswordLen = 72
	maxNameLen     = 100
	maxEmailLen    = 254
)

// UserStore is what the auth service needs from user storage.
//
// It is an interface (defined here, where it is USED) so unit tests can
// pass an in-memory fake instead of a real database. *users.Repository
// satisfies it automatically: Go interfaces are implemented implicitly.
type UserStore interface {
	Create(ctx context.Context, nu users.NewUser) (users.User, error)
	GetByEmail(ctx context.Context, email string) (users.User, error)
}

// Service contains the registration and login business logic.
type Service struct {
	users  UserStore
	tokens *TokenManager
	hasher PasswordHasher

	// dummyHash is compared against when the email does not exist, so a
	// login for an unknown email takes as long as one with a wrong password.
	// Otherwise response time alone would reveal which emails are registered.
	dummyHash string
}

// NewService creates the auth Service.
func NewService(store UserStore, tokens *TokenManager, hasher PasswordHasher) (*Service, error) {
	dummy, err := hasher.Hash("dummy-password-used-only-for-timing")
	if err != nil {
		return nil, err
	}
	return &Service{users: store, tokens: tokens, hasher: hasher, dummyHash: dummy}, nil
}

// RegisterInput is the data a new user submits.
type RegisterInput struct {
	Email    string
	Name     string
	Password string
}

// Register validates the input, hashes the password and creates a CUSTOMER.
//
// Public registration can never create an ADMIN. Admins are promoted by an
// operator directly in the database (see README).
func (s *Service) Register(ctx context.Context, in RegisterInput) (users.User, error) {
	in.Email = strings.TrimSpace(in.Email)
	in.Name = strings.TrimSpace(in.Name)

	v := validate.Errors{}
	validateEmail(v, in.Email)
	v.Check(in.Name != "", "name", "is required")
	v.Check(utf8.RuneCountInString(in.Name) <= maxNameLen, "name", fmt.Sprintf("must be at most %d characters", maxNameLen))
	v.Check(len(in.Password) >= minPasswordLen, "password", fmt.Sprintf("must be at least %d characters", minPasswordLen))
	v.Check(len(in.Password) <= maxPasswordLen, "password", fmt.Sprintf("must be at most %d bytes", maxPasswordLen))
	if err := v.Err(); err != nil {
		return users.User{}, err
	}

	hash, err := s.hasher.Hash(in.Password)
	if err != nil {
		return users.User{}, err
	}

	u, err := s.users.Create(ctx, users.NewUser{
		Email:        in.Email,
		Name:         in.Name,
		PasswordHash: hash,
		Role:         identity.RoleCustomer,
	})
	if err != nil {
		return users.User{}, err // may be users.ErrEmailTaken
	}

	slog.InfoContext(ctx, "user registered", "user_id", u.ID)
	return u, nil
}

// LoginInput is the data a user submits to log in.
type LoginInput struct {
	Email    string
	Password string
}

// LoginResult is returned on successful login.
type LoginResult struct {
	AccessToken string
	ExpiresAt   time.Time
	User        users.User
}

// Login checks the credentials and issues an access token.
func (s *Service) Login(ctx context.Context, in LoginInput) (LoginResult, error) {
	email := strings.TrimSpace(in.Email)

	v := validate.Errors{}
	v.Check(email != "", "email", "is required")
	v.Check(in.Password != "", "password", "is required")
	if err := v.Err(); err != nil {
		return LoginResult{}, err
	}

	u, err := s.users.GetByEmail(ctx, email)
	if errors.Is(err, users.ErrNotFound) {
		// Burn the same CPU time as a real check (see dummyHash), then fail.
		_, _ = s.hasher.Matches(s.dummyHash, in.Password)
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, err
	}

	ok, err := s.hasher.Matches(u.PasswordHash, in.Password)
	if err != nil {
		return LoginResult{}, err
	}
	if !ok {
		// Log the user id, never the password that was tried.
		slog.WarnContext(ctx, "login failed: wrong password", "user_id", u.ID)
		return LoginResult{}, ErrInvalidCredentials
	}

	token, expiresAt, err := s.tokens.Issue(u.ID, u.Role)
	if err != nil {
		return LoginResult{}, err
	}

	slog.InfoContext(ctx, "user logged in", "user_id", u.ID)
	return LoginResult{AccessToken: token, ExpiresAt: expiresAt, User: u}, nil
}

func validateEmail(v validate.Errors, email string) {
	if email == "" {
		v.Add("email", "is required")
		return
	}
	if len(email) > maxEmailLen {
		v.Add("email", fmt.Sprintf("must be at most %d characters", maxEmailLen))
		return
	}
	// mail.ParseAddress also accepts `Bob <bob@x.com>`; requiring the parsed
	// address to equal the input allows only the plain `bob@x.com` form.
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		v.Add("email", "must be a valid email address")
	}
}
