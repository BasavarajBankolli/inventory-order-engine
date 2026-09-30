package money

import "testing"

func TestString(t *testing.T) {
	tests := map[Money]string{
		New(129900, "INR"): "1299.00 INR",
		New(1999, "USD"):   "19.99 USD",
		New(5, "EUR"):      "0.05 EUR",
		New(0, "INR"):      "0.00 INR",
		New(-250, "INR"):   "-2.50 INR",
	}
	for m, want := range tests {
		if got := m.String(); got != want {
			t.Errorf("%+v.String() = %q, want %q", m, got, want)
		}
	}
}
