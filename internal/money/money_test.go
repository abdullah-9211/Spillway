package money

import "testing"

func TestParseUSD(t *testing.T) {
	tests := []struct {
		in      string
		want    Micros
		wantErr bool
	}{
		{"0", 0, false}, {"0.00", 0, false}, {"3", 3_000_000, false}, {"0.15", 150_000, false},
		{"0.075", 75_000, false}, {"15.000001", 15_000_001, false}, {".5", 500_000, false},
		{"-1.5", -1_500_000, false}, {"0.0000001", 0, true}, {"abc", 0, true}, {"", 0, true},
		{"1.2.3", 0, true}, {"1e3", 0, true},
	}
	for _, tc := range tests {
		got, err := ParseUSD(tc.in)
		if (err != nil) != tc.wantErr || (err == nil && got != tc.want) {
			t.Errorf("ParseUSD(%q) = %d, %v; want %d, err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestString(t *testing.T) {
	tests := map[Micros]string{0: "0.000000", 123: "0.000123", 1_500_000: "1.500000", -250_000: "-0.250000"}
	for in, want := range tests {
		if got := in.String(); got != want {
			t.Errorf("Micros(%d).String() = %q, want %q", int64(in), got, want)
		}
	}
}

func TestTokenCost(t *testing.T) {
	tests := []struct {
		name string
		in   int
		inP  Micros
		out  int
		outP Micros
		want Micros
	}{
		{"zero tokens", 0, 3_000_000, 0, 15_000_000, 0},
		{"one million in at $3", 1_000_000, 3_000_000, 0, 15_000_000, 3_000_000},
		{"mixed", 1000, 3_000_000, 500, 15_000_000, 10_500},
		{"small request is not lost", 100, 150_000, 0, 600_000, 15},
		{"rounds half up", 5, 100_000, 0, 0, 1},
		{"free model", 5000, 0, 5000, 0, 0},
	}
	for _, tc := range tests {
		if got := TokenCost(tc.in, tc.inP, tc.out, tc.outP); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}
