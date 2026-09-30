// Package users owns user accounts: the User type, how users are stored in
// PostgreSQL and the /users/me endpoint.
//
// Registration and login (passwords, tokens) live in the auth package,
// which uses this package's Repository to read and create users.
package users

import (
	"errors"
	"time"

	"inventory-order-engine/internal/identity"
)

// Errors returned by the repository. Callers compare with errors.Is.
var (
	ErrNotFound   = errors.New("user not found")
	ErrEmailTaken = errors.New("email is already registered")
)

// User is a user account as stored in the database.
//
// It deliberately has NO json tags: PasswordHash must never be sent to a
// client by accident. HTTP handlers convert it to a Response instead.
type User struct {
	ID           int64
	Email        string
	Name         string
	PasswordHash string
	Role         identity.Role
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// NewUser is the data needed to create a user.
type NewUser struct {
	Email        string
	Name         string
	PasswordHash string
	Role         identity.Role
}

// Response is the public JSON representation of a user.
type Response struct {
	ID        int64         `json:"id"`
	Email     string        `json:"email"`
	Name      string        `json:"name"`
	Role      identity.Role `json:"role"`
	CreatedAt time.Time     `json:"created_at"`
}

// ToResponse converts a User to its public JSON form.
func ToResponse(u User) Response {
	return Response{
		ID:        u.ID,
		Email:     u.Email,
		Name:      u.Name,
		Role:      u.Role,
		CreatedAt: u.CreatedAt,
	}
}
