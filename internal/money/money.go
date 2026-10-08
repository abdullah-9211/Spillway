// Package money holds dollar amounts as integer micro-dollars. Cost is never a float64.
package money

import (
	"fmt"
	"strings"
)

// Micros is an amount in millionths of a US dollar.
type Micros int64

const perDollar = 1_000_000

// ParseUSD parses a decimal dollar string such as "0.15" or "3" with at most six decimal places.
func ParseUSD(s string) (Micros, error) {
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" && frac == "" {
		return 0, fmt.Errorf("invalid dollar amount %q", s)
	}
	if len(frac) > 6 {
		return 0, fmt.Errorf("dollar amount %q has more than 6 decimal places", s)
	}
	frac += strings.Repeat("0", 6-len(frac))
	var w, f int64
	if whole != "" {
		if _, err := fmt.Sscanf(whole, "%d", &w); err != nil || !digits(whole) {
			return 0, fmt.Errorf("invalid dollar amount %q", s)
		}
	}
	if _, err := fmt.Sscanf(frac, "%d", &f); err != nil || !digits(frac) {
		return 0, fmt.Errorf("invalid dollar amount %q", s)
	}
	m := Micros(w*perDollar + f)
	if neg {
		m = -m
	}
	return m, nil
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// String formats the amount as dollars with six decimals, e.g. "0.000123".
func (m Micros) String() string {
	sign := ""
	if m < 0 {
		sign, m = "-", -m
	}
	return fmt.Sprintf("%s%d.%06d", sign, m/perDollar, m%perDollar)
}

// TokenCost prices a request. Prices are micro-dollars per million tokens, so the products are summed
// before dividing once, which keeps small requests from rounding to zero twice. Rounds half up.
func TokenCost(inputTokens int, inputPerMtok Micros, outputTokens int, outputPerMtok Micros) Micros {
	total := int64(inputTokens)*int64(inputPerMtok) + int64(outputTokens)*int64(outputPerMtok)
	return Micros((total + perDollar/2) / perDollar)
}
