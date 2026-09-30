package auth

import (
	"errors"
	"net/http"
	"time"

	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/users"
	"inventory-order-engine/internal/validate"
)

// Handler exposes the auth Service over HTTP. It only translates between
// JSON and Go values; all rules live in the Service.
type Handler struct {
	svc *Service
}

// NewHandler creates an auth Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

type registerRequest struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// Register handles POST /api/v1/auth/register.
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	u, err := h.svc.Register(r.Context(), RegisterInput{
		Email:    req.Email,
		Name:     req.Name,
		Password: req.Password,
	})
	if err != nil {
		writeServiceError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusCreated, users.ToResponse(u))
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	AccessToken string         `json:"access_token"`
	TokenType   string         `json:"token_type"`
	ExpiresIn   int64          `json:"expires_in"` // seconds, as in OAuth2
	ExpiresAt   time.Time      `json:"expires_at"`
	User        users.Response `json:"user"`
}

// Login handles POST /api/v1/auth/login.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	res, err := h.svc.Login(r.Context(), LoginInput{Email: req.Email, Password: req.Password})
	if err != nil {
		writeServiceError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, loginResponse{
		AccessToken: res.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int64(time.Until(res.ExpiresAt).Seconds()),
		ExpiresAt:   res.ExpiresAt,
		User:        users.ToResponse(res.User),
	})
}

// writeServiceError maps service errors to HTTP responses. Anything not
// recognised is an unexpected failure: logged in full, returned as a 500.
func writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var verr validate.Errors
	switch {
	case errors.As(err, &verr):
		httpx.WriteValidationError(w, r, verr)
	case errors.Is(err, users.ErrEmailTaken):
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeEmailTaken, "an account with this email already exists")
	case errors.Is(err, ErrInvalidCredentials):
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeInvalidCredentials, "invalid email or password")
	default:
		httpx.WriteInternalError(w, r, err)
	}
}
