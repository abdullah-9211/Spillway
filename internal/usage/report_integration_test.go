//go:build integration

package usage_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/usage"
)

// The shared development database holds other tests' rows, so every report here is narrowed to a key made for the
// test and to a range years in the past. Then the expected figures are exactly the ones seeded below.

type seeded struct {
	pool *pgxpool.Pool
	key  keys.Key
	rng  usage.Range
	r    *usage.Reader
}

func at(day int, h, m, s, ns int) time.Time {
	return time.Date(2021, 3, day, h, m, s, ns, time.UTC)
}

func seed(t *testing.T) seeded {
	t.Helper()
	pg := setup(t)
	k := pg.key
	w := usage.NewWriter(pg.pg.Pool, slog.Default(), usage.WriterOptions{})
	ms := func(n int) *int { return &n }
	add := func(created time.Time, model, provider string, in, out int, cost, saved money.Micros, lat int, cache usage.CacheStatus, outcome usage.Outcome, attempts ...usage.Attempt) {
		id, _ := uuid.NewV7()
		w.Record(usage.Row{ID: id, KeyID: k.ID, Policy: "default", Provider: provider, Model: model, InputTokens: in, OutputTokens: out, Cost: cost, Saved: saved,
			LatencyMs: lat, TTFBMs: ms(lat / 2), CacheStatus: cache, Outcome: outcome, CreatedAt: created, Attempts: attempts})
	}
	ok, miss := usage.OutcomeOK, usage.CacheMiss

	// 10 March: two alpha calls, then one that lands in the last microsecond of the day.
	add(at(10, 9, 0, 0, 0), "alpha", "p1", 100, 10, 1000, 0, 100, miss, ok)
	add(at(10, 12, 0, 0, 0), "alpha", "p1", 200, 20, 2000, 0, 300, miss, ok)
	add(at(10, 23, 59, 59, 999_999_000), "alpha", "p1", 50, 5, 500, 0, 200, miss, ok)
	// 11 March: starts exactly at midnight.
	add(at(11, 0, 0, 0, 0), "beta", "p2", 400, 40, 4000, 0, 50, miss, ok,
		usage.Attempt{Provider: "p1", Model: "alpha", Kind: "primary", ErrorKind: "server", Error: "boom"},
		usage.Attempt{Provider: "p2", Model: "beta", Kind: "fallback"})
	add(at(11, 8, 0, 0, 0), "alpha", "p1", 0, 0, 0, 1000, 2, usage.CacheHitExact, ok)
	add(at(11, 9, 0, 0, 0), "beta", "p2", 0, 0, 0, 500, 3, usage.CacheHitSemantic, ok)
	add(at(11, 10, 0, 0, 0), "beta", "p2", 0, 0, 0, 0, 900, miss, usage.OutcomeUpstreamError,
		usage.Attempt{Provider: "p2", Model: "beta", Kind: "primary", ErrorKind: "timeout", Error: "slow"})
	add(at(11, 11, 0, 0, 0), "alpha", "p1", 0, 0, 0, 0, 1, usage.CacheBypass, usage.OutcomeRateLimited)
	// 12 March: a quiet day, and 13 March (outside the range) must not leak in.
	add(at(13, 0, 0, 0, 0), "alpha", "p1", 9999, 9999, 99999, 0, 100, miss, ok)
	// Just before the range.
	add(at(9, 23, 59, 59, 999_999_000), "alpha", "p1", 7777, 7777, 77777, 0, 100, miss, ok)
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	rng, err := usage.ParseRange("2021-03-10", "2021-03-12", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return seeded{pool: pg.pg.Pool, key: k, rng: rng, r: usage.NewReader(pg.pg.Pool)}
}

func TestSummaryTotalsMatchTheSeededWorkload(t *testing.T) {
	s := seed(t)
	sum, err := s.r.Summary(context.Background(), s.rng, &s.key.ID, "model")
	if err != nil {
		t.Fatal(err)
	}
	tot := sum.Totals
	if tot.Requests != 8 || tot.InputTokens != 750 || tot.OutputTokens != 75 || tot.Cost != 7500 || tot.Saved != 1500 || tot.CacheHits != 2 || tot.Errors != 1 || tot.Rejected != 1 {
		t.Errorf("totals = %+v", tot)
	}
	if c := tot.Cache; c.Miss != 5 || c.HitExact != 1 || c.HitSemantic != 1 || c.Bypass != 1 {
		t.Errorf("cache split = %+v", c)
	}
	// Rows outside the range, including the ones a microsecond either side of it, are not counted.
	if sum.Fallbacks != 1 {
		t.Errorf("fallbacks fired = %d, want 1", sum.Fallbacks)
	}
}

// The check from the phase's verify list: report totals equal a plain SQL SUM over usage.
func TestSummaryEqualsSQLSum(t *testing.T) {
	s := seed(t)
	sum, err := s.r.Summary(context.Background(), s.rng, &s.key.ID, "day")
	if err != nil {
		t.Fatal(err)
	}
	var requests, in, out int64
	var costMicros, savedMicros int64
	err = s.pool.QueryRow(context.Background(), `
		SELECT count(*), sum(input_tokens), sum(output_tokens), (sum(cost_usd) * 1000000)::bigint, (sum(saved_usd) * 1000000)::bigint
		FROM usage WHERE api_key_id = $1 AND created_at >= $2 AND created_at < $3`, s.key.ID, s.rng.From, s.rng.To).
		Scan(&requests, &in, &out, &costMicros, &savedMicros)
	if err != nil {
		t.Fatal(err)
	}
	var days money.Micros
	var dayReqs int64
	for _, g := range sum.Groups {
		days += g.Cost
		dayReqs += g.Requests
	}
	if sum.Totals.Requests != requests || sum.Totals.InputTokens != in || sum.Totals.OutputTokens != out ||
		int64(sum.Totals.Cost) != costMicros || int64(sum.Totals.Saved) != savedMicros || int64(days) != costMicros || dayReqs != requests {
		t.Errorf("report %+v vs SQL %d %d %d %d %d; per-day cost %d", sum.Totals, requests, in, out, costMicros, savedMicros, days)
	}
}

func TestGroupByDayUsesUTCDaysAndFillsQuietOnes(t *testing.T) {
	s := seed(t)
	sum, _ := s.r.Summary(context.Background(), s.rng, &s.key.ID, "day")
	if len(sum.Groups) != 3 {
		t.Fatalf("a 3-day range has 3 groups, got %d", len(sum.Groups))
	}
	d10, d11, d12 := sum.Groups[0], sum.Groups[1], sum.Groups[2]
	if d10.Key != "2021-03-10" || d10.Requests != 3 || d10.Cost != 3500 || d10.Models["alpha"] != 3500 {
		t.Errorf("10 March (including 23:59:59.999999) = %+v", d10)
	}
	if d11.Requests != 5 || d11.Cost != 4000 || d11.Saved != 1500 || d11.Models["beta"] != 4000 {
		t.Errorf("11 March (starting at exactly midnight) = %+v", d11)
	}
	if d12.Key != "2021-03-12" || d12.Requests != 0 || d12.Cost != 0 || len(d12.Models) != 0 {
		t.Errorf("a quiet day is present with zeros: %+v", d12)
	}
}

func TestGroupByModelHasLatencyOfRealCallsOnly(t *testing.T) {
	s := seed(t)
	sum, _ := s.r.Summary(context.Background(), s.rng, &s.key.ID, "model")
	if len(sum.Groups) != 2 {
		t.Fatalf("groups = %+v", sum.Groups)
	}
	beta, alpha := sum.Groups[0], sum.Groups[1] // beta cost 4000, alpha cost 3500: by spend, highest first
	if beta.Key != "beta" || alpha.Key != "alpha" {
		t.Fatalf("order = %s, %s", beta.Key, alpha.Key)
	}
	// alpha's successful, uncached calls took 100, 300 and 200 ms; the cache hit (2 ms) and the rate-limited row (1 ms) do not count.
	if alpha.TimedRequests != 3 || math.Abs(alpha.P50Ms-200) > 0.5 || math.Abs(alpha.P95Ms-290) > 0.5 {
		t.Errorf("alpha latency: n=%d p50=%v p95=%v", alpha.TimedRequests, alpha.P50Ms, alpha.P95Ms)
	}
	if beta.TimedRequests != 1 || beta.P50Ms != 50 || beta.Errors != 1 || beta.CacheHits != 1 || beta.Requests != 3 {
		t.Errorf("beta = %+v", beta)
	}
}

func TestGroupByKey(t *testing.T) {
	s := seed(t)
	sum, err := s.r.Summary(context.Background(), s.rng, nil, "key")
	if err != nil {
		t.Fatal(err)
	}
	var mine *usage.Group
	for i := range sum.Groups {
		if sum.Groups[i].Key == s.key.ID.String() {
			mine = &sum.Groups[i]
		}
	}
	if mine == nil || mine.Requests != 8 || mine.Cost != 7500 || mine.Label != s.key.Name {
		t.Errorf("this key's row = %+v", mine)
	}
	// Narrowed to the key, only its row remains.
	only, _ := s.r.Summary(context.Background(), s.rng, &s.key.ID, "key")
	if len(only.Groups) != 1 {
		t.Errorf("groups = %d, want 1", len(only.Groups))
	}
}

func TestEmptyRange(t *testing.T) {
	s := seed(t)
	rng, _ := usage.ParseRange("2019-01-01", "2019-01-05", time.Now())
	for _, g := range []string{"day", "model", "key"} {
		sum, err := s.r.Summary(context.Background(), rng, &s.key.ID, g)
		if err != nil {
			t.Fatal(err)
		}
		if sum.Totals.Requests != 0 || sum.Totals.Cost != 0 || sum.Totals.Saved != 0 || len(sum.Providers) != 0 {
			t.Errorf("%s: totals = %+v", g, sum.Totals)
		}
		wantGroups := 0
		if g == "day" {
			wantGroups = 5 // every day still has a bar's worth of zeros
		}
		if len(sum.Groups) != wantGroups {
			t.Errorf("%s: groups = %d, want %d", g, len(sum.Groups), wantGroups)
		}
	}
	if _, err := s.r.Summary(context.Background(), s.rng, nil, "planet"); err != usage.ErrBadGroup {
		t.Errorf("bad group: %v", err)
	}
}

func TestProviderAttemptCounts(t *testing.T) {
	s := seed(t)
	sum, _ := s.r.Summary(context.Background(), s.rng, &s.key.ID, "model")
	got := map[string]usage.ProviderAttempts{}
	for _, p := range sum.Providers {
		got[p.Provider] = p
	}
	// p1 failed once (the primary before the fallback); p2 failed once (the timeout) and took one fallback.
	if got["p1"].FailedAttempts != 1 || got["p2"].FailedAttempts != 1 || got["p2"].FallbacksTo != 1 || got["p1"].FallbacksTo != 0 {
		t.Errorf("providers = %+v", got)
	}
}

func TestRequestLogKeysetPaging(t *testing.T) {
	s := seed(t)
	f := usage.RequestFilter{Range: s.rng, KeyID: &s.key.ID, Limit: 3}
	var seen []string
	var times []time.Time
	for page := 0; page < 10; page++ {
		rows, next, err := s.r.Requests(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			seen = append(seen, r.ID.String())
			times = append(times, r.CreatedAt)
		}
		if next == "" {
			break
		}
		f.Cursor = next
	}
	if len(seen) != 8 {
		t.Fatalf("paging 3 at a time should return all 8 rows once, got %d", len(seen))
	}
	uniq := map[string]bool{}
	for _, id := range seen {
		uniq[id] = true
	}
	if len(uniq) != 8 {
		t.Error("a row appeared on two pages")
	}
	for i := 1; i < len(times); i++ {
		if times[i].After(times[i-1]) {
			t.Error("rows must be newest first")
		}
	}
}

func TestRequestLogFiltersAndAttempts(t *testing.T) {
	s := seed(t)
	rows, next, err := s.r.Requests(context.Background(), usage.RequestFilter{Range: s.rng, KeyID: &s.key.ID, Model: "beta", Outcome: "ok", Limit: 50})
	if err != nil || next != "" {
		t.Fatalf("%v next=%q", err, next)
	}
	if len(rows) != 2 { // the fallback success and the semantic hit
		t.Fatalf("rows = %d", len(rows))
	}
	var withAttempts *usage.RequestRow
	for i := range rows {
		if len(rows[i].Attempts) == 2 {
			withAttempts = &rows[i]
		}
	}
	if withAttempts == nil || withAttempts.Attempts[0].ErrorKind != "server" || withAttempts.Attempts[1].Kind != "fallback" || withAttempts.Cost != 4000 || withAttempts.KeyName != s.key.Name {
		t.Errorf("attempts did not come back: %+v", rows)
	}
	if bad, _, err := s.r.Requests(context.Background(), usage.RequestFilter{Range: s.rng, Cursor: "garbage"}); err != usage.ErrBadCursor || bad != nil {
		t.Errorf("garbage cursor: %v", err)
	}
}

func TestCSVExport(t *testing.T) {
	s := seed(t)
	var buf bytes.Buffer
	if err := s.r.WriteCSV(context.Background(), &buf, s.rng, &s.key.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rows[0], ",") != "day,key,model,requests,input_tokens,output_tokens,cost_usd,saved_usd,cache_hits,errors" {
		t.Fatalf("header = %v", rows[0])
	}
	got := map[string]string{}
	var cost money.Micros
	for _, r := range rows[1:] {
		got[r[0]+"/"+r[2]] = strings.Join(r[3:], ",")
		m, _ := money.ParseUSD(r[6])
		cost += m
		if r[1] != s.key.Name {
			t.Errorf("key column = %q", r[1])
		}
	}
	if got["2021-03-10/alpha"] != "3,350,35,0.003500,0.000000,0,0" || got["2021-03-11/beta"] != "3,400,40,0.004000,0.000500,1,1" || got["2021-03-11/alpha"] != "2,0,0,0.000000,0.001000,1,0" {
		t.Errorf("rows = %v", got)
	}
	if cost != 7500 {
		t.Errorf("CSV costs add up to %d, want 7500", cost)
	}
	if len(rows) != 4 {
		t.Errorf("one row per day, key and model: got %d data rows (%v)", len(rows)-1, fmt.Sprint(got))
	}
}

func TestCSVNeutralisesFormulaNames(t *testing.T) {
	pg := setup(t)
	evil, _, err := keys.NewStore(pg.pg.Pool).Create(context.Background(), keys.CreateParams{Name: `=HYPERLINK("http://evil.example","x")`})
	if err != nil {
		t.Fatal(err)
	}
	w := usage.NewWriter(pg.pg.Pool, slog.Default(), usage.WriterOptions{})
	id, _ := uuid.NewV7()
	w.Record(usage.Row{ID: id, KeyID: evil.ID, Policy: "p", Model: "alpha", Cost: 1, LatencyMs: 1, CacheStatus: usage.CacheMiss, Outcome: usage.OutcomeOK, CreatedAt: at(10, 1, 0, 0, 0)})
	_ = w.Close(context.Background())
	rng, _ := usage.ParseRange("2021-03-10", "2021-03-10", time.Now())
	var buf bytes.Buffer
	if err := usage.NewReader(pg.pg.Pool).WriteCSV(context.Background(), &buf, rng, &evil.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"'=HYPERLINK`) {
		t.Errorf("a key named like a formula must be quoted so a spreadsheet does not run it:\n%s", buf.String())
	}
}
