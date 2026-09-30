package httpx

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestPathID(t *testing.T) {
	tests := map[string]bool{"1": true, "42": true, "0": false, "-3": false, "abc": false, "1.5": false, "": false}

	for raw, wantOK := range tests {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", raw)
		req := httptest.NewRequest("GET", "/", nil)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		if _, ok := PathID(req, "id"); ok != wantOK {
			t.Errorf("PathID(%q) ok = %v, want %v", raw, ok, wantOK)
		}
	}
}

func TestQueryInt(t *testing.T) {
	tests := []struct {
		url    string
		want   int
		wantOK bool
	}{
		{"/", 0, true},
		{"/?limit=25", 25, true},
		{"/?limit=-1", -1, true}, // parsed; range checks belong to the service
		{"/?limit=ten", 0, false},
	}
	for _, tt := range tests {
		n, ok := QueryInt(httptest.NewRequest("GET", tt.url, nil), "limit")
		if n != tt.want || ok != tt.wantOK {
			t.Errorf("QueryInt(%s) = %d, %v; want %d, %v", tt.url, n, ok, tt.want, tt.wantOK)
		}
	}
}
