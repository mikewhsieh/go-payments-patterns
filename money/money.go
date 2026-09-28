// Package money implements a fixed-point decimal type for monetary
// amounts.
//
// All arithmetic happens on scaled int64 values (value * 10^Scale).
// Floating point is never involved, so an amount survives any number of
// round-trips through storage and arithmetic without losing precision.
// This is the single most important property of a payment system: the
// number you settled is the number you stored is the number you report.
package money

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// Scale is the number of decimal places every Money value carries.
// It is defined in exactly one place on purpose: a system where two
// components disagree about scale has a silent-corruption bug waiting.
const Scale = 4

var scaleFactor = pow10(Scale)

func pow10(n int) int64 {
	r := int64(1)
	for i := 0; i < n; i++ {
		r *= 10
	}
	return r
}

// Money is a fixed-point decimal amount, stored as value * 10^Scale in
// a plain int64. The zero value is 0.0000.
type Money struct {
	scaled int64
}

// FromScaled builds a Money from its internal representation
// (scaled == value * 10^Scale). Use it when reading a pre-scaled
// integer column from storage.
func FromScaled(scaled int64) Money { return Money{scaled: scaled} }

// FromInt builds a Money from a whole number of major units,
// e.g. FromInt(100) == 100.0000.
func FromInt(units int64) Money { return Money{scaled: units * scaleFactor} }

// Parse parses a decimal string such as "213.00" or "-0.7881" exactly,
// with no floating-point round-trip. Inputs with more than Scale
// fractional digits are rejected rather than silently truncated —
// silent truncation is exactly the failure mode this package exists to
// prevent.
func Parse(s string) (Money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Money{}, fmt.Errorf("money: empty string")
	}

	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}

	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	if len(fracPart) > Scale {
		return Money{}, fmt.Errorf("money: %q has more than %d fractional digits", s, Scale)
	}
	for len(fracPart) < Scale {
		fracPart += "0"
	}

	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("money: invalid integer part %q: %w", intPart, err)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("money: invalid fractional part %q: %w", fracPart, err)
	}

	scaled := whole*scaleFactor + frac
	if neg {
		scaled = -scaled
	}
	return Money{scaled: scaled}, nil
}

// MustParse is Parse but panics on error. For tests and constant-like
// literals only — never for external input.
func MustParse(s string) Money {
	m, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return m
}

// Scaled returns the internal scaled integer (value * 10^Scale).
// Storage code should persist this — never a float — so a round-trip
// through the database can never lose precision.
func (m Money) Scaled() int64 { return m.scaled }

// Add returns m + other.
func (m Money) Add(other Money) Money { return Money{scaled: m.scaled + other.scaled} }

// Sub returns m - other.
func (m Money) Sub(other Money) Money { return Money{scaled: m.scaled - other.scaled} }

// IsZero reports whether m == 0.
func (m Money) IsZero() bool { return m.scaled == 0 }

// Cmp returns -1, 0, or 1 as m is less than, equal to, or greater than
// other.
func (m Money) Cmp(other Money) int {
	switch {
	case m.scaled < other.scaled:
		return -1
	case m.scaled > other.scaled:
		return 1
	default:
		return 0
	}
}

// MulRate multiplies m by a dimensionless rate (e.g. a 0.37% fee is
// the rate 0.0037), rounding the exact product to Scale decimals using
// round-half-away-from-zero.
//
// The intermediate product is computed in arbitrary precision
// (big.Int), so it can never silently overflow the way a naive
// int64*int64 multiply could.
func (m Money) MulRate(rate Money) Money {
	// Both operands are scaled by 10^Scale, so the raw product is
	// scaled by 10^(2*Scale); divide one factor back out with proper
	// rounding.
	product := new(big.Int).Mul(big.NewInt(m.scaled), big.NewInt(rate.scaled))
	return Money{scaled: divRoundHalfAwayFromZero(product, big.NewInt(scaleFactor))}
}

func divRoundHalfAwayFromZero(num, den *big.Int) int64 {
	neg := (num.Sign() < 0) != (den.Sign() < 0)

	absNum := new(big.Int).Abs(num)
	absDen := new(big.Int).Abs(den)

	quo, rem := new(big.Int).QuoRem(absNum, absDen, new(big.Int))
	// Round half away from zero: if 2*rem >= den, round up.
	if new(big.Int).Lsh(rem, 1).Cmp(absDen) >= 0 {
		quo.Add(quo, big.NewInt(1))
	}

	result := quo.Int64()
	if neg {
		result = -result
	}
	return result
}

// String renders m with exactly Scale fractional digits, e.g. "0.7881"
// or "-213.0000".
func (m Money) String() string {
	scaled := m.scaled
	neg := scaled < 0
	if neg {
		scaled = -scaled
	}
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%0*d", sign, scaled/scaleFactor, Scale, scaled%scaleFactor)
}
