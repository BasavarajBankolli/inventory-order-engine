// Package money represents amounts of money without floating point.
//
// Amount is in MINOR units (paise, cents): Money{129900, "INR"} is Rs 1,299.00.
// Integers are exact; float64 is not (0.1 + 0.2 != 0.3).
package money

import "fmt"

// Money is an exact amount in one currency.
type Money struct {
	Amount   int64  // minor units
	Currency string // ISO 4217 code, e.g. "INR"
}

// New creates a Money value.
func New(amount int64, currency string) Money {
	return Money{Amount: amount, Currency: currency}
}

// String formats the amount for logs and messages, e.g. "1299.00 INR".
//
// It assumes 2 decimal places, which is true for INR, USD, EUR and most
// currencies (JPY has 0, a few have 3). It is for DISPLAY only; never parse
// it back or do arithmetic on it.
func (m Money) String() string {
	sign := ""
	a := m.Amount
	if a < 0 {
		sign, a = "-", -a
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, a/100, a%100, m.Currency)
}
