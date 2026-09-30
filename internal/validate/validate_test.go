package validate

import (
	"errors"
	"testing"
)

func TestErrors(t *testing.T) {
	v := Errors{}
	if v.Err() != nil {
		t.Fatal("empty Errors must produce a nil error")
	}

	v.Check(true, "ok_field", "never recorded")
	v.Check(false, "email", "is required")
	v.Check(false, "email", "second problem is ignored")
	v.Add("age", "must be positive")

	err := v.Err()
	if err == nil {
		t.Fatal("Err() = nil, want an error")
	}
	if got := err.Error(); got != "validation failed: age: must be positive; email: is required" {
		t.Errorf("Error() = %q", got)
	}

	var verr Errors
	if !errors.As(err, &verr) || len(verr) != 2 {
		t.Errorf("errors.As failed or wrong fields: %v", verr)
	}
}
