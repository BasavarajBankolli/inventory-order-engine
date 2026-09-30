package httpx

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// PathID reads a positive integer URL parameter such as {id} in
// /products/{id}. ok is false for "abc", "0", "-5", "1.5", ...
func PathID(r *http.Request, name string) (id int64, ok bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// QueryInt reads an optional integer query parameter (?limit=20).
// A missing parameter returns 0 and ok=true; a present but non-numeric one
// returns ok=false so the handler can answer 400 instead of guessing.
func QueryInt(r *http.Request, name string) (n int, ok bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return n, true
}
