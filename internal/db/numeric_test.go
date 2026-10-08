package db

import (
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/abdullah-9211/spillway/internal/money"
)

func TestNumericRoundTrip(t *testing.T) {
	for _, m := range []money.Micros{0, 1, 123, 1_500_000, 99_999_999_999, -42} {
		got, err := MicrosFromNumeric(NumericFromMicros(m))
		if err != nil || got != m {
			t.Errorf("round trip %d -> %d, %v", m, got, err)
		}
	}
}

func TestMicrosFromNumericScales(t *testing.T) {
	// numeric(14,8) as Postgres returns it: 0.00001234 is 1234 * 10^-8, i.e. 12.34 micro-dollars.
	tests := []struct {
		n    pgtype.Numeric
		want money.Micros
	}{
		{pgtype.Numeric{Int: big.NewInt(1234), Exp: -8, Valid: true}, 12},
		{pgtype.Numeric{Int: big.NewInt(1250), Exp: -8, Valid: true}, 13}, // 12.5 rounds up
		{pgtype.Numeric{Int: big.NewInt(5), Exp: 0, Valid: true}, 5_000_000},
		{pgtype.Numeric{Int: big.NewInt(-1250), Exp: -8, Valid: true}, -13},
	}
	for _, tc := range tests {
		if got, err := MicrosFromNumeric(tc.n); err != nil || got != tc.want {
			t.Errorf("%v: got %d, %v; want %d", tc.n, got, err, tc.want)
		}
	}
	if _, err := MicrosFromNumeric(pgtype.Numeric{}); err == nil {
		t.Error("NULL must be an error here; use NullMicrosFromNumeric")
	}
	if m, err := NullMicrosFromNumeric(pgtype.Numeric{}); m != nil || err != nil {
		t.Errorf("NULL: %v %v", m, err)
	}
}
