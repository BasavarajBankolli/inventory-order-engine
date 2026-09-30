package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"inventory-order-engine/internal/httpx"
	"inventory-order-engine/internal/logging"
	"inventory-order-engine/internal/requestid"
)

func TestRequestID_GeneratesWhenMissing(t *testing.T) {
	var idInHandler string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idInHandler = requestid.FromContext(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	header := rec.Header().Get(requestid.Header)
	if header == "" {
		t.Fatal("response has no X-Request-ID header")
	}
	if idInHandler != header {
		t.Errorf("context id %q != header id %q", idInHandler, header)
	}
}

func TestRequestID_ReusesValidIncomingID(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(requestid.Header, "client-id-42")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get(requestid.Header); got != "client-id-42" {
		t.Errorf("X-Request-ID = %q, want client-id-42", got)
	}
}

func TestRequestID_ReplacesInvalidIncomingID(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(requestid.Header, "bad id with spaces")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := rec.Header().Get(requestid.Header)
	if got == "bad id with spaces" || !requestid.IsValid(got) {
		t.Errorf("X-Request-ID = %q, want a freshly generated id", got)
	}
}

func TestLogger_LogsStatusAndRequestID(t *testing.T) {
	var logs bytes.Buffer
	logger := logging.New(&logs, slog.LevelInfo)

	h := RequestID(Logger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})))

	req := httptest.NewRequest(http.MethodGet, "/some/path?token=secret", nil)
	req.Header.Set(requestid.Header, "req-1")
	h.ServeHTTP(httptest.NewRecorder(), req)

	var line map[string]any
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, logs.String())
	}
	if line["status"] != float64(http.StatusTeapot) {
		t.Errorf("status = %v, want 418", line["status"])
	}
	if line["request_id"] != "req-1" {
		t.Errorf("request_id = %v, want req-1", line["request_id"])
	}
	if line["path"] != "/some/path" {
		t.Errorf("path = %v, want /some/path", line["path"])
	}
	if strings.Contains(logs.String(), "secret") {
		t.Error("query string (which may hold secrets) must not be logged")
	}
}

func TestRecoverer_ReturnsJSON500(t *testing.T) {
	logger := logging.New(io.Discard, slog.LevelInfo)
	h := RequestID(Recoverer(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(requestid.Header, "req-panic")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body.Error.Code != httpx.CodeInternal {
		t.Errorf("code = %q, want %q", body.Error.Code, httpx.CodeInternal)
	}
	if body.Error.RequestID != "req-panic" {
		t.Errorf("request_id = %q, want req-panic", body.Error.RequestID)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("panic details must not be sent to the client")
	}
}
