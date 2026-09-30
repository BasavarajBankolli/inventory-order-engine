package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxBodyBytes caps request bodies at 1 MB. Without a limit, a client could
// send a multi-gigabyte body and exhaust the server's memory.
const maxBodyBytes = 1 << 20

// DecodeJSON reads a single JSON object from the request body into dst.
//
// It is strict on purpose:
//   - unknown fields are rejected ("emial" typo -> 400, not silently ignored);
//   - trailing data after the object is rejected;
//   - the body size is limited.
//
// The returned error message is safe to show to the client.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		var maxErr *http.MaxBytesError

		switch {
		case errors.Is(err, io.EOF):
			return errors.New("request body must not be empty")
		case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF):
			return errors.New("request body contains malformed JSON")
		case errors.As(err, &typeErr):
			return fmt.Errorf("field %q has the wrong type", typeErr.Field)
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			// encoding/json has no typed error for this case, so we match the text.
			field := strings.TrimPrefix(err.Error(), "json: unknown field ")
			return fmt.Errorf("request body contains unknown field %s", field)
		case errors.As(err, &maxErr):
			return fmt.Errorf("request body must not be larger than %d bytes", maxBodyBytes)
		default:
			return errors.New("request body is not valid JSON")
		}
	}

	// A second Decode must hit EOF, otherwise there was more than one value,
	// e.g. `{"a":1}{"b":2}`.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}
