// Package money represents amounts as integer minor units with an ISO 4217 currency.
// Floats never touch money: 1 ETB = 100 santim, so ETB 185.00 is Money{18500, ETB}.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type Currency string

const (
	ETB Currency = "ETB"
	USD Currency = "USD"
	EUR Currency = "EUR"
	GBP Currency = "GBP"
	KES Currency = "KES"
	AED Currency = "AED"
	SAR Currency = "SAR"
)

// Valid reports whether c is a currency the platform supports (matches platform.currency in the schema).
func (c Currency) Valid() bool {
	switch c {
	case ETB, USD, EUR, GBP, KES, AED, SAR:
		return true
	}
	return false
}

type Money struct {
	AmountMinor int64    `json:"amount_minor"`
	Currency    Currency `json:"currency"`
}

var ErrCurrencyMismatch = errors.New("money: currency mismatch")

func New(amountMinor int64, c Currency) Money { return Money{AmountMinor: amountMinor, Currency: c} }

func (m Money) Add(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, ErrCurrencyMismatch
	}
	return Money{AmountMinor: m.AmountMinor + o.AmountMinor, Currency: m.Currency}, nil
}

func (m Money) Sub(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, ErrCurrencyMismatch
	}
	return Money{AmountMinor: m.AmountMinor - o.AmountMinor, Currency: m.Currency}, nil
}

// String formats for logs only (e.g. "ETB 185.00"). Clients format amounts themselves.
func (m Money) String() string {
	sign := ""
	a := m.AmountMinor
	if a < 0 {
		sign, a = "-", -a
	}
	return fmt.Sprintf("%s %s%d.%02d", m.Currency, sign, a/100, a%100)
}

// Display formats an amount for people, as the app does: "ETB 1,250.00".
func (m Money) Display() string {
	sign := ""
	a := m.AmountMinor
	if a < 0 {
		sign, a = "-", -a
	}
	whole := strconv.FormatInt(a/100, 10)
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return fmt.Sprintf("%s %s%s.%02d", m.Currency, sign, b.String(), a%100)
}

// MaxAmountMinor caps a single amount (10 billion ETB) so basis-point maths can't overflow int64.
const MaxAmountMinor int64 = 1_000_000_000_000

// BasisPoints returns amount × bps / 10,000, rounded half up. 50 bps = 0.5%.
func BasisPoints(amountMinor, bps int64) int64 {
	if amountMinor <= 0 || bps <= 0 {
		return 0
	}
	return (amountMinor*bps + 5_000) / 10_000
}

// Decimal formats an amount the way partner APIs send it: "100.00". All supported currencies have
// two decimal places.
func (m Money) Decimal() string {
	sign := ""
	a := m.AmountMinor
	if a < 0 {
		sign, a = "-", -a
	}
	return fmt.Sprintf("%s%d.%02d", sign, a/100, a%100)
}

var ErrBadDecimal = errors.New("money: not a decimal amount with at most two places")

// ParseDecimal reads "100", "100.5" or "100.50" into minor units without floating point.
// Signs, exponents, separators and more than two decimal places are rejected.
func ParseDecimal(s string, c Currency) (Money, error) {
	whole, frac, hasDot := s, "", false
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			whole, frac, hasDot = s[:i], s[i+1:], true
			break
		}
	}
	if whole == "" || len(whole) > 13 || len(frac) > 2 || (hasDot && frac == "") {
		return Money{}, ErrBadDecimal
	}
	var minor int64
	for _, part := range []string{whole, frac + "00"[:2-len(frac)]} {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return Money{}, ErrBadDecimal
			}
			minor = minor*10 + int64(part[i]-'0')
		}
	}
	return Money{AmountMinor: minor, Currency: c}, nil
}
