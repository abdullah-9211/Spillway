package usage

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func TestParseRange(t *testing.T) {
	now := time.Date(2026, 10, 8, 15, 30, 0, 0, time.UTC)
	tests := []struct {
		name, from, to string
		wantFrom       time.Time
		wantTo         time.Time // exclusive
		wantDays       int
		wantErr        bool
	}{
		{"default is the last 14 days including today", "", "", day(2026, 9, 25), day(2026, 10, 9), 14, false},
		{"explicit, inclusive of the last day", "2026-10-01", "2026-10-08", day(2026, 10, 1), day(2026, 10, 9), 8, false},
		{"a single day", "2026-10-08", "2026-10-08", day(2026, 10, 8), day(2026, 10, 9), 1, false},
		{"only to: 14 days ending then", "", "2026-03-31", day(2026, 3, 18), day(2026, 4, 1), 14, false},
		{"only from: up to today", "2026-10-06", "", day(2026, 10, 6), day(2026, 10, 9), 3, false},
		{"across a month end", "2026-02-27", "2026-03-02", day(2026, 2, 27), day(2026, 3, 3), 4, false},
		{"leap day", "2028-02-28", "2028-03-01", day(2028, 2, 28), day(2028, 3, 2), 3, false},
		{"from after to", "2026-10-09", "2026-10-01", time.Time{}, time.Time{}, 0, true},
		{"not a date", "last week", "", time.Time{}, time.Time{}, 0, true},
		{"impossible date", "2026-02-30", "", time.Time{}, time.Time{}, 0, true},
		{"a year and a day is too long", "2025-09-01", "2026-09-02", time.Time{}, time.Time{}, 0, true},
		{"exactly 366 days is allowed", "2025-10-08", "2026-10-08", day(2025, 10, 8), day(2026, 10, 9), 366, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := ParseRange(tc.from, tc.to, now)
			if tc.wantErr {
				if !errors.Is(err, ErrBadRange) {
					t.Fatalf("err = %v, want ErrBadRange", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !r.From.Equal(tc.wantFrom) || !r.To.Equal(tc.wantTo) || r.Days() != tc.wantDays {
				t.Errorf("range = %v to %v (%d days), want %v to %v (%d)", r.From, r.To, r.Days(), tc.wantFrom, tc.wantTo, tc.wantDays)
			}
		})
	}
}

func TestParseRangeUsesUTCNotLocalTime(t *testing.T) {
	// 23:30 on the 8th in New York is already the 9th in UTC.
	ny := time.FixedZone("NY", -4*3600)
	r, err := ParseRange("", "", time.Date(2026, 10, 8, 23, 30, 0, 0, ny))
	if err != nil {
		t.Fatal(err)
	}
	if !r.To.Equal(day(2026, 10, 10)) {
		t.Errorf("range ends %v; the UTC date is the 9th, so the exclusive end is the 10th", r.To)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 10, 8, 12, 0, 0, 123456000, time.UTC)
	id := uuid.New()
	gotT, gotID, err := decodeCursor(encodeCursor(ts, id))
	if err != nil || !gotT.Equal(ts) || gotID != id {
		t.Fatalf("%v %v %v", gotT, gotID, err)
	}
	for _, bad := range []string{"", "!!!", encodeCursor(ts, id)[:5], "bm90LWEtY3Vyc29y"} {
		if _, _, err := decodeCursor(bad); !errors.Is(err, ErrBadCursor) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestSafeCellStopsFormulaInjection(t *testing.T) {
	for in, want := range map[string]string{
		"plain": "plain", "": "", "=HYPERLINK(\"http://evil\")": "'=HYPERLINK(\"http://evil\")", "+1": "'+1", "-1": "'-1", "@x": "'@x", "a=b": "a=b",
	} {
		if got := safeCell(in); got != want {
			t.Errorf("safeCell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSortedModelsOrdersByCost(t *testing.T) {
	days := []Group{
		{Series: map[string]SeriesStat{"mini": {Cost: 2}, "sonnet": {Cost: 5}}},
		{Series: map[string]SeriesStat{"mini": {Cost: 4}, "local": {}, "alpha": {}}},
	}
	got := SortedModels(days)
	want := []string{"mini", "sonnet", "alpha", "local"} // mini 2+4=6 beats sonnet 5 // ties broken by name, so the chart colours never swap
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
