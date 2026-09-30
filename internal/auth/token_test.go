package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"inventory-order-engine/internal/identity"
)

const testSecret = "test-secret-that-is-at-least-32-bytes-long"

func TestToken_IssueAndVerify(t *testing.T) {
	m := NewTokenManager(testSecret, time.Hour)

	token, expiresAt, err := m.Issue(42, identity.RoleAdmin)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if time.Until(expiresAt) <= 59*time.Minute {
		t.Errorf("expiresAt = %v, want ~1h from now", expiresAt)
	}

	p, err := m.Verify(token)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if p.UserID != 42 || p.Role != identity.RoleAdmin {
		t.Errorf("principal = %+v, want {42 ADMIN}", p)
	}
}

func TestToken_Expired(t *testing.T) {
	m := NewTokenManager(testSecret, time.Hour)
	token, _, err := m.Issue(1, identity.RoleCustomer)
	if err != nil {
		t.Fatal(err)
	}

	// Move the manager's clock 2 hours into the future.
	m.now = func() time.Time { return time.Now().Add(2 * time.Hour) }

	if _, err := m.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Verify(expired) error = %v, want ErrInvalidToken", err)
	}
}

func TestToken_WrongSecret(t *testing.T) {
	token, _, err := NewTokenManager(testSecret, time.Hour).Issue(1, identity.RoleCustomer)
	if err != nil {
		t.Fatal(err)
	}

	other := NewTokenManager("a-completely-different-secret-of-32-bytes!", time.Hour)
	if _, err := other.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Verify(wrong secret) error = %v, want ErrInvalidToken", err)
	}
}

func TestToken_TamperedPayload(t *testing.T) {
	m := NewTokenManager(testSecret, time.Hour)
	customerToken, _, _ := m.Issue(1, identity.RoleCustomer)
	adminToken, _, _ := m.Issue(1, identity.RoleAdmin)

	// Take the header+signature of the customer token but the payload of
	// the admin token: the signature no longer matches the payload.
	c := strings.Split(customerToken, ".")
	a := strings.Split(adminToken, ".")
	forged := c[0] + "." + a[1] + "." + c[2]

	if _, err := m.Verify(forged); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Verify(tampered) error = %v, want ErrInvalidToken", err)
	}
}

func TestToken_RejectsOtherAlgorithms(t *testing.T) {
	m := NewTokenManager(testSecret, time.Hour)
	c := claims{
		Role: identity.RoleAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "1",
			Issuer:    tokenIssuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}

	// "alg": "none" means "no signature" - the classic JWT attack.
	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, c).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(none); !errors.Is(err, ErrInvalidToken) {
		t.Error("token with alg=none was accepted")
	}

	// HS512 with the right secret is still not the algorithm we issue.
	hs512, err := jwt.NewWithClaims(jwt.SigningMethodHS512, c).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(hs512); !errors.Is(err, ErrInvalidToken) {
		t.Error("token with alg=HS512 was accepted")
	}
}

func TestToken_RejectsBadClaims(t *testing.T) {
	m := NewTokenManager(testSecret, time.Hour)
	future := jwt.NewNumericDate(time.Now().Add(time.Hour))

	tests := []struct {
		name   string
		claims claims
	}{
		{"unknown role", claims{Role: "SUPERADMIN", RegisteredClaims: jwt.RegisteredClaims{Subject: "1", Issuer: tokenIssuer, ExpiresAt: future}}},
		{"non-numeric subject", claims{Role: identity.RoleCustomer, RegisteredClaims: jwt.RegisteredClaims{Subject: "abc", Issuer: tokenIssuer, ExpiresAt: future}}},
		{"wrong issuer", claims{Role: identity.RoleCustomer, RegisteredClaims: jwt.RegisteredClaims{Subject: "1", Issuer: "evil", ExpiresAt: future}}},
		{"no expiry", claims{Role: identity.RoleCustomer, RegisteredClaims: jwt.RegisteredClaims{Subject: "1", Issuer: tokenIssuer}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, tt.claims).SignedString([]byte(testSecret))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Verify(token); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("Verify() error = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestToken_Garbage(t *testing.T) {
	m := NewTokenManager(testSecret, time.Hour)
	for _, s := range []string{"", "abc", "a.b.c", "Bearer xyz"} {
		if _, err := m.Verify(s); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Verify(%q) error = %v, want ErrInvalidToken", s, err)
		}
	}
}
