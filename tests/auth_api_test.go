package tests

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

type userResponse struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

type loginResponse struct {
	AccessToken string       `json:"access_token"`
	TokenType   string       `json:"token_type"`
	ExpiresIn   int64        `json:"expires_in"`
	User        userResponse `json:"user"`
}

// The happy path a real client follows: register -> login -> /users/me.
func TestAuthFlow_RegisterLoginMe(t *testing.T) {
	api := newTestAPI(t)

	var registered userResponse
	resp := api.do("POST", "/api/v1/auth/register", map[string]string{
		"email": "alice@example.com", "name": "Alice", "password": "super-secret-1",
	}, "", &registered)
	expectStatus(t, resp, http.StatusCreated)
	if registered.ID == 0 || registered.Role != "CUSTOMER" {
		t.Fatalf("registered = %+v, want an id and role CUSTOMER", registered)
	}

	var login loginResponse
	resp = api.do("POST", "/api/v1/auth/login", map[string]string{
		"email": "ALICE@example.com", "password": "super-secret-1",
	}, "", &login)
	expectStatus(t, resp, http.StatusOK)
	if login.AccessToken == "" || login.TokenType != "Bearer" || login.ExpiresIn <= 0 {
		t.Fatalf("login = %+v, want a bearer token", login)
	}

	var me userResponse
	resp = api.do("GET", "/api/v1/users/me", nil, login.AccessToken, &me)
	expectStatus(t, resp, http.StatusOK)
	if me != registered {
		t.Errorf("me = %+v, want %+v", me, registered)
	}
}

func TestRegister_ResponseNeverContainsPassword(t *testing.T) {
	api := newTestAPI(t)

	var raw map[string]any
	resp := api.do("POST", "/api/v1/auth/register", map[string]string{
		"email": "bob@example.com", "name": "Bob", "password": "bobs-password",
	}, "", &raw)
	expectStatus(t, resp, http.StatusCreated)

	for key := range raw {
		if strings.Contains(strings.ToLower(key), "password") {
			t.Errorf("response exposes field %q", key)
		}
	}
}

func TestRegister_Errors(t *testing.T) {
	api := newTestAPI(t)
	api.do("POST", "/api/v1/auth/register", map[string]string{
		"email": "taken@example.com", "name": "T", "password": "password-1",
	}, "", nil)

	tests := []struct {
		name       string
		body       any
		wantStatus int
		wantCode   string
	}{
		{"duplicate email (different case)", map[string]string{"email": "TAKEN@example.com", "name": "T2", "password": "password-2"}, 409, "EMAIL_ALREADY_EXISTS"},
		{"invalid fields", map[string]string{"email": "nope", "name": "", "password": "short"}, 400, "VALIDATION_ERROR"},
		{"unknown field", map[string]string{"email": "x@example.com", "name": "X", "password": "password-1", "role": "ADMIN"}, 400, "VALIDATION_ERROR"},
		{"empty body", nil, 400, "VALIDATION_ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body errorResponse
			resp := api.do("POST", "/api/v1/auth/register", tt.body, "", &body)
			expectStatus(t, resp, tt.wantStatus)
			if body.Error.Code != tt.wantCode || body.Error.RequestID == "" {
				t.Errorf("error = %+v, want code %s with a request_id", body.Error, tt.wantCode)
			}
		})
	}

	// All field problems are reported together.
	var body errorResponse
	api.do("POST", "/api/v1/auth/register", map[string]string{"email": "nope", "name": "", "password": "short"}, "", &body)
	for _, f := range []string{"email", "name", "password"} {
		if body.Error.Fields[f] == "" {
			t.Errorf("fields = %v, want a problem for %q", body.Error.Fields, f)
		}
	}
}

func TestLogin_Errors(t *testing.T) {
	api := newTestAPI(t)
	api.do("POST", "/api/v1/auth/register", map[string]string{
		"email": "carol@example.com", "name": "Carol", "password": "carols-password",
	}, "", nil)

	var wrongPw, noUser errorResponse
	resp := api.do("POST", "/api/v1/auth/login", map[string]string{"email": "carol@example.com", "password": "WRONG-password"}, "", &wrongPw)
	expectStatus(t, resp, http.StatusUnauthorized)
	resp = api.do("POST", "/api/v1/auth/login", map[string]string{"email": "nobody@example.com", "password": "whatever-1"}, "", &noUser)
	expectStatus(t, resp, http.StatusUnauthorized)

	// Identical code and message: no hint about which emails exist.
	if wrongPw.Error.Code != "INVALID_CREDENTIALS" || wrongPw.Error.Code != noUser.Error.Code || wrongPw.Error.Message != noUser.Error.Message {
		t.Errorf("responses differ: %+v vs %+v", wrongPw.Error, noUser.Error)
	}
}

// A token stays cryptographically valid after its user is deleted, until it
// expires. /users/me must notice the user is gone (401), not crash or 500.
func TestMe_DeletedUser(t *testing.T) {
	api := newTestAPI(t)
	token := api.loginAs("gone@example.com", false)
	if _, err := api.pool.Exec(context.Background(), `DELETE FROM users WHERE email = 'gone@example.com'`); err != nil {
		t.Fatal(err)
	}

	var body errorResponse
	expectStatus(t, api.do("GET", "/api/v1/users/me", nil, token, &body), http.StatusUnauthorized)
	if body.Error.Message != "user no longer exists" {
		t.Errorf("message = %q", body.Error.Message)
	}
}

func TestMe_RequiresValidToken(t *testing.T) {
	api := newTestAPI(t)

	for _, token := range []string{"", "garbage", "eyJhbGciOiJub25lIn0.eyJzdWIiOiIxIn0."} {
		var body errorResponse
		resp := api.do("GET", "/api/v1/users/me", nil, token, &body)
		expectStatus(t, resp, http.StatusUnauthorized)
		if body.Error.Code != "UNAUTHENTICATED" {
			t.Errorf("token %q: code = %q, want UNAUTHENTICATED", token, body.Error.Code)
		}
	}
}
