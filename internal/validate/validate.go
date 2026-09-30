// Package validate collects input validation problems.
//
// Services validate their input and return a validate.Errors value. The
// HTTP layer recognises it (errors.As) and turns it into a 400 response
// that lists every invalid field at once, so the client can fix them all
// in one go instead of one error per request.
package validate

import (
	"sort"
	"strings"
)

// Errors maps a field name to a human-readable problem, e.g.
// {"email": "must be a valid email address"}.
type Errors map[string]string

// Add records a problem for field. The first problem per field wins.
func (e Errors) Add(field, message string) {
	if _, exists := e[field]; !exists {
		e[field] = message
	}
}

// Check adds the message for field only when ok is false.
// It keeps validation code short:
//
//	v.Check(len(name) > 0, "name", "is required")
func (e Errors) Check(ok bool, field, message string) {
	if !ok {
		e.Add(field, message)
	}
}

// Err returns e as an error, or nil if there are no problems.
// Always return v.Err() (not v) so an empty map becomes a real nil error.
func (e Errors) Err() error {
	if len(e) == 0 {
		return nil
	}
	return e
}

// Error implements the error interface with a stable, sorted message.
func (e Errors) Error() string {
	fields := make([]string, 0, len(e))
	for f := range e {
		fields = append(fields, f)
	}
	sort.Strings(fields)

	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, f+": "+e[f])
	}
	return "validation failed: " + strings.Join(parts, "; ")
}
