package db

import (
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/abdullah-9211/spillway/internal/money"
)

// NumericFromMicros converts micro-dollars to a numeric exactly, without going through a float.
func NumericFromMicros(m money.Micros) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(int64(m)), Exp: -6, Valid: true}
}

// NullNumericFromMicros is NumericFromMicros for an optional amount; nil means SQL NULL.
func NullNumericFromMicros(m *money.Micros) pgtype.Numeric {
	if m == nil {
		return pgtype.Numeric{}
	}
	return NumericFromMicros(*m)
}

// MicrosFromNumeric converts a numeric back to micro-dollars. Anything finer than a micro-dollar is
// rounded half away from zero (the schema keeps eight decimals, the code six).
func MicrosFromNumeric(n pgtype.Numeric) (money.Micros, error) {
	if !n.Valid || n.NaN || n.InfinityModifier != pgtype.Finite || n.Int == nil {
		return 0, fmt.Errorf("numeric is not a finite value")
	}
	v := new(big.Int).Set(n.Int)
	shift := int(n.Exp) + 6 // value * 10^shift, in micro-dollars
	switch {
	case shift >= 0:
		v.Mul(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(shift)), nil))
	default:
		div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-shift)), nil)
		half := new(big.Int).Rsh(div, 1)
		if v.Sign() < 0 {
			v.Sub(v, half)
		} else {
			v.Add(v, half)
		}
		v.Quo(v, div)
	}
	if !v.IsInt64() {
		return 0, fmt.Errorf("amount out of range")
	}
	return money.Micros(v.Int64()), nil
}

// NullMicrosFromNumeric returns nil for SQL NULL.
func NullMicrosFromNumeric(n pgtype.Numeric) (*money.Micros, error) {
	if !n.Valid {
		return nil, nil
	}
	m, err := MicrosFromNumeric(n)
	if err != nil {
		return nil, err
	}
	return &m, nil
}
