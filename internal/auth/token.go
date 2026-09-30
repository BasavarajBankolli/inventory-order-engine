package auth

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"inventory-order-engine/internal/identity"
)

// tokenIssuer is written into every token ("iss" claim) and required when
// verifying, so tokens minted by some other system are rejected.
const tokenIssuer = "inventory-order-engine"

// ErrInvalidToken is returned for any token that must not be trusted:
// bad signature, expired, wrong algorithm, malformed, unknown role, ...
// We deliberately do not tell the client which of these it was.
var ErrInvalidToken = errors.New("invalid or expired token")

// TokenManager creates and verifies JWT access tokens.
//
// A JWT has three base64 parts: header.payload.signature. The payload
// (claims) is readable by anyone; the signature (HMAC-SHA256 with our
// secret) proves the server created it and nobody changed it. That is why
// the server can trust a token without looking anything up: stateless auth.
type TokenManager struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time // replaceable in tests to simulate expiry
}

// NewTokenManager creates a TokenManager. secret must stay private: anyone
// who knows it can forge tokens for any user, including admins.
func NewTokenManager(secret string, ttl time.Duration) *TokenManager {
	return &TokenManager{secret: []byte(secret), ttl: ttl, now: time.Now}
}

// claims is the JWT payload. RegisteredClaims supplies the standard fields
// (sub = subject/user id, exp = expiry, iat = issued-at, iss = issuer).
type claims struct {
	Role identity.Role `json:"role"`
	jwt.RegisteredClaims
}

// Issue creates a signed token for the user.
func (m *TokenManager) Issue(userID int64, role identity.Role) (token string, expiresAt time.Time, err error) {
	now := m.now()
	expiresAt = now.Add(m.ttl)

	c := claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(userID, 10),
			Issuer:    tokenIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}

	token, err = jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return token, expiresAt, nil
}

// Verify checks the token and returns the caller's identity.
func (m *TokenManager) Verify(tokenString string) (identity.Principal, error) {
	var c claims
	_, err := jwt.ParseWithClaims(tokenString, &c,
		func(*jwt.Token) (any, error) { return m.secret, nil },
		// Only accept HS256. Without this, a classic attack is to send a
		// token with "alg":"none" (no signature) or a different algorithm.
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(m.now),
	)
	if err != nil {
		return identity.Principal{}, ErrInvalidToken
	}

	userID, err := strconv.ParseInt(c.Subject, 10, 64)
	if err != nil || userID <= 0 || !c.Role.Valid() {
		return identity.Principal{}, ErrInvalidToken
	}

	return identity.Principal{UserID: userID, Role: c.Role}, nil
}
