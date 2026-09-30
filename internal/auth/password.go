// Package auth handles registration, login, JWT access tokens and the
// middleware that protects endpoints.
package auth

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// PasswordHasher hashes and checks passwords with bcrypt.
//
// Why bcrypt and not SHA-256? SHA-256 is designed to be FAST: an attacker
// with a stolen database can try billions of guesses per second. bcrypt is
// deliberately SLOW (the cost factor) and salted (every hash is unique even
// for equal passwords), which makes brute-forcing leaked hashes impractical.
type PasswordHasher struct {
	cost int
}

// NewPasswordHasher returns a hasher with the given bcrypt cost.
// Production uses bcrypt.DefaultCost (10, ~50-100ms per hash); tests use
// bcrypt.MinCost so they run quickly.
func NewPasswordHasher(cost int) PasswordHasher {
	return PasswordHasher{cost: cost}
}

// Hash returns the bcrypt hash of password. The salt and cost are stored
// inside the resulting string, e.g. "$2a$10$<salt><hash>".
func (h PasswordHasher) Hash(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), h.cost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// Matches reports whether password matches the stored hash.
// bcrypt compares in constant time, so the check leaks no timing hints.
func (h PasswordHasher) Matches(hash, password string) (bool, error) {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("compare password: %w", err)
	}
	return true, nil
}
